package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"hop.vokh.dev/internal/client"
)

// detachEnv marks the re-executed copy, so the child runs the tunnel instead of
// spawning another child. Re-executing with the original argv and letting the
// child ignore --detach keeps the two halves in step: there is no second place
// that has to know how to rebuild the command line, and a flag added later
// needs no change here.
const detachEnv = "HOP_DETACHED"

// detachTimeout is how long the parent waits for the child to report a tunnel
// before giving up on it. Generous, because it covers DNS, a TLS handshake and
// the server's ack — but bounded, so a hung dial doesn't hang the terminal.
const detachTimeout = 30 * time.Second

// logRetention is how long the log of a finished agent is kept. Long enough to
// come back on Monday and read why Friday's tunnel died; short enough that the
// directory doesn't accumulate for the life of the machine.
const logRetention = 14 * 24 * time.Hour

// socketGrace is the equivalent for an agent's inspector socket, and is short
// because a socket is not kept for its contents — it has none. Once its agent
// is gone the file is unreachable as well as empty: `hop inspect` finds a
// socket only by first finding its agent in liveStates, so one with no live
// owner can never be opened again by anything.
//
// What the delay guards is a starting agent, not a finished one. An agent
// listens on its socket a few statements before it records itself, so for that
// window it owns a socket while looking dead to a concurrent sweep. A minute
// is five orders of magnitude more than that gap, and both sides read the same
// clock, so there is no skew to pad for either.
const socketGrace = time.Minute

// spawnDetached re-executes this binary in its own session and returns once the
// child has a tunnel, so the shell prompt comes back with the URL already
// printed and a failed handshake is still reported as a failure. Starting the
// child and exiting immediately would report success for a bad token.
func spawnDetached() {
	exe, err := os.Executable()
	if err != nil {
		fatal("cannot find my own binary: %v", err)
	}
	logDir, err := subDir("log")
	if err != nil {
		fatal("%v", err)
	}
	// Nothing else ever removes the logs and inspector sockets agents leave
	// behind, and every detached start adds one of each.
	// Done here rather than on a timer because this is the only moment the tool
	// knows it is about to add to the pile.
	pruneRuntime(logRetention, socketGrace)

	// The log is named for the child's PID, which doesn't exist until it does.
	// Opened under a temporary name and renamed after the fork: the child's
	// inherited descriptor follows the file, not the name.
	tmpLog := filepath.Join(logDir, fmt.Sprintf("starting-%d.log", os.Getpid()))
	lf, err := os.OpenFile(tmpLog, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		fatal("cannot open a log file: %v", err)
	}
	defer lf.Close()

	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Env = append(os.Environ(), detachEnv+"=1")
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.Stdin = nil
	// Setsid detaches the child from the controlling terminal, so closing the
	// window doesn't SIGHUP the tunnel — the whole point of running detached.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		os.Remove(tmpLog)
		fatal("cannot start: %v", err)
	}
	pid := cmd.Process.Pid
	logPath := filepath.Join(logDir, strconv.Itoa(pid)+".log")
	os.Rename(tmpLog, logPath)

	// Reaped in the background so an early exit is noticed rather than waited
	// out. Once this process exits the child is reparented to init, which does
	// the reaping from then on.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	s, err := waitForState(pid, done, detachTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hop: detached agent failed to start: %v\n", err)
		if out := tail(logPath, 10); out != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n", out)
		}
		// Killing it is a no-op if it already exited, and stops a hung dial
		// from lingering unowned after we have declared failure.
		cmd.Process.Kill()
		os.Exit(1)
	}

	fmt.Printf("\n  %s  →  http://%s\n\n", s.URL, s.Local)
	fmt.Printf("  detached, pid %d. %s for the request log,\n  %s to watch requests in a browser, %s to end it.\n\n",
		pid,
		paint("hop log "+s.Subdomain, cyan),
		paint("hop inspect "+s.Subdomain, cyan),
		paint("hop stop "+s.Subdomain, cyan),
	)
}

// isDetachedChild reports whether this process is the re-executed half. It
// behaves like any other agent; only its output already goes to a file, so
// colour switches itself off and nobody is watching for progress.
func isDetachedChild() bool { return os.Getenv(detachEnv) != "" }

// byeWait bounds the goodbye. It is a round trip to the server on a connection
// that is already open, so this is generous; the point of a bound at all is
// that Ctrl-C must never appear to hang, least of all when the reason for
// pressing it was a network that had already gone.
const byeWait = time.Second

// cleanupOnSignal ends the tunnel tidily on a termination signal: it tells the
// server the stop was deliberate, so the name is free immediately rather than
// held for the grace window, and it removes the state record. Without the
// latter the file would sit there until something noticed its lock was free —
// correct, but it means `hop ps` cleans up after a process the user stopped a
// week ago.
//
// Only the signals that mean "stop" are handled, and the process normally dies
// where it says so below: the handler does its work and re-raises with the
// default disposition, so the exit status is the one the caller expects.
//
// The returned channel closes once the handler has finished, and exists
// because "normally" is not "always". A process started as a background job by
// a non-interactive shell inherits SIGINT ignored; Notify overrides that so the
// handler still runs, but Reset puts the ignore back and the re-raise does
// nothing at all. The reconnect loop waits on this channel rather than on being
// killed, so that case exits promptly instead of hanging on a signal that is
// never coming.
func cleanupOnSignal(sf *stateFile, farewell *client.Farewell) <-chan struct{} {
	done := make(chan struct{})
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		defer close(done)
		sig := <-ch
		// Said first. Releasing the local record is instant and the goodbye is
		// a round trip, so doing it the other way round would mean `hop stop`
		// followed immediately by `hop http --sub <same>` could find the name
		// still held by the agent it just stopped.
		farewell.Say(byeWait)
		if sf != nil {
			sf.release()
		}
		signal.Reset(sig.(syscall.Signal))
		syscall.Kill(os.Getpid(), sig.(syscall.Signal))
	}()
	return done
}

// tail returns the last n lines of a file, for explaining a child that died
// before it could report anything.
func tail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// shortenHome renders a path the way a person would write it, so the hint fits
// on one line.
func shortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || !strings.HasPrefix(path, home+string(filepath.Separator)) {
		return path
	}
	return "~" + strings.TrimPrefix(path, home)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "hop: "+format+"\n", args...)
	os.Exit(1)
}
