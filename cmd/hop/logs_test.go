package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrintTailShowsTheLastLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, end := captureTail(t, f, 2)
	if want := "three\nfour\n"; got != want {
		t.Errorf("tail = %q, want %q", got, want)
	}
	// -f resumes from here, so it must be the end of the file rather than the
	// end of what was printed — otherwise following would replay old lines.
	if end != 19 {
		t.Errorf("end = %d, want the file size 19", end)
	}
}

// A log longer than the read window is truncated from the front, and the
// fragment of a line at the cut must not be printed as though it were one.
func TestPrintTailDropsThePartialFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	line := strings.Repeat("x", 99) + "\n"
	var b strings.Builder
	for b.Len() < maxTailBytes+5000 {
		b.WriteString(line)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	got, _ := captureTail(t, f, 0)
	for i, l := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if len(l) != 99 {
			t.Fatalf("line %d is %d chars, want whole lines only", i, len(l))
		}
	}
}

func TestPrintTailHandlesAnEmptyLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if got, _ := captureTail(t, f, 10); strings.TrimSpace(got) != "" {
		t.Errorf("tail of an empty log = %q, want nothing", got)
	}
}

// Logs are never cleaned up by anything else, so a start prunes the old ones —
// but only for agents that have finished.
func TestPruneLogsKeepsLiveAndRecentOnes(t *testing.T) {
	home := tempHome(t)

	sf, err := holdState(State{PID: os.Getpid(), Subdomain: "live"})
	if err != nil {
		t.Fatal(err)
	}
	defer sf.release()

	dir, err := subDir("log")
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// This machine's own PID stands in for a running agent.
	mine := write(filepath.Base(mustLogPath(t, os.Getpid())), 90*24*time.Hour)
	old := write("424242.log", 30*24*time.Hour)
	recent := write("424243.log", time.Hour)

	pruneLogs(logRetention)

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old log of a finished agent survived: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Errorf("recent log was pruned: %v", err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("log of a running agent was pruned despite its age: %v", err)
	}
	_ = home
}

func TestPastLogsListsOnlyFinishedAgents(t *testing.T) {
	tempHome(t)

	sf, err := holdState(State{PID: os.Getpid(), Subdomain: "live"})
	if err != nil {
		t.Fatal(err)
	}
	defer sf.release()

	dir, _ := subDir("log")
	for _, name := range []string{"1001.log", "1002.log", filepath.Base(mustLogPath(t, os.Getpid()))} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	live, _ := liveStates()
	got := pastLogs(live)
	if len(got) != 2 {
		t.Fatalf("pastLogs = %+v, want the two finished agents only", got)
	}
	for _, p := range got {
		if p.pid == os.Getpid() {
			t.Errorf("pastLogs included the running agent (pid %d)", p.pid)
		}
	}
}

func mustLogPath(t *testing.T, pid int) string {
	t.Helper()
	p, err := logPathFor(pid)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// captureTail runs printTail with stdout redirected to a pipe.
func captureTail(t *testing.T, f *os.File, n int) (string, int64) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()

	end, err := printTail(f, n)
	w.Close()
	os.Stdout = saved
	if err != nil {
		t.Fatal(err)
	}
	return <-done, end
}

// The log file is plain text, so colour is re-applied when it is displayed.
// Only the method and status are touched; the padding between them has to
// survive untouched or the columns shift.
func TestRenderColoursMethodAndStatus(t *testing.T) {
	colour = true
	defer func() { colour = false }()

	in := "  GET    200      1ms  Mac Chrome      /"
	got := render(in)

	if !strings.Contains(got, green+"GET"+reset) {
		t.Errorf("method not coloured: %q", got)
	}
	if !strings.Contains(got, green+"200"+reset) {
		t.Errorf("status not coloured: %q", got)
	}
	if stripColour(got) != in {
		t.Errorf("stripped = %q, want the original line %q", stripColour(got), in)
	}
}

func TestRenderLeavesOtherLinesAlone(t *testing.T) {
	colour = true
	defer func() { colour = false }()

	for _, line := range []string{
		"",
		"  http://myapp.hop.vokh.dev  →  http://127.0.0.1:3000",
		"hop: dial hop.vokh.dev:7443: connection refused — reconnecting in 1s",
		"  GET /not-a-log-line",
	} {
		if got := render(line); got != line {
			t.Errorf("render(%q) = %q, want it unchanged", line, got)
		}
	}
}

// An unparseable status is logged as "---", which must not be read as a code.
func TestRenderHandlesMissingStatus(t *testing.T) {
	colour = true
	defer func() { colour = false }()

	in := "  ?      ---      2ms  -               ?"
	got := render(in)
	if !strings.Contains(got, dim+"---"+reset) {
		t.Errorf("missing status not dimmed: %q", got)
	}
	if stripColour(got) != in {
		t.Errorf("stripped = %q, want %q", stripColour(got), in)
	}
}

// Nothing is coloured when the output isn't a terminal, so a piped or
// redirected log stays plain.
func TestRenderIsPlainWithoutColour(t *testing.T) {
	colour = false
	in := "  GET    200      1ms  curl            /"
	if got := render(in); got != in {
		t.Errorf("render = %q, want the line unchanged", got)
	}
}

func stripColour(s string) string {
	for _, code := range []string{reset, dim, red, green, yellow, blue, magenta, cyan} {
		s = strings.ReplaceAll(s, code, "")
	}
	return s
}
