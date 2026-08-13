// Command hopd is the hop server: public HTTP(S) ingress plus the agent
// control listener.
//
// It runs in two topologies. Standalone, it terminates TLS itself on :443 with
// an ACME wildcard certificate and redirects :80. Behind a reverse proxy that
// already owns those ports, it serves the ingress in plaintext on an internal
// address (-ingress-tls=false) while still terminating TLS on the control
// listener, which speaks hop's own protocol and cannot be proxied as HTTP.
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
		ingressAddr  = flag.String("ingress", ":443", "public ingress address")
		redirectAddr = flag.String("redirect", ":80", "address serving the HTTP-to-HTTPS redirect; empty to disable")
		controlAddr  = flag.String("control", ":7443", "agent control address")
		ingressTLS   = flag.Bool("ingress-tls", true, "terminate TLS on the ingress; false when behind a reverse proxy")
		controlTLS   = flag.Bool("control-tls", true, "terminate TLS on the control listener")
		scheme       = flag.String("scheme", "", "scheme for agent-facing URLs (default: https when -ingress-tls, else http)")
		domain       = flag.String("domain", "hop.vokh.dev", "zone tunnels live under")
		publicPort   = flag.String("public-port", "", "port appended to agent-facing URLs; empty for the scheme default")
		tokensFlag   = flag.String("tokens", "", "comma-separated agent tokens (or set HOP_TOKENS)")
		email        = flag.String("email", "", "ACME account email for expiry notices")
		staging      = flag.Bool("staging", true, "use the Let's Encrypt staging CA; set false for real certificates")
		certDir      = flag.String("cert-dir", "/var/lib/hop/certs", "directory for the ACME account key and certificates")
	)
	flag.Parse()

	tokens := parseTokens(*tokensFlag)
	if len(tokens) == 0 {
		log.Fatal("no tokens configured: pass -tokens or set HOP_TOKENS")
	}

	if *scheme == "" {
		*scheme = "http"
		if *ingressTLS {
			*scheme = "https"
		}
	}

	srv := server.New(server.Config{
		Domain:       *domain,
		PublicScheme: *scheme,
		PublicPort:   *publicPort,
		Tokens:       tokens,
	})

	// One certificate covers both listeners. The wildcard is only worth
	// obtaining if we serve tunnel traffic ourselves; behind a proxy the proxy
	// holds it and we need just the bare name for the control listener.
	var tlsCfg *tls.Config
	if *ingressTLS || *controlTLS {
		if *staging {
			log.Print("using the Let's Encrypt STAGING CA — browsers will not trust these certificates")
		} else {
			log.Print("using the Let's Encrypt production CA")
		}
		names := *domain
		if *ingressTLS {
			names += " and *." + *domain
		}
		log.Printf("obtaining certificate for %s ...", names)

		var err error
		tlsCfg, err = certs.TLSConfig(context.Background(), certs.Config{
			Domain:          *domain,
			Email:           *email,
			CloudflareToken: os.Getenv("CLOUDFLARE_API_TOKEN"),
			StorageDir:      *certDir,
			Staging:         *staging,
			Wildcard:        *ingressTLS,
		})
		if err != nil {
			log.Fatalf("certificates: %v", err)
		}
		log.Print("certificate ready")
	}

	startControl(srv, *controlAddr, tlsCfg, *controlTLS)

	if *ingressTLS && *redirectAddr != "" {
		startRedirect(*redirectAddr)
	}

	ingress := &http.Server{
		Addr:              *ingressAddr,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
	}
	if !*ingressTLS {
		log.Printf("ingress listening on %s (plaintext) for *.%s", *ingressAddr, *domain)
		log.Fatalf("ingress: %v", ingress.ListenAndServe())
	}

	// The ingress speaks HTTP, so it advertises HTTP protocols.
	ingress.TLSConfig = tlsCfg.Clone()
	ingress.TLSConfig.NextProtos = []string{"h2", "http/1.1"}
	ingress.TLSConfig.MinVersion = tls.VersionTLS12
	log.Printf("ingress listening on %s for *.%s", *ingressAddr, *domain)
	// Certificates come from TLSConfig, so there are no files to name here.
	log.Fatalf("ingress: %v", ingress.ListenAndServeTLS("", ""))
}

func startControl(srv *server.Server, addr string, tlsCfg *tls.Config, useTLS bool) {
	var (
		ln  net.Listener
		err error
	)
	if useTLS {
		// The control listener carries hop's own framing, so it advertises no
		// application protocols.
		cfg := tlsCfg.Clone()
		cfg.NextProtos = nil
		cfg.MinVersion = tls.VersionTLS12
		ln, err = tls.Listen("tcp", addr, cfg)
	} else {
		ln, err = net.Listen("tcp", addr)
	}
	if err != nil {
		log.Fatalf("control listen: %v", err)
	}

	go func() {
		if useTLS {
			log.Printf("control listening on %s", ln.Addr())
		} else {
			log.Printf("control listening on %s (plaintext)", ln.Addr())
		}
		log.Fatalf("control: %v", srv.ServeControl(ln))
	}()
}

func startRedirect(addr string) {
	go func() {
		redirect := &http.Server{
			Addr:              addr,
			Handler:           http.HandlerFunc(redirectToHTTPS),
			ReadHeaderTimeout: 15 * time.Second,
		}
		log.Printf("redirecting %s to https", addr)
		log.Printf("redirect listener stopped: %v", redirect.ListenAndServe())
	}()
}

func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusMovedPermanently)
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
