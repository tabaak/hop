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
	"sync"
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
		hostHeader = fs.String("host-header", "preserve", "Host sent to the local app: preserve, rewrite, or a literal value")
		quiet      = fs.Bool("quiet", false, "don't log requests")
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

	local := fmt.Sprintf("%s:%d", *host, port)
	cfg := client.Config{
		Server:     *serverAddr,
		Local:      local,
		Subdomain:  *sub,
		Token:      *token,
		TLS:        !*noTLS,
		HostHeader: resolveHostHeader(*hostHeader, local),
	}
	if !*quiet {
		cfg.Log = logRequest
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

// resolveHostHeader maps the flag onto the Host the local app should see.
// "rewrite" is the common case: Vite and a few other dev servers reject
// requests whose Host they don't recognise, and pointing it at the local
// address is what they expect. A literal value covers everything else.
func resolveHostHeader(flagVal, local string) string {
	switch flagVal {
	case "", "preserve":
		return ""
	case "rewrite":
		return local
	default:
		return flagVal
	}
}

var logMu sync.Mutex

// logRequest prints one line per request. Status and duration come before the
// target so the columns line up when paths vary in length, which is most of
// the value of having a live log at all.
func logRequest(method, target string, status int, took time.Duration) {
	if method == "" {
		method, target = "?", "?"
	}
	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(os.Stderr, "  %-6s %3s %8s  %s\n", method, statusText(status), duration(took), target)
}

// statusText renders an unparseable status as "---" rather than 0, which would
// read as a real code.
func statusText(status int) string {
	if status == 0 {
		return "---"
	}
	return strconv.Itoa(status)
}

func duration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
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
  --host-header <v>  Host sent to the local app: preserve (default), rewrite,
                     or a literal value
  --quiet            don't log requests
  --no-tls           connect without TLS (local development only)
`)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
