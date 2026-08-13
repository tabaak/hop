package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"

	"github.com/hashicorp/yamux"
)

// Tunnel is one connected agent, plus the reverse proxy that reaches it.
type Tunnel struct {
	Sub   string
	sess  *yamux.Session
	proxy *httputil.ReverseProxy
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
func NewTunnel(sub string, sess *yamux.Session, publicScheme string) *Tunnel {
	t := &Tunnel{Sub: sub, sess: sess}
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

// Wait blocks until the agent's session goes away.
func (t *Tunnel) Wait() { <-t.sess.CloseChan() }

func (t *Tunnel) Close() error { return t.sess.Close() }
