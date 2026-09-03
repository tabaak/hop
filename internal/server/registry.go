package server

import (
	"errors"
	"math/rand"
	"regexp"
	"sort"
	"sync"
	"time"
)

var (
	// ErrTaken means the requested subdomain is in use or reserved.
	ErrTaken = errors.New("subdomain is already in use")
	// ErrBadName means the requested subdomain isn't a legal DNS label.
	ErrBadName = errors.New("subdomain must be 1-32 chars of a-z, 0-9 or '-'")
)

// Names we never hand out, because they'll collide with the control plane or
// with things people reasonably expect to be ours.
var reserved = map[string]bool{
	"www": true, "api": true, "admin": true, "dashboard": true,
	"app": true, "static": true, "cdn": true, "mail": true,
	"hop": true, "tunnel": true,
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// entry is one claimed name. tunnel is nil in two quite different situations,
// and telling them apart is most of what the grace window is: between Reserve
// and Bind the claim is still being built, and after an unexpected drop it is
// a tombstone holding the name for its owner to come back to.
type entry struct {
	tunnel *Tunnel
	// owner is the token's label, not the token itself. Holding the secret in
	// server memory for the life of every tunnel bought nothing, and the label
	// is the more useful identity anyway: it survives a rotation of the secret
	// behind it.
	owner string
	gen   uint64
	// since is when the tunnel bound, not when the name was reserved. A claim
	// that never binds has no uptime to report, and a reconnect that takes the
	// name over starts its own clock.
	since time.Time
	// expires is set only on a tombstone: the moment the held name goes back
	// to the pool. Zero on a bound tunnel and on a claim still mid-handshake.
	expires time.Time
}

// leased reports whether e is a tombstone — a claim whose session went away
// without saying goodbye, holding its name for the owner to reclaim.
//
// The zero expires is what separates it from a claim between Reserve and Bind.
// That distinction matters: a half-built claim must refuse even its own owner,
// or two of this operator's agents starting at once can steal each other's
// names mid-handshake.
func (e *entry) leased() bool { return e.tunnel == nil && !e.expires.IsZero() }

// expired reports whether e is a tombstone whose window has passed. Such an
// entry is treated as absent by everything that asks, whether or not the timer
// that deletes it has fired yet — so the answer never depends on timer latency.
func (e *entry) expired(now time.Time) bool { return e.leased() && now.After(e.expires) }

// Registry maps a subdomain to its live tunnel.
//
// Claiming is two-phase. The handshake must tell the agent its URL on the raw
// connection *before* both sides upgrade to yamux, but the Tunnel doesn't exist
// until after that upgrade. Reserve takes the name so a racing agent loses
// immediately; Bind fills in the tunnel once the session is up. Lookup treats a
// reserved-but-unbound name as absent, so no request reaches a half-built tunnel.
//
// Every claim carries a generation number. Bind and Release only act when the
// generation still matches, which is what stops a slow-exiting predecessor from
// deleting the claim its own successor just made.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*entry
	nextGen uint64
}

func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]*entry)}
}

// Reserve claims want for owner, or allocates a random name if want is empty.
// owner is a token label, so every device holding the same label is the same
// owner and separate labels are separate owners.
//
// If want is held by a tunnel belonging to the same owner, that tunnel is
// evicted and returned so the caller can close it. This is the reconnect path:
// after a dropped connection the server may not have reaped the dead session
// yet, and the owner shouldn't have to wait ~45s for keepalive to notice.
func (r *Registry) Reserve(want, owner string) (sub string, gen uint64, evicted *Tunnel, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	if want == "" {
		for i := 0; i < 100; i++ {
			c := randomName()
			// An expired tombstone is free even though its entry is still in
			// the map: the timer that removes it has no bearing on whether the
			// name is available, and a pool that shrank every time a tunnel
			// dropped would run dry on a long-lived server.
			if e, exists := r.entries[c]; !exists || e.expired(now) {
				want = c
				break
			}
		}
		if want == "" {
			return "", 0, nil, errors.New("could not allocate a free name")
		}
	} else {
		if !nameRe.MatchString(want) {
			return "", 0, nil, ErrBadName
		}
		if reserved[want] {
			return "", 0, nil, ErrTaken
		}
		if e, exists := r.entries[want]; exists && !e.expired(now) {
			switch {
			case e.owner != owner:
				// Someone else's, live or held. The lease is what makes a
				// dropped tunnel's name un-stealable for the window, which is
				// the point of the whole exercise.
				return "", 0, nil, ErrTaken
			case e.tunnel != nil:
				// Our own live session. The server hasn't reaped it yet, so
				// hand the name over and let the caller close it.
				evicted = e.tunnel
			case e.leased():
				// Our own tombstone: the reconnect this window exists for.
				// Nothing to evict — the session it belonged to is gone.
			default:
				// Our own claim, still between Reserve and Bind. Refused even
				// though the owner matches: two agents of ours racing must not
				// be able to take each other's half-built claims.
				return "", 0, nil, ErrTaken
			}
		}
	}

	r.nextGen++
	r.entries[want] = &entry{owner: owner, gen: r.nextGen}
	return want, r.nextGen, evicted, nil
}

