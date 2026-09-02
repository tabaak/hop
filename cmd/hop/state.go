package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Every running agent records itself under ~/.hop/run so `hop ps` can say which
// tunnels are this machine's and `hop stop` can find the process behind one.
// Foreground agents are tracked too, not just detached ones: `hop stop myapp`
// failing because that tunnel happened to be started in a terminal would be a
// distinction the user never made.
//
// Liveness is decided by an advisory lock the agent holds on its own file for
// as long as it runs, rather than by asking whether its PID exists. The kernel
// drops the lock however the process dies — SIGKILL, panic, power cut — so a
// file whose lock is free is provably stale. Checking the PID instead would
// eventually signal an unrelated process that inherited the number after a
// reboot, which is a bad way to find out the check was naive.
//
// This is Unix-only, as is detaching itself.
type State struct {
	PID int `json:"pid"`
	// Subdomain and URL are empty until the tunnel is up, which is how the
	// parent of a detached agent knows the handshake succeeded.
	Subdomain string    `json:"subdomain,omitempty"`
	URL       string    `json:"url,omitempty"`
	Local     string    `json:"local"`
	Server    string    `json:"server"`
	Detached  bool      `json:"detached"`
	Started   time.Time `json:"started"`
	Log       string    `json:"log,omitempty"`
}

// hopDir is ~/.hop, holding run/ and log/.
func hopDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".hop"), nil
}

func subDir(name string) (string, error) {
	base, err := hopDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, name)
	// 0700: these files name the ports and hosts you are exposing.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func statePath(dir string, pid int) string { return filepath.Join(dir, strconv.Itoa(pid)+".json") }

// logPathFor is where a detached agent's output goes. Named for the PID
// because that is the only handle that exists at the moment the file has to be
// opened — the tunnel has no name until the server has answered.
func logPathFor(pid int) (string, error) {
	dir, err := subDir("log")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strconv.Itoa(pid)+".log"), nil
}

// socketPathFor is where an agent serves its inspector over a private unix
// socket, which is how `hop inspect <name>` reaches a tunnel that was started
// without --inspect. Named for the PID for the same reason the log is.
func socketPathFor(pid int) (string, error) {
	dir, err := subDir("run")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, strconv.Itoa(pid)+".sock"), nil
}

// pruneRuntime deletes the files agents leave behind: the log of a detached
// run, and the unix socket its inspector listened on. Neither is removed by
// the agent itself in every case — a killed process removes nothing, and the
// kernel does not reap unix sockets — so without this the directories grow for
// the life of the machine.
//
// A file is swept only if no live agent owns its PID and it has not been
// touched inside its own window. The two windows are not the same length and
// are not measuring the same thing: a log is kept so it can still be read, so
// it is kept for as long as reading it is plausible, while a socket is kept
// only long enough that a starting agent cannot have its own socket swept
// between listening on it and recording itself.
func pruneRuntime(logAge, socketAge time.Duration) {
	alive := make(map[int]bool)
	live, _ := liveStates()
	for _, s := range live {
		alive[s.PID] = true
	}

	logs, err := subDir("log")
	if err == nil {
		pruneByPID(logs, ".log", alive, logAge)
	}
	// Sockets live in run/ alongside the state records, which are swept by
	// liveStates on their lock instead — a record's owner is provably gone or
	// provably alive, so age never comes into it.
	run, err := subDir("run")
	if err == nil {
		pruneByPID(run, ".sock", alive, socketAge)
	}
}

// pruneByPID removes <pid><ext> files in dir whose PID is not alive and whose
// mtime is older than maxAge.
func pruneByPID(dir, ext string, alive map[int]bool, maxAge time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ext) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSuffix(name, ext))
		if err != nil || alive[pid] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		os.Remove(filepath.Join(dir, name))
	}
}

// stateFile is a held claim on ~/.hop/run/<pid>.json. The lock lives on the
// open file, so it must stay open for the life of the agent.
type stateFile struct {
	f    *os.File
	path string
}

// holdState records s and keeps its file locked until the process exits.
func holdState(s State) (*stateFile, error) {
	dir, err := subDir("run")
	if err != nil {
		return nil, err
	}
	path := statePath(dir, s.PID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s is locked by another process: %w", path, err)
	}
	sf := &stateFile{f: f, path: path}
	if err := sf.write(s); err != nil {
		sf.release()
		return nil, err
	}
	return sf, nil
}

// write replaces the file's contents. Called again on every reconnect, since a
// server-assigned name can change if the old one was taken in the meantime.
func (sf *stateFile) write(s State) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if _, err := sf.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := sf.f.Truncate(0); err != nil {
		return err
	}
	_, err = sf.f.Write(b)
	return err
}

func (sf *stateFile) release() {
	sf.f.Close() // drops the lock
	os.Remove(sf.path)
}

// liveStates returns the agents running on this machine, newest name order,
// deleting the records of any that have died.
func liveStates() ([]State, error) {
	dir, err := subDir("run")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var live []State
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		s, held, err := readState(path)
		if err != nil {
			// Unparseable. That is either a file being written right now or
			// debris; either way, guessing which and deleting it is worse than
			// leaving it. A live agent overwrites its own file on reconnect.
			continue
		}
		if !held {
			os.Remove(path)
			// The agent's inspector socket outlives it the same way: unix
			// sockets are not cleaned up by the kernel, and a dead agent has
			// nobody to remove its file. Swept here rather than on exit
			// because this runs however the process died.
			if pid, err := strconv.Atoi(strings.TrimSuffix(e.Name(), ".json")); err == nil {
				if sock, err := socketPathFor(pid); err == nil {
					os.Remove(sock)
				}
			}
			continue
		}
		live = append(live, s)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Subdomain < live[j].Subdomain })
	return live, nil
}

// readState reads one record and reports whether its owner still holds the
// lock. Acquiring the lock proves the owner is gone, so it is released again
// immediately — a reader must never keep it.
func readState(path string) (s State, held bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return s, false, err
	}
	defer f.Close()

	held = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
	if !held {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}

	b, err := io.ReadAll(f)
	if err != nil {
		return s, held, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, held, err
	}
	return s, held, nil
}

// waitForState polls until the agent with this PID reports a tunnel, which is
// how the foreground half of `-d` learns the handshake worked. done fires if
// the child exits first.
func waitForState(pid int, done <-chan error, timeout time.Duration) (State, error) {
	dir, err := subDir("run")
	if err != nil {
		return State{}, err
	}
	path := statePath(dir, pid)

	deadline := time.After(timeout)
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			// The agent gave up before reporting: a refused token, a taken
			// name, a local port that isn't there. Its own message is in the
			// log, which the caller prints.
			if err == nil {
				err = fmt.Errorf("exited immediately")
			}
			return State{}, err
		case <-deadline:
			return State{}, fmt.Errorf("no tunnel after %s", timeout)
		case <-tick.C:
			s, held, err := readState(path)
			if err != nil || !held || s.Subdomain == "" {
				continue
			}
			return s, nil
		}
	}
}
