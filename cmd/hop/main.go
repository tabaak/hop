// Command hop is the agent. It exposes a local port through a hop server.
//
//	hop http 3000
//	hop http 3000 --sub myapp
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"hop.vokh.dev/internal/client"
)

func main() {
	fs := flag.NewFlagSet("hop http", flag.ExitOnError)
	var (
		serverAddr = fs.String("server", envOr("HOP_SERVER", "hop.vokh.dev:7443"), "hop server control address")
		sub        = fs.String("sub", "", "requested subdomain (default: server picks one)")
		token      = fs.String("token", os.Getenv("HOP_TOKEN"), "agent token (or set HOP_TOKEN)")
		host       = fs.String("local-host", "127.0.0.1", "local host to forward to")
		noTLS      = fs.Bool("no-tls", false, "connect without TLS (local development only)")
	)
	fs.Usage = usage

	args := os.Args[1:]
	if len(args) < 2 || args[0] != "http" {
		usage()
		os.Exit(2)
	}
	port, err := strconv.Atoi(args[1])
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "hop: %q is not a valid port\n", args[1])
		os.Exit(2)
	}
	fs.Parse(args[2:])

	if *token == "" {
		fmt.Fprintln(os.Stderr, "hop: no token; pass --token or set HOP_TOKEN")
		os.Exit(2)
	}

	cfg := client.Config{
		Server:    *serverAddr,
		Local:     fmt.Sprintf("%s:%d", *host, port),
		Subdomain: *sub,
		Token:     *token,
		TLS:       !*noTLS,
	}

	// Reconnect with backoff, because a laptop lid closing shouldn't end the
	// session. A refusal is terminal: retrying a bad token or a name someone
	// else holds would just spin.
	const (
		minBackoff = time.Second
		maxBackoff = 30 * time.Second
		// A session that lasted this long counts as healthy, so the next drop
		// starts backing off from scratch rather than from 30s.
		healthy = 30 * time.Second
	)
	backoff := minBackoff
	for {
		start := time.Now()
		assigned, err := client.Run(cfg)

		// Ask for the same name next time so the URL survives the reconnect.
		if assigned != "" {
			cfg.Subdomain = assigned
		}
		if errors.Is(err, client.ErrRefused) {
			fmt.Fprintf(os.Stderr, "hop: %v\n", err)
			os.Exit(1)
		}
		if time.Since(start) > healthy {
			backoff = minBackoff
		}

		fmt.Fprintf(os.Stderr, "hop: %v — reconnecting in %s\n", err, backoff.Round(time.Second))
		time.Sleep(backoff)
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `hop — expose a local port through a hop server

usage:
  hop http <port> [flags]

flags:
  --sub <name>       requested subdomain (default: server picks one)
  --server <addr>    control address (default $HOP_SERVER or hop.vokh.dev:7443)
  --token <token>    agent token (default $HOP_TOKEN)
  --local-host <ip>  local host to forward to (default 127.0.0.1)
  --no-tls           connect without TLS (local development only)
`)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
