package main

import (
	"math/rand"
	"time"
)

const (
	// baseBackoff is the width of the window after the first failure, not the
	// wait itself: full jitter draws from inside it, so the first retry is
	// somewhere in the next second rather than exactly a second away.
	baseBackoff = time.Second
	// maxBackoff caps the window. Half of it is the average wait once a server
	// has been down a while, which is about as long as anyone will sit and
	// watch before reaching for the logs anyway.
	maxBackoff = 30 * time.Second
	// maxAttempt caps the exponent. baseBackoff<<5 is 32s, already past
	// maxBackoff, so no higher attempt changes the answer — and an uncapped
	// shift is a real bug rather than a wasted branch: at attempt 34 the
	// duration overflows int64 into a negative number and rand.Int63n panics
	// on it. An agent left running through a long outage gets there.
	maxAttempt = 5
	// healthy is how long a session with no traffic on it has to last before it
	// counts as having worked.
	healthy = 30 * time.Second
)

// healthySession reports whether a session that has just ended earned a reset
// of the backoff, so an overnight tunnel doesn't crawl after one blip.
//
// Both halves are needed. Elapsed time alone believes a server that accepts
// connections and then ignores them — long-lived and useless. Traffic alone
// punishes an idle webhook endpoint that sat there correctly for six hours
// without being called, which is the ordinary state of the thing this tool
// exists to serve.
//
// elapsed is measured with a monotonic clock, which on macOS does not advance
// while the machine is asleep. That is the behaviour we want and not a wart to
// work around: a session that spanned a closed lid is judged on the part of it
// that was awake, which is the only part that says anything about health.
func healthySession(served bool, elapsed time.Duration) bool {
	return served || elapsed > healthy
}

// nextBackoff returns how long to wait before reconnect attempt n, counting
// from zero.
//
// The wait is a uniform draw from [0, window), where window doubles per
// attempt up to maxBackoff — "full jitter". Plain exponential backoff is the
// wrong shape for this: every agent that lost the same server lost it at the
// same instant, so they all wake together and arrive together, and the retry
// storm is what keeps the server down. Drawing from the whole window spreads
// them across it, and costs nothing when there is only one agent.
//
// Sleeping for nearly zero on an early attempt is deliberate, not a rounding
// accident. A Wi-Fi switch is over in a moment, and the agent that happens to
// draw a short wait is back before anyone notices the tunnel was gone.
func nextBackoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > maxAttempt {
		attempt = maxAttempt
	}
	window := baseBackoff << attempt
	if window > maxBackoff {
		window = maxBackoff
	}
	return time.Duration(rand.Int63n(int64(window)))
}
