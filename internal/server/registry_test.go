package server

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"hop.vokh.dev/internal/tunnel"
)

// liveTunnel returns a Tunnel with a real session behind it, over an in-memory
// pipe. Most tests here only compare tunnel pointers and can use a bare
// &Tunnel{}; anything that closes one needs a session to close.
func liveTunnel(t *testing.T, sub string) *Tunnel {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })

	sess, err := yamux.Client(a, tunnel.Config())
	if err != nil {
		t.Fatal(err)
	}
	// The far end, standing in for the agent.
	peer, err := yamux.Server(b, tunnel.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })

	return NewTunnel(sub, "127.0.0.1:3000", sess, "https")
}

func TestReserveRejectsBadAndReservedNames(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"Bad_Name", "-lead", "trail-", "has.dot", "", "x"} {
		if name == "" {
			continue // empty means "assign one", covered elsewhere
		}
		if _, _, _, err := r.Reserve(name, "laptop"); err == nil && name != "x" {
			t.Errorf("Reserve(%q) = nil error, want rejection", name)
		}
	}
	if _, _, _, err := r.Reserve("admin", "laptop"); !errors.Is(err, ErrTaken) {
		t.Errorf("Reserve(admin) err = %v, want ErrTaken", err)
	}
}

func TestReserveIsExclusiveAcrossOwners(t *testing.T) {
	r := NewRegistry()
	sub, gen, _, err := r.Reserve("myapp", "laptop")
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	r.Bind(sub, gen, &Tunnel{Sub: sub})

	if _, _, _, err := r.Reserve("myapp", "phone"); !errors.Is(err, ErrTaken) {
		t.Fatalf("cross-owner Reserve err = %v, want ErrTaken", err)
	}
}

func TestReserveEvictsOwnSessionOnReconnect(t *testing.T) {
	r := NewRegistry()
	sub, gen, _, err := r.Reserve("myapp", "laptop")
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	first := &Tunnel{Sub: sub}
	r.Bind(sub, gen, first)

	_, gen2, evicted, err := r.Reserve("myapp", "laptop")
	if err != nil {
		t.Fatalf("reconnect Reserve: %v", err)
	}
	if evicted != first {
		t.Fatalf("evicted = %v, want the first tunnel", evicted)
	}
	if gen2 == gen {
		t.Fatal("reconnect reused the previous generation")
	}
	// The name is claimed but not yet bound, so nothing should be routable.
	if _, ok := r.Lookup("myapp"); ok {
		t.Fatal("Lookup found an unbound claim")
	}
}

// The predecessor's handler always runs Release after being evicted. It must
// not delete the successor's claim.
func TestReleaseByEvictedPredecessorIsNoop(t *testing.T) {
	r := NewRegistry()
	sub, gen1, _, _ := r.Reserve("myapp", "laptop")
	r.Bind(sub, gen1, &Tunnel{Sub: sub})

	_, gen2, _, _ := r.Reserve("myapp", "laptop")
	successor := &Tunnel{Sub: sub}
	r.Bind(sub, gen2, successor)

	r.Release(sub, gen1) // predecessor finally exits

	got, ok := r.Lookup("myapp")
	if !ok || got != successor {
		t.Fatalf("Lookup after stale Release = (%v, %v), want the successor", got, ok)
	}
}

func TestReleaseFreesTheName(t *testing.T) {
	r := NewRegistry()
	sub, gen, _, _ := r.Reserve("myapp", "laptop")
	r.Bind(sub, gen, &Tunnel{Sub: sub})
	r.Release(sub, gen)

	if _, ok := r.Lookup("myapp"); ok {
		t.Fatal("name still routable after Release")
	}
	if _, _, _, err := r.Reserve("myapp", "someone-else"); err != nil {
		t.Fatalf("Reserve after Release: %v", err)
	}
}

func TestReserveEmptyAssignsDistinctNames(t *testing.T) {
	r := NewRegistry()
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		sub, gen, _, err := r.Reserve("", "laptop")
		if err != nil {
			t.Fatalf("Reserve(\"\"): %v", err)
		}
		if seen[sub] {
			t.Fatalf("Reserve(\"\") handed out %q twice", sub)
		}
		seen[sub] = true
		r.Bind(sub, gen, &Tunnel{Sub: sub})
	}
	if got := r.Count(); got != 20 {
		t.Errorf("Count() = %d, want 20", got)
	}
}

