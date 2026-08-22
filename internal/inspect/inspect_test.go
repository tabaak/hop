package inspect

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

const getHead = "GET /a?q=1 HTTP/1.1\r\nHost: x.hop.vokh.dev\r\nUser-Agent: curl/8\r\n\r\n"

func TestBeginParsesRequestHead(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte(getHead))
	if ex == nil {
		t.Fatal("Begin returned nil for a valid head")
	}

	recs := h.Records()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Method != "GET" || rec.Target != "/a?q=1" || rec.Proto != "HTTP/1.1" {
		t.Errorf("request line = %q %q %q", rec.Method, rec.Target, rec.Proto)
	}
	if rec.Host != "x.hop.vokh.dev" {
		t.Errorf("host = %q", rec.Host)
	}
	if got := len(rec.Request.Headers); got != 2 {
		t.Errorf("got %d headers, want 2", got)
	}
	// Published while still in flight, which is the point of publishing early.
	if rec.Done {
		t.Error("record is marked done before Close")
	}
}

func TestBeginSkipsNonHTTP(t *testing.T) {
	h := New("127.0.0.1:3000")
	if ex := h.Begin([]byte("\x16\x03\x01garbage")); ex != nil {
		t.Error("Begin recorded something that isn't a request")
	}
}

func TestCapturesBothDirections(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte("POST /submit HTTP/1.1\r\nHost: x\r\n\r\n"))
	ex.Request().Write([]byte(`{"a":1}`))
	ex.Response().Write([]byte("HTTP/1.1 201 Created\r\nContent-Type: application/json\r\n\r\n"))
	ex.Response().Write([]byte(`{"ok":true}`))
	ex.Close()

	rec := h.Records()[0]
	if rec.Request.Body != `{"a":1}` || rec.Request.Bytes != 7 {
		t.Errorf("request body = %q (%d bytes)", rec.Request.Body, rec.Request.Bytes)
	}
	if rec.Status != 201 || rec.StatusText != "Created" {
		t.Errorf("status = %d %q", rec.Status, rec.StatusText)
	}
	if rec.Response.Body != `{"ok":true}` {
		t.Errorf("response body = %q", rec.Response.Body)
	}
	if len(rec.Response.Headers) != 1 || rec.Response.Headers[0].Name != "Content-Type" {
		t.Errorf("response headers = %v", rec.Response.Headers)
	}
	if !rec.Done {
		t.Error("record not marked done after Close")
	}
	if rec.TookMS > rec.TotalMS {
		t.Errorf("time to first byte %v exceeds total %v", rec.TookMS, rec.TotalMS)
	}
}

// The head can arrive in any number of pieces, and the status must be parsed
// from the whole of it rather than from whichever fragment came first.
func TestResponseHeadAcrossWrites(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte(getHead))
	for _, chunk := range []string{"HTTP/1.", "1 404 Not F", "ound\r\nX-A: 1", "\r\n\r\nbo", "dy"} {
		ex.Response().Write([]byte(chunk))
	}
	ex.Close()

	rec := h.Records()[0]
	if rec.Status != 404 || rec.StatusText != "Not Found" {
		t.Errorf("status = %d %q", rec.Status, rec.StatusText)
	}
	if rec.Response.Body != "body" {
		t.Errorf("body = %q, want %q", rec.Response.Body, "body")
	}
}

// A stream that never sends a blank line — a bare TCP protocol, or a head
// larger than we will buffer — must still show its bytes rather than vanish.
func TestResponseWithoutHeadIsStillCaptured(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte(getHead))
	ex.Response().Write([]byte(strings.Repeat("x", maxHead+10)))
	ex.Close()

	rec := h.Records()[0]
	if rec.Response.Bytes != maxHead+10 {
		t.Errorf("captured %d bytes, want %d", rec.Response.Bytes, maxHead+10)
	}
}

func TestBodiesAreCapped(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte(getHead))
	big := strings.Repeat("a", maxBody+1000)
	ex.Request().Write([]byte(big))
	ex.Response().Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	ex.Response().Write([]byte(big))
	ex.Close()

	rec := h.Records()[0]
	if len(rec.Request.Body) != maxBody || !rec.Request.Truncated {
		t.Errorf("request kept %d bytes, truncated=%v", len(rec.Request.Body), rec.Request.Truncated)
	}
	if rec.Request.Bytes != len(big) {
		t.Errorf("request size = %d, want %d — the count must reflect the wire", rec.Request.Bytes, len(big))
	}
	if len(rec.Response.Body) != maxBody || !rec.Response.Truncated {
		t.Errorf("response kept %d bytes, truncated=%v", len(rec.Response.Body), rec.Response.Truncated)
	}
}

func TestBinaryBodyIsNotSent(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte(getHead))
	ex.Response().Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	ex.Response().Write([]byte{0xff, 0xfe, 0x00, 0x01})
	ex.Close()

	rec := h.Records()[0]
	if !rec.Response.Binary || rec.Response.Body != "" {
		t.Errorf("binary=%v body=%q, want the bytes reported but not sent", rec.Response.Binary, rec.Response.Body)
	}
	if rec.Response.Bytes != 4 {
		t.Errorf("size = %d, want 4", rec.Response.Bytes)
	}
}

