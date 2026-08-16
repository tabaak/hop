// Command hop is the agent. It exposes a local port through a hop server.
//
//	hop http 3000
//	hop http 3000 --sub myapp
//	hop http 3000 -d
//	hop ps
//	hop stop myapp
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"hop.vokh.dev/internal/client"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "http":
		runHTTP(args[1:])
	// ls and status are the names people reach for from other tools; all three
	// are the same command rather than one being the "real" one.
	case "ps", "ls", "status":
		runPS(args[1:])
	case "stop":
		runStop(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

// runHTTP holds a tunnel open until it is killed, so it never returns.
func runHTTP(args []string) {
	fs := flag.NewFlagSet("hop http", flag.ExitOnError)
	var (
		serverAddr = fs.String("server", envOr("HOP_SERVER", "hop.vokh.dev:7443"), "hop server control address")
		sub        = fs.String("sub", "", "requested subdomain (default: server picks one)")
		token      = fs.String("token", os.Getenv("HOP_TOKEN"), "agent token (or set HOP_TOKEN)")
		host       = fs.String("local-host", "127.0.0.1", "local host to forward to")
		noTLS      = fs.Bool("no-tls", false, "connect without TLS (local development only)")
		hostHeader = fs.String("host-header", "preserve", "Host sent to the local app: preserve, rewrite, or a literal value")
		quiet      = fs.Bool("quiet", false, "don't log requests")
		noColour   = fs.Bool("no-color", false, "disable colour in the request log")
		detach     bool
	)
	// Registered twice so both spellings work; Go's flag package treats -d and
	// --d as the same flag, but not -d and --detach.
	fs.BoolVar(&detach, "d", false, "run in the background")
	fs.BoolVar(&detach, "detach", false, "run in the background")
	fs.Usage = usage

	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "hop: %q is not a valid port\n", args[0])
		os.Exit(2)
	}
	fs.Parse(args[1:])

	if *token == "" {
		fmt.Fprintln(os.Stderr, "hop: no token; pass --token or set HOP_TOKEN")
		os.Exit(2)
	}

	// The token is checked first, so `-d` fails in the terminal rather than in
	// a log file the user hasn't been told about yet.
	detached := isDetachedChild()
	if detach && !detached {
		spawnDetached()
		return
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
		initColour(*noColour)
		cfg.Log = logRequest
	}

	// Recorded whether or not this agent is detached, so `hop ps` can mark the
	// tunnels this machine is serving and `hop stop` can reach them. A tunnel
	// started in a terminal is no harder to stop by name for it.
	state := State{
		PID:      os.Getpid(),
		Local:    local,
		Server:   *serverAddr,
		Detached: detached,
		Started:  time.Now(),
	}
	if detached {
		if dir, err := subDir("log"); err == nil {
			state.Log = filepath.Join(dir, strconv.Itoa(os.Getpid())+".log")
		}
	}
	sf, err := holdState(state)
	if err != nil {
		// Not fatal: a tunnel that works is worth more than the bookkeeping
		// that would have let `hop stop` find it. Say so and carry on.
		fmt.Fprintf(os.Stderr, "hop: not recording this tunnel for `hop ps`/`hop stop`: %v\n", err)
	} else {
		defer sf.release()
	}

	everUp := false
	cfg.OnUp = func(sub, url string) {
		everUp = true
		if sf == nil {
			return
		}
		state.Subdomain, state.URL = sub, url
		if err := sf.write(state); err != nil {
			fmt.Fprintf(os.Stderr, "hop: cannot update %s: %v\n", sf.path, err)
		}
	}
	// Ctrl-C and `hop stop` both arrive as signals, which would otherwise skip
	// every deferred call and leave the record behind. The lock would still
	// mark it dead, but a file that deletes itself is tidier than one that
	// waits to be reaped.
	cleanupOnSignal(sf)

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
			fmt.Fprintf(os.Stderr, "hop: tunnel %v\n", err)
			// os.Exit runs no deferred calls, so the record is dropped by hand.
			// The lock would mark it dead anyway; this just keeps the directory
			// honest for the next reader.
			if sf != nil {
				sf.release()
			}
			os.Exit(1)
		}
		// A detached agent that has never connected gives up instead of
		// retrying. Backing off for half a minute would be right for a tunnel
		// that dropped, but at startup there is somebody at a terminal waiting
		// to be told whether this worked — and "the server is down" is worth
		// hearing now rather than after the parent's timeout.
		if detached && !everUp {
			fmt.Fprintf(os.Stderr, "hop: %v\n", err)
			if sf != nil {
				sf.release()
			}
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

func usage() {
	fmt.Fprint(os.Stderr, `hop — expose a local port through a hop server

usage:
  hop http <port> [flags]   open a tunnel to a local port
  hop ps [flags]            list the tunnels currently up (also: ls, status)
  hop stop <name>...        stop a tunnel running on this machine
  hop stop --all            stop all of them

http flags:
  --sub <name>       requested subdomain (default: server picks one)
  -d, --detach       run in the background; stop it with hop stop
  --local-host <ip>  local host to forward to (default 127.0.0.1)
  --host-header <v>  Host sent to the local app: preserve (default), rewrite,
                     or a literal value
  --quiet            don't log requests

ps flags:
  --json             print the listing as JSON

stop flags:
  -a, --all          stop every tunnel on this machine

common flags:
  --server <addr>    control address (default $HOP_SERVER or hop.vokh.dev:7443)
  --token <token>    agent token (default $HOP_TOKEN)
  --no-tls           connect without TLS (local development only)
`)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