func TestCountIgnoresUnboundClaims(t *testing.T) {
	r := NewRegistry()
	r.Reserve("pending", "laptop")
	if got := r.Count(); got != 0 {
		t.Errorf("Count() = %d, want 0 for an unbound claim", got)
	}
}

func TestCloseRevokedDropsOnlyRevokedOwners(t *testing.T) {
	r := NewRegistry()

	// Two devices, two names. Only one device's credential is revoked.
	mine, gen1, _, _ := r.Reserve("mine", "laptop")
	r.Bind(mine, gen1, liveTunnel(t, mine))
	theirs, gen2, _, _ := r.Reserve("theirs", "phone")
	r.Bind(theirs, gen2, liveTunnel(t, theirs))

	// A name reserved but not yet bound has no session to close, and must not
	// be reported as one.
	r.Reserve("pending", "laptop")

	closed := r.CloseRevoked(map[string]bool{"phone": true})

	if len(closed) != 1 || closed[0] != "mine" {
		t.Fatalf("CloseRevoked closed %v, want [mine]", closed)
	}
}

func TestCloseRevokedKeepsEveryoneWhenNothingChanged(t *testing.T) {
	r := NewRegistry()
	sub, gen, _, _ := r.Reserve("myapp", "laptop")
	r.Bind(sub, gen, liveTunnel(t, sub))

	// Rotating a secret under the same label reloads the file but must not
	// disturb the tunnel that label is holding.
	if closed := r.CloseRevoked(map[string]bool{"laptop": true}); len(closed) != 0 {
		t.Fatalf("CloseRevoked closed %v on an unchanged owner set", closed)
	}
	if _, ok := r.Lookup("myapp"); !ok {
		t.Error("tunnel went away despite its owner still being accepted")
	}
}

// Snapshot backs `hop ps`. A reserved-but-unbound claim is deliberately absent:
// it exists for the few milliseconds between the ack and the yamux upgrade, and
// nothing can be routed to it yet.
func TestSnapshotListsOnlyBoundTunnels(t *testing.T) {
	r := NewRegistry()

	for _, sub := range []string{"zeta", "alpha"} {
		_, gen, _, err := r.Reserve(sub, "laptop")
		if err != nil {
			t.Fatal(err)
		}
		r.Bind(sub, gen, liveTunnel(t, sub))
	}
	if _, _, _, err := r.Reserve("pending", "phone"); err != nil {
		t.Fatal(err)
	}

	got := r.Snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot = %+v, want 2 entries", got)
	}
	// Sorted, so the table doesn't reshuffle between invocations.
	if got[0].Sub != "alpha" || got[1].Sub != "zeta" {
		t.Errorf("order = %q, %q; want alpha, zeta", got[0].Sub, got[1].Sub)
	}
	if got[0].Owner != "laptop" {
		t.Errorf("owner = %q, want %q", got[0].Owner, "laptop")
	}
	if got[0].Since.IsZero() {
		t.Error("Since is zero; uptime would be reported as decades")
	}
}

// Uptime is measured from the bind, not the reserve, so a takeover reports how
// long the *current* session has been serving rather than how long the name has
// been held.
func TestSnapshotUptimeRestartsOnTakeover(t *testing.T) {
	r := NewRegistry()

	_, gen, _, err := r.Reserve("myapp", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	r.Bind("myapp", gen, liveTunnel(t, "myapp"))
	first := r.Snapshot()[0].Since

	// Long enough that the two timestamps differ on any clock granularity,
	// short enough not to be felt.
	time.Sleep(time.Millisecond)

	_, gen2, evicted, err := r.Reserve("myapp", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if evicted == nil {
		t.Fatal("want the previous tunnel handed back for closing")
	}
	r.Bind("myapp", gen2, liveTunnel(t, "myapp"))

	if second := r.Snapshot()[0].Since; !second.After(first) {
		t.Errorf("Since = %v after takeover, want later than %v", second, first)
	}
}
