package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hop.vokh.dev/internal/inspect"
)

// watchPoll is how often the inspector checks that the tunnel it is serving is
// still alive. The state file's advisory lock is the same liveness signal every
// other subcommand uses: a free lock proves the agent is gone, however it died.
// Polling rather than watching — there is nothing to subscribe to, and reading
// a small file twice a second costs nothing.
const watchPoll = 500 * time.Millisecond

// runInspect serves the request inspector of a tunnel that is already running,
// which is the one thing `--inspect` cannot do: decide afterwards.
//
// It binds 127.0.0.1:4040 itself and proxies to the agent over a private unix
// socket the agent has held since start, so capture costs nothing until someone
// actually asks to look, and two tunnels never fight over a port before that.
//
// The command stays in the foreground, like `hop log -f`, and exits when the
// tunnel does: the records live in the agent's memory, so an exited tunnel has
// an empty feed by definition.
func runInspect(args []string) {
	fs := flag.NewFlagSet("hop inspect", flag.ExitOnError)
	noOpen := fs.Bool("no-open", false, "don't open the inspector page in a browser")
	noColour := fs.Bool("no-color", false, "disable colour")
	fs.Usage = usage
	targets := parseFlags(fs, args)

	initColourOn(os.Stdout, *noColour)

	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "hop: which tunnel? e.g. hop inspect myapp")
		live, _ := liveStates()
		listRunning(live)
		os.Exit(2)
	}
	// One page per machine — 4040 is fixed — so naming several would silently
	// inspect only the last of them.
	if len(targets) > 1 {
		fmt.Fprintln(os.Stderr, "hop: name one tunnel; the inspector serves one at a time")
		os.Exit(2)
	}

	live, err := liveStates()
	if err != nil {
		fatal("%v", err)
	}
	matches, ok := findState(live, targets[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "hop: no tunnel named %q is running on this machine\n", targets[0])
		listRunning(live)
		fmt.Fprintf(os.Stderr, "\n  `hop ps` lists tunnels from every device; only this machine's can be inspected here.\n")
		os.Exit(1)
	}
	if len(matches) > 1 {
		ambiguousRef(targets[0], matches)
		os.Exit(1)
	}
	s := matches[0]

	socketPath, err := socketPathFor(s.PID)
	if err != nil {
		fatal("%v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		fatal("%s (pid %d) has no inspector socket — restart the tunnel with this version of hop",
			displayName(s), s.PID)
	}

	ln, err := net.Listen("tcp", inspect.DefaultAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hop: %s is already in use\n", inspect.DefaultAddr)
		fmt.Fprintf(os.Stderr, "\n  another --inspector or hop inspect is probably serving there; stop it first —\n  the inspector looks at one tunnel at a time\n")
		os.Exit(1)
	}

	srv := &http.Server{Handler: inspect.Proxy(socketPath), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	name := displayName(s)
	fmt.Printf("\n  inspector  →  http://%s   (%s, pid %d)\n", ln.Addr(), paint(name, cyan), s.PID)
	if s.URL != "" {
		fmt.Printf("  %s  →  %s\n", s.URL, s.Local)
	}
	fmt.Printf("\n  Ctrl-C to stop watching %s.\n\n", paint(name, cyan))

	if !*noOpen {
		openBrowser("http://" + ln.Addr().String())
	}

	if waitForEnd(s) == watchAgentGone {
		fmt.Printf("\n  %s exited — closing the inspector.\n", paint(name, cyan))
	}
	srv.Close()
	<-served
}

type watchResult int

const (
	watchStopped   watchResult = iota // the user ended it
	watchAgentGone                    // the tunnel ended it
)

// waitForEnd blocks until the user interrupts or the inspected agent releases
// its state record, which is how the process announces its death whatever way
// it died.
func waitForEnd(s State) watchResult {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)

	dir, err := subDir("run")
	if err != nil {
		// Nothing to poll without it; Ctrl-C still works.
		<-sig
		return watchStopped
	}
	path := statePath(dir, s.PID)

	tick := time.NewTicker(watchPoll)
	defer tick.Stop()
	for {
		select {
		case <-sig:
			return watchStopped
		case <-tick.C:
			if _, held, err := readState(path); err != nil || !held {
				return watchAgentGone
			}
		}
	}
}
