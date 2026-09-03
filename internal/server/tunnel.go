package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"hop.vokh.dev/internal/proto"
)

// byeTimeout bounds how long the server will wait for a goodbye frame once an
// agent has opened a stream to send one.
const byeTimeout = 5 * time.Second

// Tunnel is one connected agent, plus the reverse proxy that reaches it.
type Tunnel struct {
	Sub string
	// Local is the address the agent says it forwards to. Reported by the
	// agent, shown in listings, and never used to route anything.
	Local string
	sess  *yamux.Session
	proxy *httputil.ReverseProxy
	// graceful records that the agent said goodbye, so the name is released
	// now instead of being held for the grace window.
	graceful atomic.Bool
}

// NewTunnel wires a reverse proxy whose transport dials yamux streams instead
// of TCP sockets. That one substitution is the whole trick: every inbound
// request opens a fresh stream over the agent's long-lived connection, and
// ReverseProxy handles hop-by-hop headers, streaming bodies and Upgrade
// (WebSocket) requests for us.
//
// publicScheme is what the browser used, which is not always what reached us:
// behind a TLS-terminating reverse proxy the inbound request is plaintext even
// though the client spoke HTTPS.
func NewTunnel(sub, local string, sess *yamux.Session, publicScheme string) *Tunnel {
	t := &Tunnel{Sub: sub, Local: local, sess: sess}
	go t.listen()
	t.proxy = &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			// The dialer ignores this address; it exists only so the request
			// line and Host header are well-formed. Keeping the inbound Host
			// means the local app sees myapp.hop.vokh.dev, which is usually
			// what you want (see --host-header in M4 for when it isn't).
			r.Out.URL.Host = r.In.Host
			r.Out.Host = r.In.Host
			// Drops any inbound X-Forwarded-* rather than appending to them,
			// so a client can't forge them.
			r.SetXForwarded()
			// ...then correct the scheme, which SetXForwarded takes from the
			// connection that reached us rather than the one the client made.
			r.Out.Header.Set("X-Forwarded-Proto", publicScheme)
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return sess.OpenStream()
			},
			// One stream per request. Pooling would work, but this keeps the
			// lifetime obvious and matches what the agent expects: accept a
			// stream, dial localhost, copy, close.
			DisableKeepAlives: true,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "hop: tunnel error: "+err.Error(), http.StatusBadGateway)
		},
	}
	return t
}

// listen accepts the streams the agent opens. The server opens a stream per
// inbound request and the agent has never opened any, so the only thing that
// arrives here is a goodbye — but it is written as a loop over anything,
// because a stream nobody reads is a stream that blocks the agent writing it.
func (t *Tunnel) listen() {
	for {
		st, err := t.sess.Accept()
		if err != nil {
			return
		}
		go t.readBye(st)
	}
}

// readBye reads one frame from a stream the agent opened, and closes the
// session if it was a goodbye.
//
// The order matters: graceful is set *before* the close, and the close is what
// wakes Wait. A handleAgent that checked the flag after being woken by the
// agent's own close would be racing this goroutine, and would hold the name of
// an agent that had politely asked it not to.
func (t *Tunnel) readBye(st net.Conn) {
	defer st.Close()

	if err := st.SetReadDeadline(time.Now().Add(byeTimeout)); err != nil {
		return
	}
	var bye proto.Bye
	if err := proto.Read(st, &bye); err != nil {
		return
	}
	t.graceful.Store(true)
	t.sess.Close()
}

// Graceful reports whether the agent said goodbye before going away. Only
// meaningful once Wait has returned.
func (t *Tunnel) Graceful() bool { return t.graceful.Load() }

// Wait blocks until the agent's session goes away.
func (t *Tunnel) Wait() { <-t.sess.CloseChan() }

func (t *Tunnel) Close() error { return t.sess.Close() }
