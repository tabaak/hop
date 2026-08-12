package server_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
}

func newHarness(t *testing.T, useTLS bool) *harness {
	t.Helper()

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/echo" {
			body, _ := io.ReadAll(r.Body)
			io.WriteString(w, r.Method+" "+r.URL.RequestURI()+" "+string(body))
			return
		}
		io.WriteString(w, "local app saw "+r.URL.Path)
	}))
	t.Cleanup(app.Close)

	srv := server.New(server.Config{
		Domain:       "localhost",
		PublicScheme: "http",
		Tokens:       map[string]bool{testToken: true},
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	cfg := client.Config{
		Local:     strings.TrimPrefix(app.URL, "http://"),
		Subdomain: "myapp",
		Token:     testToken,
		TLS:       useTLS,
	}

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
	h := &harness{ingress: ingress}

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
