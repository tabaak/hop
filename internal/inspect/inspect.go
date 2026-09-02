// Package inspect records the requests passing through the agent and serves a
// local page to look at them.
//
// It sits on the data path, so the rules here are strict: nothing it does may
// block a request or hold a byte back. Capture is a tee into a bounded buffer
// under a short-lived mutex, and the fan-out to browsers drops rather than
// waits. An inspector that stalls a tunnel would be worse than no inspector.
package inspect

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// MaxRecords is how many exchanges are kept. The buffer is a debugging aid,
	// not a log: fifty is a few minutes of clicking around, and the whole thing
	// stays in memory without a size to tune.
	MaxRecords = 50
	// maxBody bounds what is kept of each body, per direction. Past this the
	// bytes still flow — only the copy stops, and the record says so.
	maxBody = 64 << 10
	// maxHead bounds the response head we are willing to buffer while looking
	// for the blank line. A head longer than this isn't one.
	maxHead = 64 << 10
	// subBuffer is how many events a browser may fall behind before it starts
	// missing them. Dropping is deliberate: the alternative is blocking a
	// request on a slow SSE reader.
	subBuffer = 64
)

// Header is one header line, kept in the order it arrived. A map would lose
// both the order and any repeated name, and repeated Set-Cookie is exactly the
// kind of thing you open an inspector to see.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Message is one side of an exchange.
type Message struct {
	Headers []Header `json:"headers"`
	// Body is the captured body as text. Empty when Binary is set.
	Body string `json:"body"`
	// Bytes is the size seen on the wire, which is larger than Body when
	// Truncated is set.
	Bytes     int  `json:"bytes"`
	Truncated bool `json:"truncated"`
	// Binary marks a body that isn't valid UTF-8, and so isn't shown.
	Binary bool `json:"binary"`
}

// Record is one exchange, as the UI sees it. Values handed to the hub are
// snapshots: once stored, a Record is never written to again, which is what
// lets the browser-facing side read them without touching the request's locks.
type Record struct {
	ID       int64  `json:"id"`
	Method   string `json:"method"`
	Target   string `json:"target"`
	Proto    string `json:"proto"`
	Host     string `json:"host"`
	Replayed bool   `json:"replayed"`

	Started    time.Time `json:"started"`
	Status     int       `json:"status"`
	StatusText string    `json:"statusText"`
	// TookMS is time to first byte of the response, matching the terminal log.
	TookMS float64 `json:"tookMs"`
	// TotalMS is set once the exchange ends, which for a WebSocket or an SSE
	// stream is much later than the first byte.
	TotalMS float64 `json:"totalMs"`
	Done    bool    `json:"done"`
	Error   string  `json:"error,omitempty"`

	Request  Message `json:"request"`
	Response Message `json:"response"`

	// raw is the request exactly as it arrived, kept for replay. Unexported, so
	// it never reaches the browser: the head is already there field by field.
	rawHead []byte
	rawBody []byte
}

// Hub holds the ring buffer and the connected browsers.
type Hub struct {
	// local is the address replayed requests are sent to.
	local string

	mu        sync.Mutex
	ring      []Record // oldest first
	nextID    int64
	publicURL string
	subs      map[chan []byte]struct{}
}

// New returns a hub that replays to local, e.g. "127.0.0.1:3000".
func New(local string) *Hub {
	return &Hub{
		local: local,
		ring:  make([]Record, 0, MaxRecords),
		subs:  make(map[chan []byte]struct{}),
	}
}

// SetPublicURL records the URL the tunnel is reachable at, so the UI can build
// a cURL command that goes through hop rather than straight to the local port.
// Called again after a reconnect, since the name can change.
func (h *Hub) SetPublicURL(url string) {
	h.mu.Lock()
	h.publicURL = url
	h.mu.Unlock()
}

func (h *Hub) PublicURL() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.publicURL
}

// Records returns the buffer, newest first.
func (h *Hub) Records() []Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Record, 0, len(h.ring))
	for i := len(h.ring) - 1; i >= 0; i-- {
		out = append(out, h.ring[i])
	}
	return out
}

// store upserts a snapshot, evicting the oldest record once the ring is full.
func (h *Hub) store(rec Record) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.ring {
		if h.ring[i].ID == rec.ID {
			h.ring[i] = rec
			return
		}
	}
	if len(h.ring) == MaxRecords {
		copy(h.ring, h.ring[1:])
		h.ring = h.ring[:MaxRecords-1]
	}
	h.ring = append(h.ring, rec)
}

