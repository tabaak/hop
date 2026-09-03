package server_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.vokh.dev/internal/client"
	"hop.vokh.dev/internal/inspect"
	"hop.vokh.dev/internal/proto"
	"hop.vokh.dev/internal/server"
	"hop.vokh.dev/internal/tokens"
)

const testToken = "test-token"

// otherToken belongs to a second label, so it is a different owner: it cannot
// take over a name the first one holds, which is what makes a refusal a
// refusal rather than a takeover.
const otherToken = "other-token"

// TestTunnelEndToEnd_Plaintext is the M1 development path: no TLS anywhere.
func TestTunnelEndToEnd_Plaintext(t *testing.T) {
	h := newHarness(t, false)
	body := h.get(t, "myapp", "/hello")
	if want := "local app saw /hello"; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// TestTunnelEndToEnd_TLS is the M2 production path: the agent dials over TLS
// and verifies the server certificate. It uses a throwaway CA rather than
// skipping verification, so a broken certificate chain still fails the test.
func TestTunnelEndToEnd_TLS(t *testing.T) {
	h := newHarness(t, true)
	body := h.get(t, "myapp", "/hello")
	if want := "local app saw /hello"; body != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}

// TestTunnelPassesRequestDetails checks that method, path, query and body all
// survive the round trip, since the agent splices raw bytes and never parses
// the HTTP itself.
func TestTunnelPassesRequestDetails(t *testing.T) {
	h := newHarness(t, false)

	req, err := http.NewRequest("POST", h.ingress.URL+"/echo?q=1", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "myapp.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through tunnel: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)

	if want := "POST /echo?q=1 payload"; string(got) != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// Behind a TLS-terminating reverse proxy the request reaching hopd is
// plaintext, but the browser used HTTPS. The local app must be told the
// scheme the client actually used, or it will build http:// redirects and
// absolute URLs.
func TestForwardedProtoReflectsPublicScheme(t *testing.T) {
	h := newHarnessScheme(t, false, "https")
	if got := h.get(t, "myapp", "/proto"); got != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want %q", got, "https")
	}
}

// A client must not be able to forge the forwarded headers.
func TestForwardedProtoIgnoresClientHeader(t *testing.T) {
	h := newHarnessScheme(t, false, "https")

	req, _ := http.NewRequest("GET", h.ingress.URL+"/proto", nil)
	req.Host = "myapp.localhost"
	req.Header.Set("X-Forwarded-Proto", "gopher")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if string(body) != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want the client header to be overridden", body)
	}
}

// By default the local app sees the public host, so links and redirects it
// builds point back through the tunnel.
func TestHostHeaderPreservedByDefault(t *testing.T) {
	h := newHarness(t, false)
	if got := h.get(t, "myapp", "/host"); got != "myapp.localhost" {
		t.Errorf("Host = %q, want %q", got, "myapp.localhost")
	}
}

// --host-header rewrite points the Host at the local address instead, for dev
// servers that reject hosts they don't recognise.
func TestHostHeaderRewrite(t *testing.T) {
	h := newHarnessOpts(t, harnessOpts{scheme: "http", rewriteHost: true})
	if got := h.get(t, "myapp", "/host"); got != h.local {
		t.Errorf("Host = %q, want the local address %q", got, h.local)
	}
}

// A literal --host-header value wins over both.
func TestHostHeaderLiteral(t *testing.T) {
	h := newHarnessOpts(t, harnessOpts{scheme: "http", hostHeader: "example.test"})
	if got := h.get(t, "myapp", "/host"); got != "example.test" {
		t.Errorf("Host = %q, want %q", got, "example.test")
	}
}

// The agent logs each request with the status the local app returned. The
// duration is time to the first byte of the response, so it is recorded when
// the status line appears rather than when the connection closes.
func TestRequestLog(t *testing.T) {
	var (
		mu   sync.Mutex
		logs []string
	)
	h := newHarnessOpts(t, harnessOpts{scheme: "http", log: func(r client.Request) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf("%s %s %d %s", r.Method, r.Target, r.Status, r.UserAgent))
	}})

	h.get(t, "myapp", "/hello")
	h.get(t, "myapp", "/missing-status")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := strings.Join(logs, "|")
		mu.Unlock()
		// waitForTunnel makes a request of its own, so match on suffix.
		if strings.Contains(got, "GET /hello 200") && strings.Contains(got, "GET /missing-status 404") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Errorf("logs = %v, want entries for /hello 200 and /missing-status 404", logs)
}

