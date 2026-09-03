package main

import (
	"testing"
	"time"
)

// The wait is random, so the properties are what can be asserted: it never
// exceeds the window for its attempt, and the window is the doubling sequence
// capped at maxBackoff. Enough draws that a bound violated one time in a
// hundred would still be caught.
func TestBackoffStaysWithinItsWindow(t *testing.T) {
	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second, // 32s, capped
	}
	for attempt, window := range want {
		for i := 0; i < 2000; i++ {
			got := nextBackoff(attempt)
			if got < 0 || got >= window {
				t.Fatalf("nextBackoff(%d) = %v, want within [0, %v)", attempt, got, window)
			}
		}
	}
}

// Full jitter is only doing its job if the draws actually spread out. A
// version that returned the window itself, or always something tiny, would
// pass the bounds test above and still reconnect the whole fleet in lockstep.
func TestBackoffSpreadsAcrossTheWindow(t *testing.T) {
	const attempt = 3 // an 8s window
	var low, high int
	for i := 0; i < 2000; i++ {
		if nextBackoff(attempt) < 4*time.Second {
			low++
		} else {
			high++
		}
	}
	if low < 700 || high < 700 {
		t.Fatalf("draws not spread across the window: %d below the midpoint, %d above", low, high)
	}
}

// The cap exists because the shift overflows, not because a longer wait would
// be unreasonable: at attempt 34 the duration goes negative and rand.Int63n
// panics on a non-positive bound. An agent left running through a long outage
// reaches those numbers, so they are tested rather than assumed unreachable.
func TestBackoffSurvivesAbsurdAttemptCounts(t *testing.T) {
	for _, attempt := range []int{maxAttempt + 1, 34, 63, 64, 1 << 20} {
		got := nextBackoff(attempt)
		if got < 0 || got >= maxBackoff {
			t.Fatalf("nextBackoff(%d) = %v, want within [0, %v)", attempt, got, maxBackoff)
		}
	}
}

// A negative attempt is not something the loop can produce, but nextBackoff is
// the kind of function that gets called from somewhere else later.
func TestBackoffTreatsNegativeAttemptAsFirst(t *testing.T) {
	for i := 0; i < 100; i++ {
		if got := nextBackoff(-1); got < 0 || got >= baseBackoff {
			t.Fatalf("nextBackoff(-1) = %v, want within [0, %v)", got, baseBackoff)
		}
	}
}

func TestHealthySessionResetRule(t *testing.T) {
	cases := []struct {
		name    string
		served  bool
		elapsed time.Duration
		want    bool
	}{
		{"served immediately", true, time.Millisecond, true},
		{"idle but long-lived", false, healthy + time.Second, true},
		{"idle and brief", false, time.Second, false},
		{"idle, exactly at the threshold", false, healthy, false},
		{"refused before anything came up", false, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := healthySession(c.served, c.elapsed); got != c.want {
				t.Fatalf("healthySession(%v, %v) = %v, want %v", c.served, c.elapsed, got, c.want)
			}
		})
	}
}

// The reset rule and the backoff meet in runHTTP's loop, which can't be run
// here; this walks the same arithmetic to show the sequence a caller gets:
// windows widen while drops keep coming, and one good session puts it back.
func TestBackoffSequenceAcrossDropsAndRecovery(t *testing.T) {
	attempt := 0
	next := func(served bool, elapsed time.Duration) time.Duration {
		if healthySession(served, elapsed) {
			attempt = 0
		}
		d := nextBackoff(attempt)
		if attempt < maxAttempt {
			attempt++
		}
		return d
	}

	// Four failures that never came up: windows widen.
	for i, window := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
		if d := next(false, 0); d >= window {
			t.Fatalf("drop %d: waited %v, want under %v", i, d, window)
		}
	}
	// A session that served traffic, however briefly, puts it back to the start.
	if d := next(true, time.Second); d >= time.Second {
		t.Fatalf("after a healthy session: waited %v, want under %v", d, time.Second)
	}
	if attempt != 1 {
		t.Fatalf("attempt = %d after a reset and one increment, want 1", attempt)
	}
}
