package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Every test here writes under ~/.hop, so HOME is redirected to a temp dir.
// t.Setenv also fails the test if it is ever run in parallel, which is the
// behaviour we want: a shared state directory would make these flaky.
func tempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

func TestHoldStateRecordsAndReleases(t *testing.T) {
	tempHome(t)

	sf, err := holdState(State{PID: os.Getpid(), Local: "127.0.0.1:3000", Server: "example:7443"})
	if err != nil {
		t.Fatalf("holdState: %v", err)
	}

	live, err := liveStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].Local != "127.0.0.1:3000" {
		t.Fatalf("liveStates = %+v, want the record just written", live)
	}

	sf.release()
	if live, _ = liveStates(); len(live) != 0 {
		t.Fatalf("liveStates = %+v after release, want none", live)
	}
}

// The record is rewritten when the tunnel comes up, since the name is not known
// until the server has answered.
func TestStateGainsItsNameOnUp(t *testing.T) {
	tempHome(t)

	s := State{PID: os.Getpid(), Local: "127.0.0.1:3000"}
	sf, err := holdState(s)
	if err != nil {
		t.Fatal(err)
	}
	defer sf.release()

	if live, _ := liveStates(); live[0].Subdomain != "" {
		t.Fatalf("subdomain = %q before the tunnel is up, want empty", live[0].Subdomain)
	}

	s.Subdomain, s.URL = "myapp", "https://myapp.example"
	if err := sf.write(s); err != nil {
		t.Fatal(err)
	}

	live, _ := liveStates()
	if live[0].Subdomain != "myapp" || live[0].URL != "https://myapp.example" {
		t.Fatalf("state = %+v, want the assigned name and URL", live[0])
	}
}

// A rewrite must not leave the tail of the longer previous record behind, which
// would make the file unparseable and the tunnel invisible.
func TestStateRewriteTruncates(t *testing.T) {
	tempHome(t)

	sf, err := holdState(State{PID: os.Getpid(), Subdomain: "a-very-long-subdomain-name", Local: "127.0.0.1:3000"})
	if err != nil {
		t.Fatal(err)
	}
	defer sf.release()

	if err := sf.write(State{PID: os.Getpid(), Subdomain: "x", Local: "127.0.0.1:3000"}); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(sf.path)
	if err != nil {
		t.Fatal(err)
	}
	var got State
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("state file is not valid JSON after a shorter rewrite: %v (%s)", err, b)
	}
	if got.Subdomain != "x" {
		t.Errorf("subdomain = %q, want %q", got.Subdomain, "x")
	}
}

// The record of a process that died without cleaning up is swept on the next
// read. This is the case a PID check gets wrong once the number is recycled;
// the lock cannot be, because the kernel drops it with the process.
func TestLiveStatesSweepsRecordsOfDeadProcesses(t *testing.T) {
	home := tempHome(t)

	// Written by a real process that then exits, so the file is left behind
	// exactly as a killed agent would leave it.
	holder := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsState$")
	holder.Env = append(os.Environ(), "HOP_TEST_HOLDER=1", "HOME="+home)
	if out, err := holder.CombinedOutput(); err != nil {
		t.Fatalf("holder: %v\n%s", err, out)
	}

	path := filepath.Join(home, ".hop", "run", "424242.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("holder did not leave a record: %v", err)
	}

	live, err := liveStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("liveStates = %+v, want the dead process's record ignored", live)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stale record still on disk: %v", err)
	}
}

// TestHelperHoldsState is not a test: it is the subprocess above, writing a
// record and exiting without releasing it.
func TestHelperHoldsState(t *testing.T) {
	if os.Getenv("HOP_TEST_HOLDER") == "" {
		t.Skip("helper for TestLiveStatesSweepsRecordsOfDeadProcesses")
	}
	if _, err := holdState(State{PID: 424242, Subdomain: "ghost", Local: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	// Deliberately no release: the point is to die still holding it.
}

// A record whose owner is alive must survive, or `hop stop` would lose track of
// a running tunnel.
func TestLiveStatesKeepsHeldRecords(t *testing.T) {
	home := tempHome(t)

	holder := exec.Command(os.Args[0], "-test.run=^TestHelperHoldsStateAndWaits$")
	holder.Env = append(os.Environ(), "HOP_TEST_HOLDER=1", "HOME="+home)
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		holder.Process.Kill()
		holder.Wait()
	}()

	// The subprocess needs a moment to write its record.
	var live []State
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		live, _ = liveStates()
		if len(live) == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(live) != 1 || live[0].Subdomain != "held" {
		t.Fatalf("liveStates = %+v, want the running holder's record", live)
	}
	if live[0].PID != holder.Process.Pid {
		t.Errorf("pid = %d, want the holder's %d", live[0].PID, holder.Process.Pid)
	}
}

func TestHelperHoldsStateAndWaits(t *testing.T) {
	if os.Getenv("HOP_TEST_HOLDER") == "" {
		t.Skip("helper for TestLiveStatesKeepsHeldRecords")
	}
	if _, err := holdState(State{PID: os.Getpid(), Subdomain: "held", Local: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second) // killed by the parent
}

// waitForState is how `-d` learns the handshake worked, so it must not report
// success until the name is there — a record exists from the moment the agent
// starts, several dials before it means anything.
func TestWaitForStateWaitsForTheName(t *testing.T) {
	tempHome(t)

	pid := os.Getpid()
	sf, err := holdState(State{PID: pid, Local: "127.0.0.1:3000"})
	if err != nil {
		t.Fatal(err)
	}
	defer sf.release()

	go func() {
		time.Sleep(150 * time.Millisecond)
		sf.write(State{PID: pid, Subdomain: "late", URL: "https://late.example", Local: "127.0.0.1:3000"})
	}()

	got, err := waitForState(pid, make(chan error), 5*time.Second)
	if err != nil {
		t.Fatalf("waitForState: %v", err)
	}
	if got.Subdomain != "late" {
		t.Errorf("subdomain = %q, want %q", got.Subdomain, "late")
	}
}

// An agent that dies during startup must fail the parent immediately rather
// than making it wait out the timeout.
func TestWaitForStateFailsWhenTheChildExits(t *testing.T) {
	tempHome(t)

	done := make(chan error, 1)
	done <- exec.Command("false").Run()

	start := time.Now()
	if _, err := waitForState(999999, done, 30*time.Second); err == nil {
		t.Fatal("waitForState returned success for a child that exited")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("waitForState waited for the timeout instead of noticing the exit")
	}
}

func TestWaitForStateTimesOut(t *testing.T) {
	tempHome(t)

	start := time.Now()
	_, err := waitForState(999999, make(chan error), 200*time.Millisecond)
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s, want roughly the 200ms timeout", d)
	}
}

// findState accepts a PID as well as a name, which is the only handle a tunnel
// has before the server has assigned it one.
func TestFindState(t *testing.T) {
	live := []State{
		{PID: 11, Subdomain: "myapp"},
		{PID: 22, Subdomain: ""},
	}
	if s, ok := findState(live, "myapp"); !ok || s.PID != 11 {
		t.Errorf("by name = (%+v, %v), want pid 11", s, ok)
	}
	if s, ok := findState(live, strconv.Itoa(22)); !ok || s.PID != 22 {
		t.Errorf("by pid = (%+v, %v), want pid 22", s, ok)
	}
	if _, ok := findState(live, "nope"); ok {
		t.Error("found a tunnel that isn't running")
	}
}
