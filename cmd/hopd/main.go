// Command hopd is the hop server: public HTTP(S) ingress plus the agent
// control listener.
//
// In production it serves TLS on :443 using a wildcard certificate obtained via
// the ACME DNS-01 challenge, redirects :80, and accepts agents over TLS on
// :7443. Pass -tls=false for plaintext local development.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"hop.vokh.dev/internal/certs"
	"hop.vokh.dev/internal/server"
)

func main() {
	var (
		useTLS      = flag.Bool("tls", true, "serve TLS with an ACME wildcard certificate")
		httpsAddr   = flag.String("https", ":443", "public HTTPS ingress address")
		httpAddr    = flag.String("http", ":80", "redirect-to-HTTPS address; the plaintext ingress when -tls=false")
		controlAddr = flag.String("control", ":7443", "agent control address")
		domain      = flag.String("domain", "hop.vokh.dev", "zone tunnels live under")
		publicPort  = flag.String("public-port", "", "port appended to agent-facing URLs; empty for the scheme default")
		tokensFlag  = flag.String("tokens", "", "comma-separated agent tokens (or set HOP_TOKENS)")
		email       = flag.String("email", "", "ACME account email for expiry notices")
		staging     = flag.Bool("staging", true, "use the Let's Encrypt staging CA; set false for real certificates")
		certDir     = flag.String("cert-dir", "/var/lib/hop/certs", "directory for the ACME account key and certificates")
	)
	flag.Parse()

	tokens := parseTokens(*tokensFlag)
	if len(tokens) == 0 {
		log.Fatal("no tokens configured: pass -tokens or set HOP_TOKENS")
	}

	scheme := "http"
	if *useTLS {
		scheme = "https"
	}
	srv := server.New(server.Config{
		Domain:       *domain,
		PublicScheme: scheme,
		PublicPort:   *publicPort,
		Tokens:       tokens,
	})

	if !*useTLS {
		runPlaintext(srv, *httpAddr, *controlAddr, *domain)
		return
	}
	runTLS(srv, tlsOpts{
		httpsAddr:   *httpsAddr,
		httpAddr:    *httpAddr,
		controlAddr: *controlAddr,
		domain:      *domain,
		email:       *email,
		staging:     *staging,
		certDir:     *certDir,
	})
}

// runPlaintext is the local development mode from M1: no certificates, no DNS.
func runPlaintext(srv *server.Server, httpAddr, controlAddr, domain string) {
	log.Print("TLS disabled: serving plaintext (development mode)")

	controlLn, err := net.Listen("tcp", controlAddr)
	if err != nil {
		log.Fatalf("control listen: %v", err)
	}
	go serveControl(srv, controlLn)

	ingress := &http.Server{
		Addr:              httpAddr,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("ingress listening on %s for *.%s", httpAddr, domain)
	log.Fatalf("ingress: %v", ingress.ListenAndServe())
}

type tlsOpts struct {
	httpsAddr, httpAddr, controlAddr string
	domain, email, certDir           string
	staging                          bool
}

func runTLS(srv *server.Server, o tlsOpts) {
	if o.staging {
		log.Print("using the Let's Encrypt STAGING CA — browsers will not trust these certificates")
	} else {
		log.Print("using the Let's Encrypt production CA")
	}

	// Obtaining the wildcard blocks: it writes a DNS TXT record and waits for
	// it to propagate. Better to fail here, loudly, than to bind :443 and serve
	// handshake errors.
	log.Printf("obtaining certificate for %s and *.%s ...", o.domain, o.domain)
	base, err := certs.TLSConfig(context.Background(), certs.Config{
		Domain:          o.domain,
		Email:           o.email,
		CloudflareToken: os.Getenv("CLOUDFLARE_API_TOKEN"),
		StorageDir:      o.certDir,
		Staging:         o.staging,
	})
	if err != nil {
		log.Fatalf("certificates: %v", err)
	}
	log.Print("certificate ready")

	// The ingress speaks HTTP, so it advertises HTTP protocols. The control
	// listener carries our own framing and advertises nothing.
	ingressTLS := base.Clone()
	ingressTLS.NextProtos = []string{"h2", "http/1.1"}
	ingressTLS.MinVersion = tls.VersionTLS12

	controlTLS := base.Clone()
	controlTLS.NextProtos = nil
	controlTLS.MinVersion = tls.VersionTLS12

	controlLn, err := tls.Listen("tcp", o.controlAddr, controlTLS)
	if err != nil {
		log.Fatalf("control listen: %v", err)
	}
	go serveControl(srv, controlLn)

	go func() {
		redirect := &http.Server{
			Addr:              o.httpAddr,
			Handler:           http.HandlerFunc(redirectToHTTPS),
			ReadHeaderTimeout: 15 * time.Second,
		}
		log.Printf("redirecting %s to https", o.httpAddr)
		log.Printf("redirect listener stopped: %v", redirect.ListenAndServe())
	}()

	ingress := &http.Server{
		Addr:              o.httpsAddr,
		Handler:           srv,
		TLSConfig:         ingressTLS,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("ingress listening on %s for *.%s", o.httpsAddr, o.domain)
	// Certificates come from TLSConfig, so there are no files to name here.
	log.Fatalf("ingress: %v", ingress.ListenAndServeTLS("", ""))
}

func serveControl(srv *server.Server, ln net.Listener) {
	log.Printf("control listening on %s", ln.Addr())
	log.Fatalf("control: %v", srv.ServeControl(ln))
}

func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	target := "https://" + host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// parseTokens reads tokens from the flag, falling back to HOP_TOKENS so the
// systemd unit can keep them out of the process arguments.
func parseTokens(flagVal string) map[string]bool {
	raw := flagVal
	if raw == "" {
		raw = os.Getenv("HOP_TOKENS")
	}
	tokens := make(map[string]bool)
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tokens[t] = true
		}
	}
	return tokens
}