// A protocol upgrade must survive the tunnel: the agent reads the request head
// but then splices raw bytes, so what flows after the 101 is untouched. This
// is the WebSocket path, exercised with a bare hijacking handler rather than a
// real WebSocket library so the test carries no extra dependency.
func TestUpgradeSurvivesTunnel(t *testing.T) {
	var logged []string
	var mu sync.Mutex
	h := newHarnessOpts(t, harnessOpts{scheme: "http", log: func(r client.Request) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf("%s %s %d", r.Method, r.Target, r.Status))
	}})

	conn, err := net.Dial("tcp", strings.TrimPrefix(h.ingress.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	fmt.Fprint(conn, "GET /upgrade HTTP/1.1\r\nHost: myapp.localhost\r\n"+
		"Connection: Upgrade\r\nUpgrade: raw-echo\r\n\r\n")

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status line: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q, want 101", strings.TrimSpace(status))
	}
	// Drain the remaining response headers.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	// Post-upgrade the connection is a raw byte pipe in both directions.
	for _, msg := range []string{"ping", "pong", "third"} {
		if _, err := io.WriteString(conn, msg+"\n"); err != nil {
			t.Fatalf("write after upgrade: %v", err)
		}
		got, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read after upgrade: %v", err)
		}
		if want := "echo:" + msg + "\n"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(logged, "|"), "GET /upgrade 101") {
		t.Errorf("logs = %v, want the upgrade logged as 101", logged)
	}
}

func TestUnknownSubdomainIs404(t *testing.T) {
	h := newHarness(t, false)

	req, _ := http.NewRequest("GET", h.ingress.URL+"/", nil)
	req.Host = "nosuchtunnel.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// `hop ps` is the only way to see tunnels other devices are serving, so the
// listing has to come from the server over the control port rather than from
// anything the local agent knows.
func TestListReportsLiveTunnels(t *testing.T) {
	h := newHarness(t, false)

	got, err := client.List(h.agent)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("listed %d tunnels, want 1: %+v", len(got), got)
	}
	if got[0].Subdomain != "myapp" {
		t.Errorf("subdomain = %q, want %q", got[0].Subdomain, "myapp")
	}
	// The label, never the token.
	if got[0].Owner != "test-agent" {
		t.Errorf("owner = %q, want %q", got[0].Owner, "test-agent")
	}
	if want := "http://myapp.localhost"; got[0].URL != want {
		t.Errorf("url = %q, want %q", got[0].URL, want)
	}
	// Reported by the agent in its handshake — the server has no other way to
	// know what a tunnel forwards to.
	if got[0].Local != h.local {
		t.Errorf("local = %q, want the agent's forward target %q", got[0].Local, h.local)
	}
	if got[0].UptimeSeconds < 0 {
		t.Errorf("uptime = %ds, want a non-negative duration", got[0].UptimeSeconds)
	}
}

// Listing must not claim a name of its own. If it did, every `hop ps` would
// consume a random subdomain and show up in its own output.
func TestListDoesNotClaimAName(t *testing.T) {
	h := newHarness(t, false)

	for i := 0; i < 3; i++ {
		got, err := client.List(h.agent)
		if err != nil {
			t.Fatalf("List %d: %v", i, err)
		}
		if len(got) != 1 {
			t.Fatalf("after %d listings, %d tunnels are up, want 1: %+v", i+1, len(got), got)
		}
	}
}

// The listing says which names are in use, which is worth as much to an
// attacker choosing a target as it is to their owner.
func TestListRequiresAValidToken(t *testing.T) {
	h := newHarness(t, false)

	cfg := h.agent
	cfg.Token = "not-the-token"
	_, err := client.List(cfg)
	if !errors.Is(err, client.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
}

// harness wires a local app, a hop server and a connected agent together.
type harness struct {
	ingress *httptest.Server
	// local is the address the agent forwards to, i.e. what --host-header
	// rewrite should produce.
	local string
	// agent is the config the connected agent used, so a test can dial the
	// control port a second time with the same credentials.
	agent client.Config
	// control is the raw listener under any TLS, so a test can cut the
	// transport the way a network does.
	control *dropListener
}

// dropListener hands out the connections it accepted so a test can kill them.
//
// Closing the yamux session instead would be the tidy way to end a tunnel, and
// the wrong thing to test: a session that shuts down cleanly is the case the
// grace window is not for. Closing the TCP connection underneath it is what a
// vanished network looks like from the server's side.
type dropListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *dropListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.conns = append(l.conns, c)
	l.mu.Unlock()
	return c, nil
}

