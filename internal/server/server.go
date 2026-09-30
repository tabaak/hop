// Package server implements hopd: the public HTTP ingress and the control
// listener that agents connect to.
package server

import (
	"errors"
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

// graceWindow is how long a dropped tunnel's name stays with its owner.
//
// The number is arbitrary and worth saying so: nothing can take the name
// during the window but the owner itself, so this is not a race to win. It
// bounds how long a laptop that is never coming back squats a name, and it is
// long enough to cover a Wi-Fi switch, a sleep, or a few rounds of a backoff
// that starts at under a second.
//
// Note it stacks with detection: the server only starts the window once yamux
// notices the session is gone, so the worst case from the drop itself is this
// plus a keepalive interval.
const graceWindow = 45 * time.Second

// retryAfter is the Retry-After sent with a held name's 503, in seconds. Short
// enough that a sender pacing itself by the header comes back inside the
// window rather than after it.
const retryAfter = "5"

type Config struct {
	// Domain is the zone tunnels live under, e.g. "hop.vokh.dev".
	Domain string
	// PublicScheme is the scheme used in URLs handed back to agents.
	PublicScheme string
	// PublicPort is appended to agent URLs when non-empty (M1 dev only).
	PublicPort string
	// Tokens authenticates agents.
	Tokens Authenticator
	// Release is hopd's release version, reported to authenticated agents so
	// they can tell their user when the server is behind them.
	Release string
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
		// A name whose agent dropped is held for a moment, and answering "not
		// found" for it would be a lie with consequences: a webhook sender
		// reads 404 as *this endpoint is gone*, stops trying, and in some
		// cases disables the endpoint outright. 503 is what every other
		// transient outage says, and the senders that retry at all retry on
		// it.
		//
		// Retry-After is sent because it is the honest header for this, not
		// because much will read it — Stripe, GitHub and Slack all pace their
		// own retries and ignore it.
		if left, held := s.reg.Leased(sub); held {
			w.Header().Set("Retry-After", retryAfter)
			s.writeStatus(w, http.StatusServiceUnavailable, fmt.Sprintf(
				"%q lost its agent and is reconnecting. The name is held for another %s.",
				sub, left.Round(time.Second)))
			return
		}
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
	h, label, ok := s.hello(conn)
	if !ok {
		conn.Close()
		return
	}
	if h.Op == proto.OpList {
		s.serveList(conn, remote, label)
		return
	}

	c, ok := s.claim(conn, h, label)
	if !ok {
		conn.Close()
		return
	}
	sub, gen := c.sub, c.gen
	// The label makes the log answer "which of my devices is this?", which is
	// the whole reason tokens are issued per device.
	who := fmt.Sprintf("%s (%s, hop %s)", remote, c.label, releaseOrUnknown(h.Release))

	// From here the claim is ours; every exit path must dispose of it.
	//
	// A session that never bound is released outright: nothing was ever
	// reachable under that name, and holding it would let an agent crash-
	// looping through the handshake lock itself out of its own namespace, one
	// fresh grace window per attempt. Only a tunnel that actually served leaves
	// a lease behind.
	hold := false
	defer func() {
		if hold && s.reg.Lease(sub, gen, graceWindow) {
			log.Printf("agent %s: holding %q for %s", who, sub, graceWindow)
			return
		}
		s.reg.Release(sub, gen)
	}()

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

	t := NewTunnel(sub, c.local, sess, s.cfg.PublicScheme)
	s.reg.Bind(sub, gen, t)
	hold = true
	log.Printf("agent %s: tunnel up for %q (%d live)", who, sub, s.reg.Count())

	t.Wait()
	// A goodbye is the difference between "this laptop is on a train" and
	// "this developer pressed Ctrl-C". Only the first is worth holding a name
	// for; the second wants its name back immediately, most often because it
	// is about to be used again by the very next command.
	hold = !t.Graceful()
	log.Printf("agent %s: tunnel down for %q", who, sub)
}

// claim is a successful handshake: the name taken, the generation guarding it,
// the label that owns it, and any predecessor the caller must close.
type claim struct {
	sub     string
	label   string
	local   string
	gen     uint64
	evicted *Tunnel
}

// hello reads the opening frame and authenticates it, returning the label that
// owns the token. Everything a connection may go on to do needs both, so this
// runs before the operations diverge — there is no path that reaches the
// registry unauthenticated.
func (s *Server) hello(conn net.Conn) (h proto.Hello, label string, ok bool) {
	if err := conn.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return h, "", false
	}
	if err := proto.Read(conn, &h); err != nil {
		log.Printf("agent %s: bad hello: %v", conn.RemoteAddr(), err)
		return h, "", false
	}
	label, ok = s.cfg.Tokens.Lookup(h.Token)
	if !ok {
		log.Printf("agent %s: rejected token", conn.RemoteAddr())
		// A HelloAck, whatever the op. Its Err field is the one thing both
		// reply types share a name for, so a refused list still decodes.
		proto.Write(conn, proto.HelloAck{Err: "invalid token", Code: proto.CodeBadToken})
		return h, "", false
	}
	// Checked after the token, so only someone entitled to use the server
	// learns which release it runs.
	if ok, agentTooOld := proto.Supported(h.Version); !ok {
		log.Printf("agent %s (%s): unsupported protocol %q (hop %s)", conn.RemoteAddr(), label, h.Version, releaseOrUnknown(h.Release))
		proto.Write(conn, proto.HelloAck{
			Err:        versionRefusal(h.Version, agentTooOld),
			Code:       proto.CodeVersion,
			ServerInfo: s.info(),
		})
		return h, "", false
	}
	return h, label, true
}

