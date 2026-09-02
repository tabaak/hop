package inspect

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// unixClient dials a unix socket, addressing requests at 127.0.0.1 like the
// proxy does on the browser's behalf — anything else is refused by design.
func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}
}

// sockPath is a fresh path for a test socket. Not t.TempDir(): macOS caps
// unix socket addresses at 104 bytes and the test-specific names overrun it,
// while anything under ~/.hop/run never gets close.
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hop-inspect")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, strconv.Itoa(os.Getpid())+".sock")
}

// servedHub returns a hub pointed at local, listening on a fresh unix socket,
// and an HTTP server proxying to it — the whole `hop inspect` arrangement in
// miniature.
func servedHub(t *testing.T, local string) (*Hub, *httptest.Server) {
	t.Helper()
	hub := New(local)
	path := sockPath(t)
	if err := hub.ListenUnix(path); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(Proxy(path))
	t.Cleanup(srv.Close)
	return hub, srv
}

// The socket speaks the same surface 4040 does, so `hop inspect` can serve a
// tunnel that was started without --inspect without the agent growing a second
// API for it.
func TestProxyServesTheInspectorOverTheSocket(t *testing.T) {
	hub, srv := servedHub(t, "127.0.0.1:3000")
	hub.SetPublicURL("https://myapp.example")
	ex := hub.Begin([]byte(getHead))
	ex.Response().Write([]byte("HTTP/1.1 204 No Content\r\n\r\n"))
	ex.Close()

	res, err := http.Get(srv.URL + "/api/records")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got state
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Local != "127.0.0.1:3000" || got.PublicURL != "https://myapp.example" {
		t.Errorf("local = %q public = %q", got.Local, got.PublicURL)
	}
	if len(got.Records) != 1 || got.Records[0].Target != "/a?q=1" || !got.Records[0].Done {
		t.Fatalf("records = %+v", got.Records)
	}

	page, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	if page.StatusCode != 200 || !strings.Contains(string(body), "hop inspector") {
		t.Errorf("the UI did not come through the proxy (status %d)", page.StatusCode)
	}
}

// The agent's loopback-Host guard must survive the hop through the socket: the
// proxy preserves what the browser sent, so a rebound name is refused exactly
// as if the browser had reached the agent's own listener.
func TestProxyKeepsTheLoopbackGuard(t *testing.T) {
	_, srv := servedHub(t, "127.0.0.1:3000")

	req, _ := http.NewRequest("GET", srv.URL+"/api/records", nil)
	req.Host = "evil.example.com"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want the agent's 403", res.StatusCode)
	}
}

// The feed has to arrive live through the proxy. A buffered proxy would hold
// every event until the stream ended, which for SSE is never.
func TestEventsStreamThroughTheProxy(t *testing.T) {
	hub, srv := servedHub(t, "127.0.0.1:3000")

	res, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type = %q", ct)
	}

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
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "/a?q=1") {
			return
		}
	}
	t.Fatal("no event arrived")
}

// Replay goes through the proxy too: capture, then ask for the request again
// from the far side of the socket.
func TestReplayThroughTheProxy(t *testing.T) {
	replayed := make(chan string, 1)
	local := localApp(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		replayed <- string(b)
		w.WriteHeader(http.StatusTeapot)
	}))

	hub, srv := servedHub(t, local)
	head := "POST /things HTTP/1.1\r\nHost: " + local + "\r\nContent-Length: 7\r\n\r\n"
	ex := hub.Begin([]byte(head))
	ex.Request().Write([]byte(`{"a":1}`))
	ex.Close()

	url := srv.URL + "/api/replay?id=" + strconv.FormatInt(ex.ID(), 10)
	res, err := http.Post(url, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := <-replayed; got != `{"a":1}` {
		t.Errorf("local app got %q", got)
	}
}

// A dead agent leaves its socket file behind; the next agent with that pid
// binds anyway rather than failing until something sweeps the directory.
func TestListenUnixRemovesAStaleSocket(t *testing.T) {
	path := sockPath(t)
	if err := os.WriteFile(path, []byte("debris"), 0o600); err != nil {
		t.Fatal(err)
	}
	hub := New("127.0.0.1:3000")
	if err := hub.ListenUnix(path); err != nil {
		t.Fatalf("ListenUnix refused to replace stale debris: %v", err)
	}
	res, err := unixClient(path).Get("http://127.0.0.1/api/records")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("status = %d", res.StatusCode)
	}
}

// Capture runs whether or not anyone is watching; the records have to be there
// when someone finally attaches. This pins the optimisation that skips encoding
// events for zero subscribers — it must skip only the encoding.
func TestCaptureWithoutWatchersStillRecords(t *testing.T) {
	hub, srv := servedHub(t, "127.0.0.1:3000")

	// No subscriber exists yet, so publish takes the short path.
	for i := 0; i < 3; i++ {
		hub.Begin([]byte(getHead)).Close()
	}

	res, err := http.Get(srv.URL + "/api/records")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var got state
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Records) != 3 {
		t.Fatalf("records = %d, want all three despite nobody watching", len(got.Records))
	}
}
