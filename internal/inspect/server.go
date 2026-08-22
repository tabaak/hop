package inspect

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultAddr is where the inspector listens. Loopback only, and not
// configurable by accident: everything it holds — cookies, auth headers, whole
// request bodies — is exactly what must not be reachable from the network.
const DefaultAddr = "127.0.0.1:4040"

//go:embed ui/index.html
var ui embed.FS

// Start binds addr and serves the inspector in the background. The bind error
// comes back synchronously, so a caller can report a port clash and carry on
// without the tunnel depending on it.
func (h *Hub) Start(addr string) (net.Addr, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{
		Handler:           h.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go srv.Serve(ln)
	return ln.Addr(), nil
}

// Handler is the inspector's HTTP surface.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.serveIndex)
	mux.HandleFunc("/api/records", h.serveRecords)
	mux.HandleFunc("/api/events", h.serveEvents)
	mux.HandleFunc("/api/replay", h.serveReplay)
	return localOnly(mux)
}

// localOnly rejects requests that didn't come from this machine's own browser.
// The listener is already on loopback, but a page on the public internet can
// still point a form or an <img> at 127.0.0.1:4040 and, with a hostname that
// resolves there, read the answer back. Requiring a loopback Host closes that:
// a rebound name won't match.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		switch {
		case host == "localhost", host == "127.0.0.1", host == "::1", host == "[::1]":
		default:
			http.Error(w, "hop inspector is local-only", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Hub) serveIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page, err := ui.ReadFile("ui/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(page)
}

// state is the payload the UI loads on start and re-reads after a reconnect.
type state struct {
	Local     string   `json:"local"`
	PublicURL string   `json:"publicUrl"`
	Records   []Record `json:"records"`
}

func (h *Hub) serveRecords(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(state{
		Local:     h.local,
		PublicURL: h.PublicURL(),
		Records:   h.Records(),
	})
}

// serveEvents streams records as they change. Each event carries a whole
// record, and the UI keys on the id, so an update to a request already on
// screen replaces it rather than adding a second row.
func (h *Hub) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")

	events, cancel := h.subscribe()
	defer cancel()
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// A tick keeps the connection warm through anything that times out an idle
	// stream, and gives a quiet tunnel something to prove the feed is alive.
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case payload, ok := <-events:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// serveReplay re-sends a recorded request to the local app. The replay is
// recorded like any other request, so it appears in the feed marked as one.
func (h *Hub) serveReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	newID, err := h.Replay(id)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errNoRecord) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"id": newID})
}

var errNoRecord = errors.New("no such request")

// replayTimeout bounds a replay end to end. A replayed request is sent from a
// button, not from a browser that will give up on its own, and one whose body
// was truncated may leave the local app waiting for bytes that no longer
// exist — so the deadline is what ends it.
const replayTimeout = 30 * time.Second

// Replay re-sends the recorded request id to the local app and returns the id
// of the new record. Sent as the bytes that arrived, so the local app sees the
// same request up to whatever the capture dropped — the one edit is the
// Connection header, since this is a single request on a fresh connection
// rather than one stream of many.
func (h *Hub) Replay(id int64) (int64, error) {
	rec, ok := h.find(id)
	if !ok {
		return 0, errNoRecord
	}
	if rec.Request.Truncated {
		// The body on record is short, so its Content-Length is a lie. Sending
		// it anyway would hang the local app until the deadline; saying so is
		// more useful than a mystery timeout.
		return 0, fmt.Errorf("body was too large to record in full (%d bytes), so it cannot be replayed", rec.Request.Bytes)
	}

	conn, err := net.DialTimeout("tcp", h.local, 5*time.Second)
	if err != nil {
		return 0, fmt.Errorf("dial %s: %w", h.local, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(replayTimeout))

	// Sent with Connection: close, which is what ends the response: the server
	// hangs up after it, and the read below returns. Recorded from the head we
	// actually send, so the inspector shows the request that was made.
	head := withConnectionClose(rec.rawHead)
	ex := h.begin(head, true)
	if ex == nil {
		return 0, errors.New("request cannot be replayed")
	}
	defer ex.Close()

	if _, err := conn.Write(head); err != nil {
		ex.Fail(http.StatusBadGateway, err.Error())
		return ex.ID(), nil
	}
	if len(rec.rawBody) > 0 {
		if _, err := conn.Write(rec.rawBody); err != nil {
			ex.Fail(http.StatusBadGateway, err.Error())
			return ex.ID(), nil
		}
		ex.Request().Write(rec.rawBody)
	}
	// Deliberately no CloseWrite here. Half-closing would be the tidy way to
	// say "that's the whole request", and Go's net/http copes, but Node's HTTP
	// server treats the FIN as the client giving up and closes without
	// answering at all — a replay that silently returned nothing. Connection:
	// close carries the same meaning at a level every server agrees on.

	if _, err := io.Copy(ex.Response(), conn); err != nil && !isExpectedEnd(err) {
		ex.Fail(http.StatusBadGateway, err.Error())
	}
	return ex.ID(), nil
}

// withConnectionClose returns head with a single Connection: close, replacing
// whatever connection headers it carried. Everything else is left byte for
// byte, so the request the app sees is the one that was recorded.
func withConnectionClose(head []byte) []byte {
	out := make([]byte, 0, len(head)+20)
	rest := head

	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			rest = nil
		}
		if hasPrefixFold(line, "connection:") || hasPrefixFold(line, "keep-alive:") ||
			hasPrefixFold(line, "proxy-connection:") {
			continue
		}
		// The blank line terminating the head: the new header goes in front of
		// it, and anything after it is body that isn't ours to touch.
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			out = append(out, "Connection: close\r\n"...)
		}
		out = append(out, line...)
	}
	return out
}

func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && strings.EqualFold(string(b[:len(prefix)]), prefix)
}

// isExpectedEnd reports whether err is just the connection ending, which for a
// streaming response is how a replay normally finishes.
func isExpectedEnd(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "connection reset by peer")
}