// versionRefusal is the sentence an agent prints before it exits, so it names
// the side that has to change and what to do about it. Agents older than
// CodeVersion print it verbatim, with nothing of their own around it.
func versionRefusal(agent string, agentTooOld bool) string {
	if agentTooOld {
		return fmt.Sprintf("this hop is too old for the server (protocol %s, server needs %s–%s); upgrade hop and try again",
			agent, proto.MinVersion, proto.Version)
	}
	return fmt.Sprintf("the server is too old for this hop (protocol %s, server speaks up to %s); ask whoever runs hopd to upgrade it, or use an older hop",
		agent, proto.Version)
}

// info is what an authenticated agent is told about this server.
func (s *Server) info() proto.ServerInfo {
	return proto.ServerInfo{Release: s.cfg.Release, Protocol: proto.Version}
}

func releaseOrUnknown(r string) string {
	if r == "" {
		return "older than v1.1.0"
	}
	return r
}

// serveList answers `hop ps` and hangs up. No yamux upgrade, no claim, nothing
// to release: the reply is the whole exchange.
//
// Every valid token sees every tunnel, not just its own. Labels identify the
// devices of one operator rather than separate tenants, and "which of my
// machines is serving that name" is most of the reason to ask.
func (s *Server) serveList(conn net.Conn, remote net.Addr, label string) {
	defer conn.Close()

	live := s.reg.Snapshot()
	out := make([]proto.TunnelInfo, 0, len(live))
	for _, l := range live {
		out = append(out, proto.TunnelInfo{
			Subdomain:     l.Sub,
			URL:           s.urlFor(l.Sub),
			Owner:         l.Owner,
			Local:         l.Local,
			UptimeSeconds: int64(time.Since(l.Since).Seconds()),
		})
	}
	if err := proto.Write(conn, proto.Listing{Tunnels: out, ServerInfo: s.info()}); err != nil {
		return
	}
	log.Printf("agent %s (%s): listed %d tunnel(s)", remote, label, len(out))
}

// claim reserves the requested name for label and acks it. On failure it
// reports the reason to the agent and returns ok=false.
func (s *Server) claim(conn net.Conn, h proto.Hello, label string) (c claim, ok bool) {
	// Ownership is keyed on the label, not on the secret. Rotating a token
	// therefore keeps the names its owner holds, and two devices with separate
	// tokens cannot evict each other — which is the point of issuing them
	// separately.
	sub, gen, evicted, err := s.reg.Reserve(h.Subdomain, label)
	if err != nil {
		proto.Write(conn, proto.HelloAck{Err: err.Error(), Code: refusalCode(err)})
		return claim{}, false
	}

	ack := proto.HelloAck{Subdomain: sub, URL: s.urlFor(sub), ServerInfo: s.info()}
	if err := proto.Write(conn, ack); err != nil {
		s.reg.Release(sub, gen)
		return claim{}, false
	}

	// Clear the handshake deadline; yamux keepalive governs the tunnel now.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		s.reg.Release(sub, gen)
		return claim{}, false
	}
	return claim{sub: sub, label: label, local: proto.CleanLocal(h.Local), gen: gen, evicted: evicted}, true
}

// refusalCode classifies a Reserve failure for the agent. An error with no
// code is one the agent should not retry, which is the right default: the
// exhausted-namespace case is the only unclassified one, and a server with no
// free names left is not a situation another dial in a second improves.
func refusalCode(err error) string {
	switch {
	case errors.Is(err, ErrTaken):
		return proto.CodeTaken
	case errors.Is(err, ErrBadName):
		return proto.CodeBadName
	default:
		return ""
	}
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