// Bind attaches a live tunnel to a claim, if that claim is still current.
func (r *Registry) Bind(sub string, gen uint64, t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[sub]; ok && e.gen == gen {
		e.tunnel = t
		e.since = time.Now()
	}
}

// Release drops a claim, if it's still current. Safe to call for a claim that
// was never bound, and safe to call after being evicted by a successor.
func (r *Registry) Release(sub string, gen uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[sub]; ok && e.gen == gen {
		delete(r.entries, sub)
	}
}

// Lease turns a bound claim into a tombstone: the tunnel is dropped but the
// name stays with its owner until d has passed. It reports whether it acted,
// which is false for a claim some successor has already taken over.
//
// This is what a dropped tunnel leaves behind, so the URL a webhook was
// registered against still belongs to the same agent when it reconnects a few
// seconds later.
//
// Expiry is a timer rather than a swept queue. The generation guard that
// already protects Release from a slow-exiting predecessor protects it from a
// stale timer for free: a successor that reclaimed the name holds a newer
// generation, so the old timer finds a mismatch and does nothing. A goroutine
// scanning the map would be a third party that guard doesn't cover.
func (r *Registry) Lease(sub string, gen uint64, d time.Duration) bool {
	r.mu.Lock()
	e, ok := r.entries[sub]
	if !ok || e.gen != gen {
		r.mu.Unlock()
		return false
	}
	e.tunnel = nil
	e.expires = time.Now().Add(d)
	r.mu.Unlock()

	time.AfterFunc(d, func() { r.Release(sub, gen) })
	return true
}

// Leased reports whether sub is held for an owner who isn't currently
// connected, and how long is left. Requests for such a name are answered
// differently from requests for a name nobody has: it is coming back.
func (r *Registry) Leased(sub string) (time.Duration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[sub]
	if !ok || !e.leased() {
		return 0, false
	}
	left := time.Until(e.expires)
	if left <= 0 {
		return 0, false
	}
	return left, true
}

// Lookup returns the live tunnel for sub, if one is serving.
func (r *Registry) Lookup(sub string) (*Tunnel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[sub]
	if !ok || e.tunnel == nil {
		return nil, false
	}
	return e.tunnel, true
}

// CloseRevoked drops every live tunnel whose owner is absent from keep, and
// returns the names it closed.
//
// This is what makes deleting a line from the tokens file mean something. A
// revocation that only stopped *future* connections would leave a stolen
// credential serving traffic for as long as its holder cared to keep the
// socket open, which is the opposite of what you want in the minute after
// discovering it leaked.
//
// Rotation is deliberately not affected: a new secret under the same label is
// the same owner, so that tunnel stays up.
func (r *Registry) CloseRevoked(keep map[string]bool) []string {
	r.mu.Lock()
	var (
		doomed []*Tunnel
		names  []string
	)
	for sub, e := range r.entries {
		if keep[e.owner] {
			continue
		}
		if e.tunnel != nil {
			doomed = append(doomed, e.tunnel)
			names = append(names, sub)
		}
		// Deleted here, before the close below rather than after it. Closing a
		// tunnel wakes the goroutine serving it, and that goroutine's last act
		// is to lease the name for the grace window — which would hand a
		// revoked credential its subdomain back for another 45s. Lease is
		// generation-guarded against an entry that is gone, so removing the
		// claim first is what makes the revocation stick. A held name goes the
		// same way: there is no session to close, and nothing to wait for.
		delete(r.entries, sub)
	}
	r.mu.Unlock()

	// Closed outside the lock. Each close wakes the goroutine serving that
	// tunnel, which takes the write lock on its way out — holding the lock
	// here would deadlock against it.
	for _, t := range doomed {
		t.Close()
	}
	return names
}

// Live is one serving tunnel, as reported to `hop ps`.
type Live struct {
	Sub   string
	Owner string
	Local string
	Since time.Time
}

// Snapshot lists every tunnel currently serving, sorted by name.
//
// Reserved-but-unbound claims are left out, matching Lookup: a name that
// nothing can be routed to yet would be a confusing thing to see in a list of
// what is up.
//
// Sorted here rather than at the caller so the order is stable across calls —
// map iteration would reshuffle the table on every invocation and make a
// changed row hard to spot.
func (r *Registry) Snapshot() []Live {
	r.mu.RLock()
	out := make([]Live, 0, len(r.entries))
	for sub, e := range r.entries {
		if e.tunnel != nil {
			out = append(out, Live{Sub: sub, Owner: e.owner, Local: e.tunnel.Local, Since: e.since})
		}
	}
	r.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool { return out[i].Sub < out[j].Sub })
	return out
}

// Count returns the number of live tunnels.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, e := range r.entries {
		if e.tunnel != nil {
			n++
		}
	}
	return n
}

var (
	adjectives = []string{
		"brave", "quiet", "swift", "amber", "clever", "lucky", "sunny",
		"wild", "calm", "bold", "eager", "gentle", "keen", "merry",
	}
	nouns = []string{
		"otter", "lynx", "heron", "falcon", "badger", "marten", "ibex",
		"raven", "fennec", "tapir", "gecko", "puffin", "shrike", "vole",
	}
)

func randomName() string {
	return adjectives[rand.Intn(len(adjectives))] + "-" + nouns[rand.Intn(len(nouns))]
}
