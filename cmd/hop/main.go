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
	"strconv"
	"time"

	"hop.vokh.dev/internal/client"
	"hop.vokh.dev/internal/inspect"
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
	case "log", "logs":
		runLog(args[1:])
	case "inspect":
		runInspect(args[1:])
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
		inspector  = fs.Bool("inspect", false, "serve the request inspector on http://"+inspect.DefaultAddr)
		detach     bool
	)
	// Registered twice so both spellings work; Go's flag package treats -d and
	// --d as the same flag, but not -d and --detach.
	fs.BoolVar(&detach, "d", false, "run in the background")
	fs.BoolVar(&detach, "detach", false, "run in the background")
	fs.Usage = usage

	ports := parseFlags(fs, args)
	if len(ports) != 1 {
		usage()
		os.Exit(2)
	}
	port, err := strconv.Atoi(ports[0])
	if err != nil || port < 1 || port > 65535 {
		fmt.Fprintf(os.Stderr, "hop: %q is not a valid port\n", ports[0])
		os.Exit(2)
	}

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

	// Capture is always on. The inspector can be attached to a running tunnel
	// with `hop inspect <name>`, and a feed that only starts when you open it
	// would miss exactly the requests you opened it to see — the webhook that
	// arrived once, before anything was watching. The cost is bounded: fifty
	// records, bodies capped at 64KB each way, and nothing leaves the machine.
	hub := inspect.New(local)
	cfg.Tap = tap{hub}

	if p, err := socketPathFor(os.Getpid()); err != nil {
		fmt.Fprintf(os.Stderr, "hop: `hop inspect` will not reach this tunnel: %v\n", err)
	} else if err := hub.ListenUnix(p); err != nil {
		fmt.Fprintf(os.Stderr, "hop: inspector socket not started: %v\n", err)
	}

	if *inspector {
		addr, err := hub.Start(inspect.DefaultAddr)
		if err != nil {
			// Not fatal. Something else holding 4040 is a reason to lose the
			// inspector, not the tunnel the user actually asked for.
			fmt.Fprintf(os.Stderr, "hop: inspector not started: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "\n  inspector  →  http://%s\n", addr)
		}
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
		// Recorded so `hop log <name>` can find it without recomputing the
		// path from a PID it would have to trust.
		if path, err := logPathFor(os.Getpid()); err == nil {
			state.Log = path
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
		// Set on every reconnect, since the assigned name — and so the URL
		// the inspector puts in a cURL command — can change.
		hub.SetPublicURL(url)
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
	//
	// See healthySession for when a drop resets the backoff.
	attempt := 0
	for {
		start := time.Now()
		res, err := client.Run(cfg)

		// Ask for the same name next time so the URL survives the reconnect.
		if res.Subdomain != "" {
			cfg.Subdomain = res.Subdomain
		}
		// A refusal is usually terminal: retrying a bad token, an illegal name,
		// or a --no-tls dial to a public address would just spin.
		//
		// The exception is a name taken out from under a tunnel that had
		// already come up. The holder is then almost always this agent's own
		// previous session — or, once the server holds grace leases, the lease
		// that session left behind — and it lets go without being asked. Only
		// a Refusal carries a code; the private-peer check wraps ErrRefused
		// without one, and stays terminal.
		var refused *client.Refusal
		reclaimable := errors.As(err, &refused) && refused.Retryable() && everUp
		if errors.Is(err, client.ErrRefused) && !reclaimable {
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
		if healthySession(res.Served, time.Since(start)) {
			attempt = 0
		}

		wait := nextBackoff(attempt)
		fmt.Fprintf(os.Stderr, "hop: %v — reconnecting in %s\n", err, wait.Round(100*time.Millisecond))
		time.Sleep(wait)
		if attempt < maxAttempt {
			attempt++
		}
	}
}

// tap adapts the inspector to the agent's Tap interface. It exists for one
// line: a nil *Exchange has to become a nil Capture, since an interface
// holding a nil pointer is not itself nil and the agent tests for nil.
type tap struct{ hub *inspect.Hub }

func (t tap) Begin(head []byte) client.Capture {
	if ex := t.hub.Begin(head); ex != nil {
		return ex
	}
	return nil
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
  hop stop <tunnel>...      stop a tunnel running on this machine
  hop stop --all            stop all of them
  hop log <tunnel> [-f]     show a detached tunnel's output (also: logs)
  hop inspect <tunnel>      serve the request inspector for a running tunnel

  <tunnel> is its subdomain, its agent's pid, or the local port it serves.

http flags:
  --sub <name>       requested subdomain (default: server picks one)
  -d, --detach       run in the background; stop it with hop stop
  --local-host <ip>  local host to forward to (default 127.0.0.1)
  --host-header <v>  Host sent to the local app: preserve (default), rewrite,
                     or a literal value
  --inspect          serve the request inspector on http://127.0.0.1:4040
  --quiet            don't log requests

ps flags:
  --json             print the listing as JSON

stop flags:
  -a, --all          stop every tunnel on this machine

log flags:
  -n <count>         how many lines to show (default 50)
  -f, --follow       keep printing as the agent writes

inspect flags:
  --no-open          don't open the inspector page in a browser
                     ($BROWSER picks which one; default is the system's)

common flags:
  --server <addr>    control address (default $HOP_SERVER or hop.vokh.dev:7443)
  --token <token>    agent token (default $HOP_TOKEN)
  --no-tls           connect without TLS (local development only)
`)
}

// parseFlags parses args and returns the positional ones, allowing flags on
// either side of them.
//
// Go's flag package stops at the first word that isn't a flag, so `hop log
// myapp -n 2` would silently ignore -n — the kind of thing that looks like the
// flag not working. Parsing repeatedly, peeling off one positional each time,
// accepts both orders without hand-rolling a parser.
func parseFlags(fs *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) == 0 {
			return positional
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
