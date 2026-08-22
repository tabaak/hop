package inspect

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// localApp starts a throwaway HTTP server standing in for the port being
// tunnelled, and returns its host:port.
func localApp(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// get issues a request to the inspector, with a loopback Host so it passes the
// local-only check.
func get(t *testing.T, h http.Handler, method, target string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:4040"+target, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result()
}

func TestServesTheUI(t *testing.T) {
	h := New("127.0.0.1:3000").Handler()
	res := get(t, h, "GET", "/")
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q", ct)
	}
	body, _ := io.ReadAll(res.Body)
	if !strings.Contains(string(body), "hop inspector") {
		t.Error("the embedded page does not look like the inspector")
	}
}

func TestUnknownPathIs404(t *testing.T) {
	h := New("127.0.0.1:3000").Handler()
	if res := get(t, h, "GET", "/nope"); res.StatusCode != 404 {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

// Everything the inspector holds is private, so a page served from anywhere
// else must not be able to read it by pointing a rebound name at loopback.
func TestNonLocalHostIsRefused(t *testing.T) {
	h := New("127.0.0.1:3000").Handler()
	req := httptest.NewRequest("GET", "http://evil.example.com/api/records", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

func TestRecordsEndpoint(t *testing.T) {
	hub := New("127.0.0.1:3000")
	hub.SetPublicURL("https://myapp.hop.vokh.dev")
	hub.Begin([]byte(getHead)).Close()

	res := get(t, hub.Handler(), "GET", "/api/records")
	var got state
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Local != "127.0.0.1:3000" || got.PublicURL != "https://myapp.hop.vokh.dev" {
		t.Errorf("local = %q, public = %q", got.Local, got.PublicURL)
	}
	if len(got.Records) != 1 || got.Records[0].Target != "/a?q=1" {
		t.Fatalf("records = %+v", got.Records)
	}
}

func TestEventsStream(t *testing.T) {
	hub := New("127.0.0.1:3000")
	addr, err := hub.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Get("http://" + addr.String() + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

	// Recorded only once the stream is subscribed, so the event is a live one
	// rather than part of a backlog.
	go func() {
		time.Sleep(20 * time.Millisecond)
		ex := hub.Begin([]byte(getHead))
		ex.Response().Write([]byte("HTTP/1.1 204 No Content\r\n\r\n"))
		ex.Close()
	}()

	r := bufio.NewReader(res.Body)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Target != "/a?q=1" {
			t.Fatalf("event = %+v", rec)
		}
		return
	}
	t.Fatal("no event arrived")
}

func TestReplaySendsTheRequestAgain(t *testing.T) {
	seen := make(chan *http.Request, 4)
	bodies := make(chan string, 4)
	local := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- r
		bodies <- string(body)
		w.Header().Set("X-From", "local")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "created")
	}))

	hub := New(local)
	// A request as the agent would have captured it: head as it arrived, body
	// fed through the tee.
	head := "POST /things HTTP/1.1\r\nHost: " + local + "\r\nContent-Length: 7\r\nX-Trace: abc\r\n\r\n"
	ex := hub.Begin([]byte(head))
	ex.Request().Write([]byte(`{"a":1}`))
	ex.Close()
	original := ex.ID()

	newID, err := hub.Replay(original)
	if err != nil {
		t.Fatal(err)
	}
	if newID == original {
		t.Error("the replay reused the original record's id")
	}

	req := <-seen
	if req.Method != "POST" || req.URL.Path != "/things" {
		t.Errorf("local app saw %s %s", req.Method, req.URL.Path)
	}
	if got := req.Header.Get("X-Trace"); got != "abc" {
		t.Errorf("X-Trace = %q, want the header from the original", got)
	}
	if got := <-bodies; got != `{"a":1}` {
		t.Errorf("body = %q", got)
	}

	rec, ok := hub.find(newID)
	if !ok {
		t.Fatal("the replay was not recorded")
	}
	if !rec.Replayed {
		t.Error("the replay is not marked as one")
	}
	if rec.Status != 201 || rec.Response.Body != "created" {
		t.Errorf("replay recorded status %d body %q", rec.Status, rec.Response.Body)
	}
	if rec.Request.Body != `{"a":1}` {
		t.Errorf("replay recorded request body %q", rec.Request.Body)
	}
}

func TestReplayUnknownID(t *testing.T) {
	hub := New("127.0.0.1:3000")
	if _, err := hub.Replay(99); err == nil {
		t.Fatal("want an error for an id that was never recorded")
	}
	res := get(t, hub.Handler(), "POST", "/api/replay?id=99")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", res.StatusCode)
	}
}

