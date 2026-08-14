// Package server implements hopd: the public HTTP ingress and the control
// listener that agents connect to.
package server

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/yamux"

	"hop.vokh.dev/internal/proto"
	"hop.vokh.dev/internal/tunnel"
)

// handshakeTimeout bounds how long a freshly accepted control connection may
// take to identify itself, so idle or hostile dials can't pile up.
const handshakeTimeout = 10 * time.Second

type Config struct {
	// Domain is the zone tunnels live under, e.g. "hop.vokh.dev".
	Domain string
	// PublicScheme is the scheme used in URLs handed back to agents.
	PublicScheme string
	// PublicPort is appended to agent URLs when non-empty (M1 dev only).
	PublicPort string
	// Tokens authenticates agents.
	Tokens Authenticator
}

// Authenticator resolves an agent's token to the label that owns it. The label
// is the identity everything downstream works in: it names the device in the
// log, and it decides who may take over a subdomain. *tokens.Store implements
// it.
type Authenticator interface {
	Lookup(secret string) (label string, ok bool)
}

type Server struct {
	cfg Config
	reg *Registry
}

func New(cfg Config) *Server {
	return &Server{cfg: cfg, reg: NewRegistry()}
}

// ServeHTTP is the public ingress: map Host to a tunnel, or explain why not.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub := s.subdomainOf(r.Host)
	if sub == "" {
		s.writeStatus(w, http.StatusNotFound,
			fmt.Sprintf("No tunnel here. Public URLs look like https://myapp.%s", s.cfg.Domain))
		return
	}
	t, ok := s.reg.Lookup(sub)
	if !ok {
		s.writeStatus(w, http.StatusNotFound,
			fmt.Sprintf("No agent is serving %q right now.", sub))
		return
	}

	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	t.proxy.ServeHTTP(rec, r)
	log.Printf("%s %s %s %d %s", sub, r.Method, r.URL.Path, rec.status, time.Since(start).Round(time.Millisecond))
}

// subdomainOf extracts the tunnel name from a Host header. It returns "" for
// the bare domain, an unrelated host, or a multi-level name.
func (s *Server) subdomainOf(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	suffix := "." + s.cfg.Domain
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	sub := strings.TrimSuffix(host, suffix)
	if sub == "" || strings.Contains(sub, ".") {
		return ""
	}
	return sub
}

// RevokeExcept disconnects every agent whose owning label is no longer
// accepted, returning the subdomains it freed. Call it after the token set
// changes.
func (s *Server) RevokeExcept(keep map[string]bool) []string {
	return s.reg.CloseRevoked(keep)
}

// ServeControl runs the agent-facing listener until it errors.
func (s *Server) ServeControl(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.handleAgent(conn)
	}
}

func (s *Server) handleAgent(conn net.Conn) {
	remote := conn.RemoteAddr()
	c, ok := s.handshake(conn)
	if !ok {
		conn.Close()
		return
	}
	sub, gen := c.sub, c.gen
	// The label makes the log answer "which of my devices is this?", which is
	// the whole reason tokens are issued per device.
	who := fmt.Sprintf("%s (%s)", remote, c.label)

	// From here the claim is ours; every exit path must release it.
	defer s.reg.Release(sub, gen)

	if c.evicted != nil {
		log.Printf("agent %s: taking over %q from a previous session", who, sub)
		c.evicted.Close()
	}

	// The server opens streams toward the agent, so it takes the yamux client
	// role even though the agent dialed the TCP connection. The roles only
	// decide odd/even stream ID assignment; they must simply differ.
	sess, err := yamux.Client(conn, tunnel.Config())
	if err != nil {
		log.Printf("agent %s: yamux upgrade failed: %v", who, err)
		conn.Close()
		return
	}

	t := NewTunnel(sub, sess, s.cfg.PublicScheme)
	s.reg.Bind(sub, gen, t)
	log.Printf("agent %s: tunnel up for %q (%d live)", who, sub, s.reg.Count())

	t.Wait()
	log.Printf("agent %s: tunnel down for %q", who, sub)
}

// claim is a successful handshake: the name taken, the generation guarding it,
// the label that owns it, and any predecessor the caller must close.
type claim struct {
	sub     string
	label   string
	gen     uint64
	evicted *Tunnel
}

// handshake reads Hello, authenticates, claims a name and acks. On any failure
// it reports the reason to the agent and returns ok=false.
func (s *Server) handshake(conn net.Conn) (c claim, ok bool) {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return claim{}, false
	}

	var h proto.Hello
	if err := proto.Read(conn, &h); err != nil {
		log.Printf("agent %s: bad hello: %v", conn.RemoteAddr(), err)
		return claim{}, false
	}
	label, ok := s.cfg.Tokens.Lookup(h.Token)
	if !ok {
		log.Printf("agent %s: rejected token", conn.RemoteAddr())
		proto.Write(conn, proto.HelloAck{Err: "invalid token"})
		return claim{}, false
	}

	// Ownership is keyed on the label, not on the secret. Rotating a token
	// therefore keeps the names its owner holds, and two devices with separate
	// tokens cannot evict each other — which is the point of issuing them
	// separately.
	sub, gen, evicted, err := s.reg.Reserve(h.Subdomain, label)
	if err != nil {
		proto.Write(conn, proto.HelloAck{Err: err.Error()})
		return claim{}, false
	}

	ack := proto.HelloAck{Subdomain: sub, URL: s.urlFor(sub)}
	if err := proto.Write(conn, ack); err != nil {
		s.reg.Release(sub, gen)
		return claim{}, false
	}

	// Clear the handshake deadline; yamux keepalive governs the tunnel now.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.reg.Release(sub, gen)
		return claim{}, false
	}
	return claim{sub: sub, label: label, gen: gen, evicted: evicted}, true
}

func (s *Server) urlFor(sub string) string {
	host := sub + "." + s.cfg.Domain
	if s.cfg.PublicPort != "" {
		host = net.JoinHostPort(host, s.cfg.PublicPort)
	}
	return s.cfg.PublicScheme + "://" + host
}

func (s *Server) writeStatus(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintf(w, "hop: %s\n", msg)
}

// statusRecorder captures the response status for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.written {
		r.status, r.written = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.written = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real ResponseWriter. Without
// it, wrapping would hide Flush and Hijack, breaking streaming responses and
// WebSocket upgrades through the proxy.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
