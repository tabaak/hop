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
	// Tokens is the set of accepted agent tokens.
	Tokens map[string]bool
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
	sub, gen, evicted, ok := s.handshake(conn)
	if !ok {
		conn.Close()
		return
	}
	// From here the claim is ours; every exit path must release it.
	defer s.reg.Release(sub, gen)

	if evicted != nil {
		log.Printf("agent %s: taking over %q from a previous session", remote, sub)
		evicted.Close()
	}

	// The server opens streams toward the agent, so it takes the yamux client
	// role even though the agent dialed the TCP connection. The roles only
	// decide odd/even stream ID assignment; they must simply differ.
	sess, err := yamux.Client(conn, tunnel.Config())
	if err != nil {
		log.Printf("agent %s: yamux upgrade failed: %v", remote, err)
		conn.Close()
		return
	}

	t := NewTunnel(sub, sess, s.cfg.PublicScheme)
	s.reg.Bind(sub, gen, t)
	log.Printf("agent %s: tunnel up for %q (%d live)", remote, sub, s.reg.Count())

	t.Wait()
	log.Printf("agent %s: tunnel down for %q", remote, sub)
}

// handshake reads Hello, authenticates, claims a name and acks. On any failure
// it reports the reason to the agent and returns ok=false. A non-nil evicted
// tunnel is the caller's to close.
func (s *Server) handshake(conn net.Conn) (sub string, gen uint64, evicted *Tunnel, ok bool) {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return "", 0, nil, false
	}

	var h proto.Hello
	if err := proto.Read(conn, &h); err != nil {
		log.Printf("agent %s: bad hello: %v", conn.RemoteAddr(), err)
		return "", 0, nil, false
	}
	if !s.cfg.Tokens[h.Token] {
		log.Printf("agent %s: rejected token", conn.RemoteAddr())
		proto.Write(conn, proto.HelloAck{Err: "invalid token"})
		return "", 0, nil, false
	}

	sub, gen, evicted, err := s.reg.Reserve(h.Subdomain, h.Token)
	if err != nil {
		proto.Write(conn, proto.HelloAck{Err: err.Error()})
		return "", 0, nil, false
	}

	ack := proto.HelloAck{Subdomain: sub, URL: s.urlFor(sub)}
	if err := proto.Write(conn, ack); err != nil {
		s.reg.Release(sub, gen)
		return "", 0, nil, false
	}

	// Clear the handshake deadline; yamux keepalive governs the tunnel now.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.reg.Release(sub, gen)
		return "", 0, nil, false
	}
	return sub, gen, evicted, true
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