// find returns a copy of the stored record with the given id.
func (h *Hub) find(id int64) (Record, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.ring {
		if r.ID == id {
			return r, true
		}
	}
	return Record{}, false
}

// subscribe returns a channel of SSE payloads and the function that ends it.
func (h *Hub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, subBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.mu.Unlock()
	}
}

// publish stores the snapshot and pushes it to every connected browser. The
// send is non-blocking on purpose — see the note on subBuffer.
//
// With no browsers attached it stops after storing. Capture runs whether or
// not anyone is watching, and encoding every record to feed nobody would put
// a per-request cost on the data path. A subscriber arriving in the same
// instant misses one event, which it makes up from /api/records.
func (h *Hub) publish(rec Record) {
	h.store(rec)

	h.mu.Lock()
	watched := len(h.subs) > 0
	h.mu.Unlock()
	if !watched {
		return
	}

	payload, err := json.Marshal(rec)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- payload:
		default:
		}
	}
}

// Begin starts recording an exchange whose request head is head. It returns
// nil only if head isn't HTTP-shaped enough to be worth showing.
//
// Safe to call from many goroutines: the agent runs one per stream.
func (h *Hub) Begin(head []byte) *Exchange {
	return h.begin(head, false)
}

func (h *Hub) begin(head []byte, replayed bool) *Exchange {
	method, target, proto := requestLine(head)
	if method == "" {
		return nil
	}

	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.mu.Unlock()

	start := time.Now()
	ex := &Exchange{
		hub:   h,
		start: start,
		rec: Record{
			ID:       id,
			Method:   method,
			Target:   target,
			Proto:    proto,
			Replayed: replayed,
			Started:  start,
			Request:  Message{Headers: parseHeaders(head)},
		},
	}
	ex.rec.Host = lookup(ex.rec.Request.Headers, "host")
	ex.rec.rawHead = append([]byte(nil), head...)
	// Published straight away so the request shows up while it is still in
	// flight; every later publish upserts the same id.
	ex.publish()
	return ex
}

// Exchange captures one request and its response. Its writers sit in a tee on
// the data path, so they only ever append to a bounded buffer.
type Exchange struct {
	hub   *Hub
	start time.Time

	mu       sync.Mutex
	rec      Record
	reqBody  []byte
	respHead []byte
	respBody []byte
	// headDone is set once the response head has been parsed, after which
	// everything arriving is body.
	headDone bool
	closed   bool
}

// Request returns the writer fed a copy of the request body.
func (e *Exchange) Request() io.Writer { return &BodyWriter{ex: e} }

// Response returns the writer fed a copy of the response, head included.
func (e *Exchange) Response() io.Writer { return &BodyWriter{ex: e, response: true} }

// Fail records that the exchange never reached the local app.
func (e *Exchange) Fail(status int, msg string) {
	e.mu.Lock()
	e.rec.Status = status
	e.rec.StatusText = http.StatusText(status)
	e.rec.Error = msg
	e.rec.TookMS = msSince(e.start)
	e.mu.Unlock()
	e.publish()
}

// Close ends the exchange. Calling it twice is harmless, so callers can defer
// it next to the connection they are closing.
func (e *Exchange) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	e.rec.Done = true
	e.rec.TotalMS = msSince(e.start)
	if !e.headDone && len(e.respHead) > 0 {
		// A response that ended without a blank line: show what there was
		// rather than dropping it.
		e.parseResponseLocked(e.respHead, false)
	}
	e.mu.Unlock()
	e.publish()
}

// ID is the record's id, which the UI uses to replay it.
func (e *Exchange) ID() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rec.ID
}

func (e *Exchange) publish() {
	e.mu.Lock()
	rec := e.snapshotLocked()
	e.mu.Unlock()
	e.hub.publish(rec)
}

// snapshotLocked builds the immutable copy handed to the hub. The buffers are
// copied out, so nothing the browser side reads is written to again.
func (e *Exchange) snapshotLocked() Record {
	rec := e.rec
	rec.Request.Headers = append([]Header(nil), e.rec.Request.Headers...)
	rec.Response.Headers = append([]Header(nil), e.rec.Response.Headers...)
	rec.Request.Body, rec.Request.Binary = bodyText(e.reqBody)
	rec.Response.Body, rec.Response.Binary = bodyText(e.respBody)
	rec.rawBody = append([]byte(nil), e.reqBody...)
	return rec
}

