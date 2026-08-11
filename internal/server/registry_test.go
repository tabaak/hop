package server

import (
	"errors"
	"testing"
)

func TestReserveRejectsBadAndReservedNames(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"Bad_Name", "-lead", "trail-", "has.dot", "", "x"} {
		if name == "" {
			continue // empty means "assign one", covered elsewhere
		}
		if _, _, _, err := r.Reserve(name, "tok"); err == nil && name != "x" {
			t.Errorf("Reserve(%q) = nil error, want rejection", name)
		}
	}
	if _, _, _, err := r.Reserve("admin", "tok"); !errors.Is(err, ErrTaken) {
		t.Errorf("Reserve(admin) err = %v, want ErrTaken", err)
	}
}

func TestReserveIsExclusiveAcrossTokens(t *testing.T) {
	r := NewRegistry()
	sub, gen, _, err := r.Reserve("myapp", "tok-a")
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	r.Bind(sub, gen, &Tunnel{Sub: sub})

	if _, _, _, err := r.Reserve("myapp", "tok-b"); !errors.Is(err, ErrTaken) {
		t.Fatalf("cross-token Reserve err = %v, want ErrTaken", err)
	}
}

func TestReserveEvictsOwnSessionOnReconnect(t *testing.T) {
	r := NewRegistry()
	sub, gen, _, err := r.Reserve("myapp", "tok")
	if err != nil {
		t.Fatalf("first Reserve: %v", err)
	}
	first := &Tunnel{Sub: sub}
	r.Bind(sub, gen, first)

	_, gen2, evicted, err := r.Reserve("myapp", "tok")
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
	sub, gen1, _, _ := r.Reserve("myapp", "tok")
	r.Bind(sub, gen1, &Tunnel{Sub: sub})

	_, gen2, _, _ := r.Reserve("myapp", "tok")
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
	sub, gen, _, _ := r.Reserve("myapp", "tok")
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
		sub, gen, _, err := r.Reserve("", "tok")
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
	r.Reserve("pending", "tok")
	if got := r.Count(); got != 0 {
		t.Errorf("Count() = %d, want 0 for an unbound claim", got)
	}
}
