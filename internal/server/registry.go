package server

import (
	"errors"
	"math/rand"
	"regexp"
	"sync"
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

// entry is one claimed name. tunnel is nil between Reserve and Bind.
type entry struct {
	tunnel *Tunnel
	token  string
	gen    uint64
}

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

// Reserve claims want for token, or allocates a random name if want is empty.
//
// If want is held by a tunnel belonging to the same token, that tunnel is
// evicted and returned so the caller can close it. This is the reconnect path:
// after a dropped connection the server may not have reaped the dead session
// yet, and the owner shouldn't have to wait ~45s for keepalive to notice.
func (r *Registry) Reserve(want, token string) (sub string, gen uint64, evicted *Tunnel, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if want == "" {
		for i := 0; i < 100; i++ {
			c := randomName()
			if _, exists := r.entries[c]; !exists {
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
		if e, exists := r.entries[want]; exists {
			if e.tunnel == nil || e.token != token {
				return "", 0, nil, ErrTaken
			}
			evicted = e.tunnel
		}
	}

	r.nextGen++
	r.entries[want] = &entry{token: token, gen: r.nextGen}
	return want, r.nextGen, evicted, nil
}

// Bind attaches a live tunnel to a claim, if that claim is still current.
func (r *Registry) Bind(sub string, gen uint64, t *Tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[sub]; ok && e.gen == gen {
		e.tunnel = t
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