// dropAll cuts every control connection accepted so far.
func (l *dropListener) dropAll() {
	l.mu.Lock()
	conns := l.conns
	l.conns = nil
	l.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

// harnessOpts covers the agent-side knobs the tests vary.
type harnessOpts struct {
	useTLS      bool
	scheme      string
	rewriteHost bool
	hostHeader  string
	log         func(client.Request)
	// newTap, if set, is called with the local app's address once it is known,
	// and its result is handed to the agent.
	newTap func(local string) client.Tap
}

func newHarness(t *testing.T, useTLS bool) *harness {
	t.Helper()
	return newHarnessOpts(t, harnessOpts{useTLS: useTLS, scheme: "http"})
}

// newHarnessScheme builds a harness whose server advertises the given public
// scheme, which differs from the scheme reaching it when hopd sits behind a
// TLS-terminating reverse proxy.
func newHarnessScheme(t *testing.T, useTLS bool, scheme string) *harness {
	t.Helper()
	return newHarnessOpts(t, harnessOpts{useTLS: useTLS, scheme: scheme})
}

func newHarnessOpts(t *testing.T, opts harnessOpts) *harness {
	t.Helper()

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/echo":
			body, _ := io.ReadAll(r.Body)
			io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		case "/proto":
			io.WriteString(w, r.Header.Get("X-Forwarded-Proto"))
		case "/host":
			io.WriteString(w, r.Host)
		case "/missing-status":
			w.WriteHeader(http.StatusNotFound)
		case "/upgrade":
			// Stands in for a WebSocket server: switch protocols, then echo
			// whole lines back until the peer goes away.
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", http.StatusInternalServerError)
				return
			}
			conn, brw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			io.WriteString(brw, "HTTP/1.1 101 Switching Protocols\r\n"+
				"Connection: Upgrade\r\nUpgrade: raw-echo\r\n\r\n")
			brw.Flush()
			for {
				line, err := brw.ReadString('\n')
				if err != nil {
					return
				}
				if _, err := io.WriteString(brw, "echo:"+line); err != nil {
					return
				}
				brw.Flush()
			}
		default:
			io.WriteString(w, "local app saw "+r.URL.Path)
		}
	}))
	t.Cleanup(app.Close)

	srv := server.New(server.Config{
		Domain:       "localhost",
		PublicScheme: opts.scheme,
		Tokens: tokens.New(map[string]string{
			tokens.Hash(testToken):  "test-agent",
			tokens.Hash(otherToken): "other-agent",
		}),
	})

	rawLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rawLn.Close() })
	// Wrapped under any TLS, so dropAll cuts the TCP connection rather than
	// closing a TLS session politely.
	control := &dropListener{Listener: rawLn}
	var ln net.Listener = control

	local := strings.TrimPrefix(app.URL, "http://")
	cfg := client.Config{
		Local:     local,
		Subdomain: "myapp",
		Token:     testToken,
		TLS:       opts.useTLS,
		Log:       opts.log,
	}
	if opts.newTap != nil {
		cfg.Tap = opts.newTap(local)
	}
	switch {
	case opts.hostHeader != "":
		cfg.HostHeader = opts.hostHeader
	case opts.rewriteHost:
		cfg.HostHeader = local
	}

	useTLS := opts.useTLS
	if useTLS {
		cert, pool := selfSignedCert(t)
		ln = tls.NewListener(ln, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
		cfg.RootCAs = pool
	}
	cfg.Server = ln.Addr().String()

	go srv.ServeControl(ln)
	go client.Run(cfg)

	ingress := httptest.NewServer(srv)
	t.Cleanup(ingress.Close)
	h := &harness{ingress: ingress, local: local, agent: cfg, control: control}

	h.waitForTunnel(t)
	return h
}