// A body that was too large to keep would be replayed short, and its
// Content-Length would then hang the local app until the deadline. Refusing is
// the honest answer.
func TestReplayRefusesATruncatedBody(t *testing.T) {
	hub := New("127.0.0.1:3000")
	ex := hub.Begin([]byte("POST /big HTTP/1.1\r\nHost: x\r\n\r\n"))
	ex.Request().Write([]byte(strings.Repeat("a", maxBody+1)))
	ex.Close()

	_, err := hub.Replay(ex.ID())
	if err == nil {
		t.Fatal("want an error for a truncated body")
	}
	if !strings.Contains(err.Error(), "replayed") {
		t.Errorf("error = %v, which does not explain why", err)
	}
}

func TestReplayWhenTheLocalAppIsDown(t *testing.T) {
	// A port nothing is listening on: bound, then released.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()

	hub := New(dead)
	ex := hub.Begin([]byte(getHead))
	ex.Close()

	if _, err := hub.Replay(ex.ID()); err == nil {
		t.Fatal("want an error when the local app is not listening")
	}
	res := get(t, hub.Handler(), "POST", "/api/replay?id="+strconv.FormatInt(ex.ID(), 10))
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
}

func TestReplayEndpointIsPOSTOnly(t *testing.T) {
	hub := New("127.0.0.1:3000")
	res := get(t, hub.Handler(), "GET", "/api/replay?id=1")
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", res.StatusCode)
	}
}

func TestReplayEndpointRejectsABadID(t *testing.T) {
	hub := New("127.0.0.1:3000")
	res := get(t, hub.Handler(), "POST", "/api/replay?id=nonsense")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestStartRefusesAPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := New("127.0.0.1:3000").Start(ln.Addr().String()); err == nil {
		t.Fatal("want a bind error for a port already in use")
	}
}

// abortOnHalfClose stands in for Node's HTTP server, which treats a client
// half-close before the response as the client giving up and hangs up without
// answering. Go's net/http tolerates it, so only a server like this one
// catches the mistake.
func abortOnHalfClose(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						// EOF here is the half-close: give up without answering,
						// exactly as Node does.
						return
					}
					if strings.TrimSpace(line) == "" {
						break
					}
				}
				io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nhi")
			}()
		}
	}()
	return ln.Addr().String()
}

// A replay must not half-close the request: some servers answer nothing at
// all, and the record then shows no status — which is how this was found.
func TestReplayGetsAResponseFromAServerThatAbortsOnHalfClose(t *testing.T) {
	hub := New(abortOnHalfClose(t))
	ex := hub.Begin([]byte("GET /a HTTP/1.1\r\nHost: x\r\nConnection: keep-alive\r\n\r\n"))
	ex.Close()

	id, err := hub.Replay(ex.ID())
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := hub.find(id)
	if rec.Status != 200 {
		t.Fatalf("replay recorded status %d, want 200 — the server answered nothing", rec.Status)
	}
	if rec.Response.Body != "hi" {
		t.Errorf("body = %q", rec.Response.Body)
	}
}

func TestWithConnectionClose(t *testing.T) {
	tests := []struct {
		name string
		head string
		want string
	}{{
		name: "replaces an existing connection header",
		head: "GET / HTTP/1.1\r\nHost: x\r\nConnection: keep-alive\r\nA: 1\r\n\r\n",
		want: "GET / HTTP/1.1\r\nHost: x\r\nA: 1\r\nConnection: close\r\n\r\n",
	}, {
		name: "adds one when there is none",
		head: "GET / HTTP/1.1\r\nHost: x\r\n\r\n",
		want: "GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n",
	}, {
		name: "drops the hop-by-hop companions",
		head: "GET / HTTP/1.1\r\nKeep-Alive: timeout=5\r\nProxy-Connection: keep-alive\r\n\r\n",
		want: "GET / HTTP/1.1\r\nConnection: close\r\n\r\n",
	}, {
		name: "matches the header name case-insensitively",
		head: "GET / HTTP/1.1\r\ncOnNeCtIoN: Upgrade\r\n\r\n",
		want: "GET / HTTP/1.1\r\nConnection: close\r\n\r\n",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withConnectionClose([]byte(tt.head)); string(got) != tt.want {
				t.Errorf("got  %q\nwant %q", got, tt.want)
			}
		})
	}
}

// The head is otherwise passed through untouched, since the point of a replay
// is that the app sees the request that was recorded.
func TestWithConnectionCloseLeavesEverythingElseAlone(t *testing.T) {
	head := []byte("POST /x?q=1 HTTP/1.1\r\nHost: a.b\r\nContent-Length: 3\r\nX-Odd:   spaced  \r\n\r\n")
	got := withConnectionClose(head)
	for _, want := range []string{"POST /x?q=1 HTTP/1.1", "Host: a.b", "Content-Length: 3", "X-Odd:   spaced  "} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("%q is missing from %q", want, got)
		}
	}
}
