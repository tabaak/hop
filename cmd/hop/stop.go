package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"hop.vokh.dev/internal/proto"
)

// stopGrace is how long a stopped agent is given to close its tunnel and clean
// up before it is killed outright. It only has a socket to close, so anything
// slower than this is wedged rather than busy.
const stopGrace = 5 * time.Second

// runStop ends tunnels running on this machine.
//
// Local only, and deliberately so: `hop ps` lists what every device is serving,
// but the process behind another device's tunnel is on that device. Making stop
// reach across would mean the server could kill an agent on request, which is a
// much bigger idea than this flag — so a name that isn't ours says so plainly.
func runStop(args []string) {
	fs := flag.NewFlagSet("hop stop", flag.ExitOnError)
	var all bool
	fs.BoolVar(&all, "a", false, "stop every tunnel on this machine")
	fs.BoolVar(&all, "all", false, "stop every tunnel on this machine")
	noColour := fs.Bool("no-color", false, "disable colour")
	fs.Usage = usage
	fs.Parse(args)

	initColourOn(os.Stdout, *noColour)

	live, err := liveStates()
	if err != nil {
		fatal("%v", err)
	}
	names := fs.Args()

	switch {
	case all && len(names) > 0:
		fmt.Fprintln(os.Stderr, "hop: give either --all or a name, not both")
		os.Exit(2)
	case !all && len(names) == 0:
		fmt.Fprintln(os.Stderr, "hop: which tunnel? Name one, or pass --all")
		listRunning(live)
		os.Exit(2)
	}

	targets := live
	if !all {
		targets = nil
		for _, name := range names {
			s, ok := findState(live, name)
			if !ok {
				fmt.Fprintf(os.Stderr, "hop: no tunnel named %q is running on this machine\n", name)
				listRunning(live)
				// `hop ps` shows other devices too, so a name that is plainly
				// up can still be unstoppable from here. Say which case this is
				// rather than leaving the user to wonder.
				fmt.Fprintf(os.Stderr, "\n  `hop ps` lists tunnels from every device; only this machine's can be stopped here.\n")
				os.Exit(1)
			}
			targets = append(targets, s)
		}
	}

	if len(targets) == 0 {
		fmt.Printf("\n  No tunnels are running on this machine.\n\n")
		return
	}

	// Names padded to a common width so the detail after them lines up, the
	// same as every other list this tool prints.
	w := 0
	for _, s := range targets {
		w = max(w, len(displayName(s)))
	}

	fmt.Println()
	for _, s := range targets {
		if err := stopOne(s); err != nil {
			fmt.Printf("  %s %s: %v\n", paint("could not stop", red), describe(s, w), err)
			continue
		}
		fmt.Printf("  %s %s\n", paint("stopped", green), describe(s, w))
	}
	fmt.Println()
}

// stopOne asks an agent to exit, then makes sure it did. The wait is on the
// state file's lock rather than on the PID: the kernel releases that exactly
// when the process is gone, with no window in which a recycled PID could be
// mistaken for the original.
func stopOne(s State) error {
	if err := syscall.Kill(s.PID, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil // already gone; its record will be swept
		}
		return err
	}
	if gone(s.PID, stopGrace) {
		return nil
	}
	// Wedged. SIGTERM is a request; this isn't.
	if err := syscall.Kill(s.PID, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	if !gone(s.PID, stopGrace) {
		return fmt.Errorf("pid %d did not exit", s.PID)
	}
	return nil
}

// gone reports whether the agent released its state file within d.
func gone(pid int, d time.Duration) bool {
	dir, err := subDir("run")
	if err != nil {
		return false
	}
	path := statePath(dir, pid)

	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, held, err := readState(path); err != nil || !held {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func findState(live []State, name string) (State, bool) {
	for _, s := range live {
		if s.Subdomain == name {
			return s, true
		}
	}
	// A PID works too, which is the only handle a tunnel has before its
	// handshake completes.
	for _, s := range live {
		if fmt.Sprint(s.PID) == name {
			return s, true
		}
	}
	return State{}, false
}

// displayName is the tunnel's name, or a stand-in for one that hasn't been
// assigned yet — an agent still dialling has a PID but no name.
func displayName(s State) string {
	if s.Subdomain == "" {
		return "(connecting)"
	}
	return s.Subdomain
}

func describe(s State, w int) string {
	name := displayName(s)
	// Cleaned even though this machine wrote it: it came from a command line,
	// and a display path that trusts its input is one odd argument away from a
	// mangled table.
	detail := fmt.Sprintf("pid %d → %s", s.PID, proto.CleanLocal(s.Local))
	return pad(paint(name, cyan), name, w) + "  " + paint(detail, dim)
}

func listRunning(live []State) {
	if len(live) == 0 {
		fmt.Fprintln(os.Stderr, "\n  Nothing is running on this machine.")
		return
	}
	var names []string
	for _, s := range live {
		if s.Subdomain != "" {
			names = append(names, s.Subdomain)
		}
	}
	fmt.Fprintf(os.Stderr, "\n  Running here: %s\n", strings.Join(names, ", "))
}