// BodyWriter tees one direction of an exchange into its buffer. Writes never
// block on anything but the exchange's own mutex, and never fail: a full
// buffer is recorded as a truncation, not an error, because returning one
// would tear down the request being inspected.
type BodyWriter struct {
	ex       *Exchange
	response bool
}

func (w *BodyWriter) Write(p []byte) (int, error) {
	if w.ex == nil {
		return len(p), nil
	}
	if w.response {
		w.ex.writeResponse(p)
	} else {
		w.ex.writeRequest(p)
	}
	return len(p), nil
}

func (e *Exchange) writeRequest(p []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rec.Request.Bytes += len(p)
	e.reqBody, e.rec.Request.Truncated = appendCapped(e.reqBody, p, e.rec.Request.Truncated)
}

// writeResponse accumulates until the head is complete, publishes at that
// point — which is the moment the status is known, and so the moment the
// duration means time-to-first-byte — and appends the rest as body.
func (e *Exchange) writeResponse(p []byte) {
	e.mu.Lock()
	complete := false

	if !e.headDone {
		e.respHead = append(e.respHead, p...)
		if i := bytes.Index(e.respHead, []byte("\r\n\r\n")); i >= 0 {
			head, rest := e.respHead[:i+4], e.respHead[i+4:]
			e.parseResponseLocked(head, true)
			e.respHead = nil
			p, complete = rest, true
		} else if len(e.respHead) > maxHead {
			// Not a head. Treat everything as body so the bytes are at least
			// visible, rather than buffering the whole response looking for a
			// terminator that isn't coming.
			e.headDone = true
			p, e.respHead = e.respHead, nil
			complete = true
		} else {
			e.mu.Unlock()
			return
		}
	}

	e.rec.Response.Bytes += len(p)
	e.respBody, e.rec.Response.Truncated = appendCapped(e.respBody, p, e.rec.Response.Truncated)
	rec := e.snapshotLocked()
	e.mu.Unlock()

	// Published only when the head lands. Publishing per chunk would put a
	// JSON encode of the buffered body on the data path for every packet of a
	// streaming response.
	if complete {
		e.hub.publish(rec)
	}
}

// parseResponseLocked fills in the status and headers from a response head.
func (e *Exchange) parseResponseLocked(head []byte, full bool) {
	e.headDone = true
	e.rec.TookMS = msSince(e.start)
	line := head
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(string(line))
	if len(fields) >= 2 {
		if code, err := strconv.Atoi(fields[1]); err == nil && code >= 100 && code <= 599 {
			e.rec.Status = code
			e.rec.StatusText = http.StatusText(code)
			if len(fields) > 2 {
				e.rec.StatusText = strings.Join(fields[2:], " ")
			}
		}
	}
	if full {
		e.rec.Response.Headers = parseHeaders(head)
	}
}

// appendCapped copies what fits of p into buf, reporting whether anything was
// dropped, here or earlier.
func appendCapped(buf, p []byte, truncated bool) ([]byte, bool) {
	room := maxBody - len(buf)
	if room <= 0 {
		return buf, true
	}
	if len(p) > room {
		return append(buf, p[:room]...), true
	}
	return append(buf, p...), truncated
}

// requestLine pulls the three fields out of "GET /path HTTP/1.1".
func requestLine(head []byte) (method, target, proto string) {
	line := head
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := bytes.Fields(line)
	if len(fields) < 2 {
		return "", "", ""
	}
	if len(fields) > 2 {
		proto = string(fields[2])
	}
	return string(fields[0]), string(fields[1]), proto
}

// parseHeaders reads the header block that follows the first line, keeping
// order and duplicates.
func parseHeaders(head []byte) []Header {
	rest := head
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	}
	var out []Header
	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			break
		}
		i := bytes.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		out = append(out, Header{
			Name:  string(bytes.TrimSpace(line[:i])),
			Value: string(bytes.TrimSpace(line[i+1:])),
		})
	}
	return out
}

func lookup(headers []Header, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// bodyText renders a body for the browser. Anything that isn't valid UTF-8 is
// reported as binary rather than sent: the UI has nothing useful to do with a
// JPEG, and encoding one would bloat every event carrying it.
func bodyText(b []byte) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	if !utf8.Valid(b) {
		return "", true
	}
	return string(b), false
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}