func TestRingEvictsOldest(t *testing.T) {
	h := New("127.0.0.1:3000")
	for i := 0; i < MaxRecords+10; i++ {
		h.Begin([]byte(fmt.Sprintf("GET /%d HTTP/1.1\r\nHost: x\r\n\r\n", i))).Close()
	}
	recs := h.Records()
	if len(recs) != MaxRecords {
		t.Fatalf("kept %d records, want %d", len(recs), MaxRecords)
	}
	// Newest first, and the oldest ten are gone.
	if recs[0].Target != fmt.Sprintf("/%d", MaxRecords+9) {
		t.Errorf("newest = %s", recs[0].Target)
	}
	if recs[len(recs)-1].Target != "/10" {
		t.Errorf("oldest = %s, want /10", recs[len(recs)-1].Target)
	}
}

// An update to an in-flight request replaces its record instead of adding a
// second one, which is what lets the UI key on the id.
func TestUpdatesUpsert(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte(getHead))
	ex.Response().Write([]byte("HTTP/1.1 200 OK\r\n\r\n"))
	ex.Close()

	if got := len(h.Records()); got != 1 {
		t.Fatalf("got %d records for one exchange, want 1", got)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte(getHead))
	ex.Close()
	ex.Close()
	if got := len(h.Records()); got != 1 {
		t.Fatalf("got %d records, want 1", got)
	}
}

func TestFailRecordsTheReason(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte(getHead))
	ex.Fail(502, "connection refused")
	ex.Close()

	rec := h.Records()[0]
	if rec.Status != 502 || rec.Error != "connection refused" {
		t.Errorf("status = %d, error = %q", rec.Status, rec.Error)
	}
}

// A browser that has stopped reading must not hold up a request. The buffer
// fills, the sends are dropped, and everything else carries on.
func TestSlowSubscriberIsNotWaitedOn(t *testing.T) {
	h := New("127.0.0.1:3000")
	_, cancel := h.subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuffer*4; i++ {
			h.Begin([]byte(getHead)).Close()
		}
		close(done)
	}()
	<-done // Would deadlock if publish waited on the subscriber.
}

func TestSubscriberReceivesUpdates(t *testing.T) {
	h := New("127.0.0.1:3000")
	events, cancel := h.subscribe()
	defer cancel()

	h.Begin([]byte(getHead)).Close()
	// Begin publishes, Close publishes again: two events for one exchange.
	for i := 0; i < 2; i++ {
		select {
		case payload := <-events:
			if !strings.Contains(string(payload), `"target":"/a?q=1"`) {
				t.Errorf("event %d = %s", i, payload)
			}
		default:
			t.Fatalf("no event %d", i)
		}
	}
}

func TestCancelTwiceIsSafe(t *testing.T) {
	h := New("127.0.0.1:3000")
	_, cancel := h.subscribe()
	cancel()
	cancel()
}

// The tee sits on the data path in a goroutine per stream, so every field it
// touches has to hold up under -race.
func TestConcurrentCapture(t *testing.T) {
	h := New("127.0.0.1:3000")
	events, cancel := h.subscribe()
	defer cancel()
	go func() {
		for range events {
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ex := h.Begin([]byte(fmt.Sprintf("GET /%d HTTP/1.1\r\nHost: x\r\n\r\n", i)))
			ex.Request().Write([]byte("request body"))
			ex.Response().Write([]byte("HTTP/1.1 200 OK\r\nX: 1\r\n\r\n"))
			for j := 0; j < 20; j++ {
				ex.Response().Write([]byte("chunk"))
			}
			ex.Close()
		}(i)
	}
	wg.Wait()

	for _, rec := range h.Records() {
		if rec.Status != 200 || rec.Response.Bytes != 100 {
			t.Fatalf("record %d: status %d, %d body bytes", rec.ID, rec.Status, rec.Response.Bytes)
		}
	}
}

// Reading a record must not race a capture still writing to the same exchange:
// what the hub stores is a snapshot, not a window onto live buffers.
func TestRecordsAreSnapshots(t *testing.T) {
	h := New("127.0.0.1:3000")
	ex := h.Begin([]byte(getHead))
	ex.Response().Write([]byte("HTTP/1.1 200 OK\r\n\r\nfirst"))
	early := h.Records()[0]

	ex.Response().Write([]byte(" second"))
	ex.Close()

	if early.Response.Body != "first" {
		t.Errorf("the earlier snapshot changed under us: %q", early.Response.Body)
	}
	if got := h.Records()[0].Response.Body; got != "first second" {
		t.Errorf("latest = %q", got)
	}
}

func TestPublicURLRoundTrips(t *testing.T) {
	h := New("127.0.0.1:3000")
	if h.PublicURL() != "" {
		t.Error("a hub starts with no public URL")
	}
	h.SetPublicURL("https://myapp.hop.vokh.dev")
	if got := h.PublicURL(); got != "https://myapp.hop.vokh.dev" {
		t.Errorf("PublicURL = %q", got)
	}
}

func TestParseHeadersKeepsOrderAndDuplicates(t *testing.T) {
	head := []byte("GET / HTTP/1.1\r\nSet-Cookie: a=1\r\nSet-Cookie: b=2\r\nX-Empty:\r\n\r\n")
	got := parseHeaders(head)
	want := []Header{{"Set-Cookie", "a=1"}, {"Set-Cookie", "b=2"}, {"X-Empty", ""}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("header %d = %v, want %v", i, got[i], want[i])
		}
	}
}
