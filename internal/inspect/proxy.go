package inspect

import (
	"context"
	"net"
	"net/http"
	"net/http/httputil"
)

// Proxy serves the inspector of an agent listening on the unix socket at path.
// It is the whole of `hop inspect`: bind 127.0.0.1:4040, hand every request to
// the agent over that socket, and copy the answers back unchanged.
//
// The inbound Host is preserved rather than rewritten. The agent's handler
// refuses requests whose Host is not a loopback name — the guard that stops a
// public page reading the inspector through a rebound DNS name — and the
// browser addressing this proxy at 127.0.0.1 passes it naturally.
//
// FlushInterval is negative so responses are flushed as they arrive: the feed
// is server-sent events, and buffering it would hold every request back until
// the stream ended, which for a quiet tunnel is never.
func Proxy(path string) http.Handler {
	return &httputil.ReverseProxy{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				// The address arguments are ignored: every request goes to the
				// same socket, whatever the placeholder URL says.
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "hop-inspect.invalid"
			pr.Out.Host = pr.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// The agent died or is restarting. The page polls again after its
			// reconnect, so a plain 502 is all that is needed here; logging
			// would only spam the terminal the user is reading.
			http.Error(w, "the tunnel's inspector is not answering", http.StatusBadGateway)
		},
	}
}
