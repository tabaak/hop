// Command hopd is the hop server: public HTTP ingress plus the agent control
// listener.
//
// M1 runs everything in the clear on localhost. TLS, the wildcard certificate
// and the :80 redirect arrive in M2.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"hop.vokh.dev/internal/server"
)

func main() {
	var (
		httpAddr    = flag.String("http", ":8080", "public HTTP ingress address")
		controlAddr = flag.String("control", ":7443", "agent control address")
		domain      = flag.String("domain", "localhost", "zone tunnels live under")
		scheme      = flag.String("scheme", "http", "scheme used in agent-facing URLs")
		publicPort  = flag.String("public-port", "8080", "port appended to agent-facing URLs; empty for none")
		tokensFlag  = flag.String("tokens", "", "comma-separated agent tokens (or set HOP_TOKENS)")
	)
	flag.Parse()

	tokens := parseTokens(*tokensFlag)
	if len(tokens) == 0 {
		log.Fatal("no tokens configured: pass -tokens or set HOP_TOKENS")
	}

	srv := server.New(server.Config{
		Domain:       *domain,
		PublicScheme: *scheme,
		PublicPort:   *publicPort,
		Tokens:       tokens,
	})

	controlLn, err := net.Listen("tcp", *controlAddr)
	if err != nil {
		log.Fatalf("control listen: %v", err)
	}
	go func() {
		log.Printf("control listening on %s", controlLn.Addr())
		log.Fatalf("control: %v", srv.ServeControl(controlLn))
	}()

	ingress := &http.Server{
		Addr:              *httpAddr,
		Handler:           srv,
		ReadHeaderTimeout: 15 * time.Second,
	}
	log.Printf("ingress listening on %s for *.%s", *httpAddr, *domain)
	log.Fatalf("ingress: %v", ingress.ListenAndServe())
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