// waitForTunnel blocks until the agent has finished its handshake, so tests
// don't race the connection setup.
func (h *harness) waitForTunnel(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest("GET", h.ingress.URL+"/", nil)
		req.Host = "myapp.localhost"
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("tunnel did not come up within 10s")
}

// status returns just the ingress status code for a name, for the cases where
// the body is beside the point.
func (h *harness) status(t *testing.T, sub, path string) int {
	t.Helper()
	req, err := http.NewRequest("GET", h.ingress.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = sub + ".localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through ingress: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (h *harness) get(t *testing.T, sub, path string) string {
	t.Helper()
	req, err := http.NewRequest("GET", h.ingress.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = sub + ".localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request through tunnel: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// selfSignedCert returns a certificate valid for 127.0.0.1 plus a pool that
// trusts it, standing in for the ACME-issued wildcard in tests.
func selfSignedCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "hop test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// The inspector sits in a tee on both directions of a live tunnel, so what it
// records has to match what the caller and the local app actually exchanged —
// and the request must still work while it does.
func TestInspectorRecordsALiveTunnel(t *testing.T) {
	var hub *inspect.Hub
	h := newHarnessOpts(t, harnessOpts{scheme: "http", newTap: func(local string) client.Tap {
		hub = inspect.New(local)
		return inspectTap{hub}
	}})

	req, err := http.NewRequest("POST", h.ingress.URL+"/echo?q=1", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "myapp.localhost"
	req.Header.Set("X-Trace", "abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rec := waitForRecord(t, hub, "/echo?q=1")
	if rec.Method != "POST" || rec.Request.Body != "payload" {
		t.Errorf("recorded %s with body %q", rec.Method, rec.Request.Body)
	}
	if got := headerOf(rec.Request.Headers, "X-Trace"); got != "abc" {
		t.Errorf("X-Trace = %q, want abc", got)
	}
	if rec.Status != 200 || rec.Response.Body != "POST /echo?q=1 payload" {
		t.Errorf("recorded status %d body %q", rec.Status, rec.Response.Body)
	}
}

// Replaying sends the recorded bytes straight to the local app, bypassing the
// tunnel — so it works on a request the inspector saw pass through it.
func TestInspectorReplaysThroughToTheLocalApp(t *testing.T) {
	var hub *inspect.Hub
	h := newHarnessOpts(t, harnessOpts{scheme: "http", newTap: func(local string) client.Tap {
		hub = inspect.New(local)
		return inspectTap{hub}
	}})

	req, _ := http.NewRequest("POST", h.ingress.URL+"/echo", strings.NewReader("again"))
	req.Host = "myapp.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	rec := waitForRecord(t, hub, "/echo")
	id, err := hub.Replay(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayed := waitForID(t, hub, id)
	if !replayed.Replayed {
		t.Error("the replay is not marked as one")
	}
	if replayed.Status != 200 || replayed.Response.Body != "POST /echo again" {
		t.Errorf("replay got %d %q", replayed.Status, replayed.Response.Body)
	}
}

// A streaming response must not be held back by the capture: the inspector
// tees bytes as they pass rather than buffering the exchange.
func TestInspectorDoesNotStallAnUpgrade(t *testing.T) {
	var hub *inspect.Hub
	h := newHarnessOpts(t, harnessOpts{scheme: "http", newTap: func(local string) client.Tap {
		hub = inspect.New(local)
		return inspectTap{hub}
	}})

	conn, err := net.Dial("tcp", strings.TrimPrefix(h.ingress.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	io.WriteString(conn, "GET /upgrade HTTP/1.1\r\nHost: myapp.localhost\r\n"+
		"Connection: Upgrade\r\nUpgrade: raw-echo\r\n\r\n")

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("status = %q", status)
	}
	// Headers, then the echo, which only arrives if nothing buffered the
	// connection waiting for it to end.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	io.WriteString(conn, "ping\n")
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "echo:ping" {
		t.Errorf("echo = %q", line)
	}

	// The record exists while the connection is still open, with the 101 on it.
	rec := waitForRecord(t, hub, "/upgrade")
	if rec.Status != 101 {
		t.Errorf("recorded status %d, want 101", rec.Status)
	}
	if rec.Done {
		t.Error("the exchange is marked done while the connection is still open")
	}
}

// inspectTap is the same adapter cmd/hop uses: a nil *Exchange has to become a
// nil Capture rather than an interface holding a nil pointer.
type inspectTap struct{ hub *inspect.Hub }

func (t inspectTap) Begin(head []byte) client.Capture {
	if ex := t.hub.Begin(head); ex != nil {
		return ex
	}
	return nil
}

// waitForRecord waits for a record of a request to target, since the agent
// records from its own goroutine and may not have finished when the caller's
// response has.
func waitForRecord(t *testing.T, hub *inspect.Hub, target string) inspect.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, rec := range hub.Records() {
			if rec.Target == target && rec.Status != 0 {
				return rec
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no record for %s within 5s", target)
	return inspect.Record{}
}

func waitForID(t *testing.T, hub *inspect.Hub, id int64) inspect.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, rec := range hub.Records() {
			if rec.ID == id && rec.Status != 0 {
				return rec
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no record with id %d within 5s", id)
	return inspect.Record{}
}

func headerOf(headers []inspect.Header, name string) string {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// A refusal has to say why, because the agent's reaction differs: a bad token
// is worth exiting over, a taken name is worth waiting out. Before the code
// existed both arrived as the same opaque sentence and the agent exited on
// either — which, once the server started holding a name after a drop, would
// have turned every reconnect into a hard exit.
func TestRefusalCodesDistinguishTokenFromName(t *testing.T) {
	h := newHarness(t, false)
	h.waitForTunnel(t)

	t.Run("bad token", func(t *testing.T) {
		cfg := h.agent
		cfg.Token = "not-the-token"
		cfg.Subdomain = "whatever"
		_, err := client.Run(cfg)

		var refused *client.Refusal
		if !errors.As(err, &refused) {
			t.Fatalf("err = %v (%T), want a *client.Refusal", err, err)
		}
		if refused.Code != proto.CodeBadToken {
			t.Errorf("code = %q, want %q", refused.Code, proto.CodeBadToken)
		}
		if refused.Retryable() {
			t.Error("a bad token is retryable, want terminal")
		}
		if !errors.Is(err, client.ErrRefused) {
			t.Error("a Refusal no longer matches ErrRefused, which other callers test for")
		}
	})

	t.Run("name held by another owner", func(t *testing.T) {
		cfg := h.agent
		// A second label: same-owner requests are a takeover, not a refusal.
		cfg.Token = otherToken
		cfg.Subdomain = h.agent.Subdomain
		_, err := client.Run(cfg)

		var refused *client.Refusal
		if !errors.As(err, &refused) {
			t.Fatalf("err = %v (%T), want a *client.Refusal", err, err)
		}
		if refused.Code != proto.CodeTaken {
			t.Errorf("code = %q, want %q", refused.Code, proto.CodeTaken)
		}
		if !refused.Retryable() {
			t.Error("a taken name is terminal, want retryable")
		}
	})

	t.Run("illegal name", func(t *testing.T) {
		cfg := h.agent
		cfg.Subdomain = "Not A Legal Label"
		_, err := client.Run(cfg)

		var refused *client.Refusal
		if !errors.As(err, &refused) {
			t.Fatalf("err = %v (%T), want a *client.Refusal", err, err)
		}
		if refused.Code != proto.CodeBadName {
			t.Errorf("code = %q, want %q", refused.Code, proto.CodeBadName)
		}
		if refused.Retryable() {
			t.Error("an illegal name is retryable, want terminal")
		}
	})
}

// An agent built before the code field reads the same refusals as before: the
// sentence is unchanged and nothing decodes differently. The reverse skew —
// this agent against a server too old to send a code — leaves Code empty,
// which Retryable reports as terminal, i.e. exactly today's behaviour.
func TestRefusalWithoutACodeIsTerminal(t *testing.T) {
	refused := &client.Refusal{Reason: "subdomain is already in use"}
	if refused.Retryable() {
		t.Error("an uncoded refusal is retryable, want terminal")
	}
	if !errors.Is(refused, client.ErrRefused) {
		t.Error("an uncoded refusal does not match ErrRefused")
	}
}

// waitForListingWithout blocks until sub is no longer among the live tunnels,
// i.e. until the server has noticed the session behind it is gone. A listing
// shows bound tunnels only, so a name that has become a lease drops out of it.
func (h *harness) waitForListingWithout(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		live, err := client.List(h.agent)
		if err == nil {
			found := false
			for _, tn := range live {
				if tn.Subdomain == sub {
					found = true
					break
				}
			}
			if !found {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server still lists %q as live 10s after its transport was cut", sub)
}

// The milestone in one test: cut the network under a running tunnel, and the
// name is still the same agent's when it comes back.
func TestDroppedTunnelKeepsItsNameForItsOwner(t *testing.T) {
	h := newHarness(t, false)

	h.control.dropAll()
	h.waitForListingWithout(t, "myapp")

	// Nothing is serving it, so it does not route...
	if code := h.status(t, "myapp", "/"); code != http.StatusServiceUnavailable {
		t.Fatalf("held name answered %d, want %d", code, http.StatusServiceUnavailable)
	}

	// ...but it is not free either. Another owner is refused, with the code
	// that says so.
	other := h.agent
	other.Token = otherToken
	other.Subdomain = "myapp"
	_, err := client.Run(other)
	var refused *client.Refusal
	if !errors.As(err, &refused) || refused.Code != proto.CodeTaken {
		t.Fatalf("another owner took a held name: err = %v", err)
	}

	// The owner comes back and gets its URL, which is the whole point: the
	// endpoint someone registered with Stripe an hour ago still works.
	go client.Run(h.agent)
	h.waitForTunnel(t)

	if got := h.get(t, "myapp", "/echo"); got != "GET /echo " {
		t.Fatalf("after reconnect: %q", got)
	}
}

// A dropped tunnel holds its name against *other* owners, never against
// itself: an agent that lost its session and came straight back must not be
// made to wait out a window its own predecessor opened. That is what stops a
// crash-looping agent from locking itself out of its own namespace, one fresh
// grace window per attempt.
func TestAnOwnerIsNeverBlockedByItsOwnHold(t *testing.T) {
	h := newHarness(t, false)

	for i := 0; i < 3; i++ {
		h.control.dropAll()
		h.waitForListingWithout(t, "myapp")

		go client.Run(h.agent)
		h.waitForTunnel(t)
	}
	if got := h.get(t, "myapp", "/echo"); got != "GET /echo " {
		t.Fatalf("after three drops and reconnects: %q", got)
	}
}

// Being refused is not a claim. A rejected request must not hold the name it
// asked for, or asking for a name you cannot have would take it out of
// circulation for everyone including the agent serving it.
func TestARefusedClaimHoldsNothing(t *testing.T) {
	h := newHarness(t, false)

	other := h.agent
	other.Token = otherToken
	other.Subdomain = "myapp"
	if _, err := client.Run(other); !errors.Is(err, client.ErrRefused) {
		t.Fatalf("err = %v, want a refusal", err)
	}

	// The holder is undisturbed, and its own reconnect still works.
	if got := h.get(t, "myapp", "/echo"); got != "GET /echo " {
		t.Fatalf("after another owner was refused: %q", got)
	}
	h.control.dropAll()
	h.waitForListingWithout(t, "myapp")
	go client.Run(h.agent)
	h.waitForTunnel(t)
}

// What a webhook sender sees while the agent is away. The distinction between
// this and a 404 is the difference between "retry in a moment" and "this
// endpoint is gone" — and senders act on it: a 404 gets a delivery dropped and
// can get the endpoint disabled, while every sender that retries at all
// retries on a 503.
func TestHeldNameAnswers503WhileANameNobodyHasStays404(t *testing.T) {
	h := newHarness(t, false)

	h.control.dropAll()
	h.waitForListingWithout(t, "myapp")

	req, _ := http.NewRequest("GET", h.ingress.URL+"/webhook", nil)
	req.Host = "myapp.localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
	if got := resp.Header.Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want %q", got, "5")
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "myapp") {
		t.Errorf("body does not name the tunnel: %q", body)
	}

	// A name nobody ever claimed is still a 404: it is not coming back,
	// and telling a sender to retry it would be the same lie in reverse.
	if code := h.status(t, "nobody-has-this", "/"); code != http.StatusNotFound {
		t.Errorf("unclaimed name answered %d, want %d", code, http.StatusNotFound)
	}
}
