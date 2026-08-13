package server_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
	"hop.vokh.dev/internal/server"
)

const testToken = "test-token"

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
	h := newHarnessOpts(t, harnessOpts{scheme: "http", log: func(method, target string, status int, took time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf("%s %s %d", method, target, status))
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
	h := newHarnessOpts(t, harnessOpts{scheme: "http", log: func(method, target string, status int, took time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, fmt.Sprintf("%s %s %d", method, target, status))
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

// harness wires a local app, a hop server and a connected agent together.
type harness struct {
	ingress *httptest.Server
	// local is the address the agent forwards to, i.e. what --host-header
	// rewrite should produce.
	local string
}

// harnessOpts covers the agent-side knobs the tests vary.
type harnessOpts struct {
	useTLS      bool
	scheme      string
	rewriteHost bool
	hostHeader  string
	log         func(method, target string, status int, took time.Duration)
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
		Tokens:       map[string]bool{testToken: true},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	local := strings.TrimPrefix(app.URL, "http://")
	cfg := client.Config{
		Local:     local,
		Subdomain: "myapp",
		Token:     testToken,
		TLS:       opts.useTLS,
		Log:       opts.log,
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
	h := &harness{ingress: ingress, local: local}

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
