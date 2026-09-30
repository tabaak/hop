// Package tokens holds the set of agent credentials hopd accepts.
//
// Credentials live in a file, one per line: a label and the SHA-256 of the
// secret.
//
//	# /etc/hop/tokens
//	laptop  sha256:9f2b1c4d...
//	ci      sha256:41ac77e0...
//
// The label is what makes the file worth having. It names the device in every
// log line, and it gives you exactly one line to delete when a laptop is lost —
// rather than rotating a single shared secret and then hunting down every copy
// of it.
//
// Only the hash is stored, so a leaked backup, a stray `cat`, or a config
// pasted into an issue yields nothing usable. The cost is that the server can
// never show you a token again: it is displayed once, when minted.
//
// Plain SHA-256 is deliberate, and not an oversight. bcrypt and argon2 exist to
// make *guessable* secrets expensive to attack; a 32-byte random token has no
// dictionary to run against it, so a password KDF would buy nothing here but
// latency on every handshake.
package tokens

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	hashPrefix = "sha256:"
	// secretBytes is the size of a minted token. 256 bits is far past what
	// brute force can reach, which is the assumption the hashing choice rests
	// on.
	secretBytes = 32
)

// labelRe keeps labels to something that reads cleanly in a log line and can't
// be confused with a hash.
var labelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Hash returns the stored form of a secret.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Mint returns a new random secret.
func Mint() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// LabelOK reports whether s is a usable label.
func LabelOK(s string) bool { return labelRe.MatchString(s) }

// Store answers "is this token good, and whose is it?".
//
// It holds two sets. static comes from -tokens or HOP_TOKENS and is fixed for
// the life of the process; file comes from the tokens file and is replaced
// whenever that file changes. Reloading only ever touches the second, so a
// broken file can't invalidate the credentials the process started with.
type Store struct {
	path   string
	static map[string]string // hash -> label

	mu    sync.RWMutex
	file  map[string]string // hash -> label
	stamp stamp
}

// stamp is what we compare to decide the file changed, without hashing the file
// on every poll. Modification time plus size catches an in-place write or a
// truncate. The file's identity catches the atomic rename editors and config
// management do, which modification time alone can miss: Linux stamps files
// from a clock that only moves every few milliseconds, and a replacement token
// line is exactly as long as the one it replaces, so a quick swap can leave
// both unchanged.
type stamp struct {
	file os.FileInfo
	mod  time.Time
	size int64
}

// New returns a store backed only by the given hash-to-label pairs, with no
// file behind it. Reload and Watch are no-ops on it.
func New(static map[string]string) *Store {
	return &Store{static: static, file: map[string]string{}}
}

// Open loads path and combines it with static. An empty path means static
// only. A file that exists but cannot be parsed is a startup failure: at boot
// there is nothing to fall back to, and failing loudly beats coming up with an
// unexpectedly small set of valid tokens.
func Open(path string, static map[string]string) (*Store, error) {
	s := New(static)
	s.path = path
	if path == "" {
		return s, nil
	}
	if _, err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Lookup returns the label owning secret.
//
// The comparison is a map lookup on the hash of the presented secret, not on
// the secret itself, so there is no timing signal to mine: an attacker who
// could exploit it would have to know the token already.
func (s *Store) Lookup(secret string) (string, bool) {
	if secret == "" {
		return "", false
	}
	h := Hash(secret)
	if label, ok := s.static[h]; ok {
		return label, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	label, ok := s.file[h]
	return label, ok
}

// Labels is the set of owners currently accepted. Anything holding a name on
// behalf of a label absent from this set is holding it on a revoked credential.
func (s *Store) Labels() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]bool, len(s.static)+len(s.file))
	for _, label := range s.static {
		out[label] = true
	}
	for _, label := range s.file {
		out[label] = true
	}
	return out
}

// Len is how many credentials are currently accepted.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.static) + len(s.file)
}

// Reload re-reads the tokens file. On any error the previous set is kept, so a
// half-written or mistyped file cannot lock every agent out of a running
// server.
func (s *Store) Reload() (int, error) {
	if s.path == "" {
		return 0, nil
	}
	// Stat before reading, not after. If the file is rewritten between the two,
	// the stamp we record is the older one, so the next poll sees a difference
	// and reloads again. Stamping afterwards could record the new file's stamp
	// against the old file's contents and then never look again.
	fi, err := os.Stat(s.path)
	if err != nil {
		return 0, err
	}
	parsed, parseErr := parseFile(s.path)

	s.mu.Lock()
	// Record what we looked at even when it failed to parse, so a broken file
	// is complained about once per edit rather than once per poll.
	s.stamp = stamp{fi, fi.ModTime(), fi.Size()}
	if parseErr == nil && len(parsed) > 0 {
		s.file = parsed
	}
	s.mu.Unlock()

	if parseErr != nil {
		return 0, parseErr
	}
	if len(parsed) == 0 {
		// Almost always a truncated write rather than an intent to revoke
		// everyone at once. To actually deny every agent, stop the service.
		return 0, fmt.Errorf("%s defines no tokens", s.path)
	}
	return len(parsed), nil
}

// Watch reloads the file whenever it changes, until stop is closed. After each
// successful reload it calls onReload, if non-nil, with the labels that
// survived — which is how revoking a credential reaches the agent still using
// it.
//
// Polling rather than inotify: it needs no dependency, it survives the
// atomic-rename dance editors do (which replaces the inode a watch was
// registered on), and a few seconds of delay is irrelevant for adding a
// device. The cost of a missed event here is that a new token doesn't work
// yet, which the operator notices immediately.
func (s *Store) Watch(stop <-chan struct{}, every time.Duration, onReload func(labels map[string]bool)) {
	if s.path == "" {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if !s.changed() {
				continue
			}
			if _, err := s.Reload(); err != nil {
				log.Printf("tokens: %v — keeping the previous set", err)
				continue
			}
			// Len, not the file's own count, so this number means the same
			// thing as the one logged at startup. Reporting just the file's
			// share reads as tokens disappearing whenever HOP_TOKENS is also
			// in play.
			log.Printf("tokens: reloaded %s, %d accepted", s.path, s.Len())
			if onReload != nil {
				onReload(s.Labels())
			}
		}
	}
}

func (s *Store) changed() bool {
	fi, err := os.Stat(s.path)
	if err != nil {
		// Most likely the brief gap in the middle of a rename. Say nothing and
		// let the next tick see the replacement.
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !os.SameFile(fi, s.stamp.file) || !fi.ModTime().Equal(s.stamp.mod) || fi.Size() != s.stamp.size
}

func parseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	byHash := make(map[string]string)
	byLabel := make(map[string]bool)

	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf(`%s:%d: want "<label> sha256:<hex>", got %d field(s)`, path, n, len(fields))
		}
		label, value := fields[0], fields[1]

		if !LabelOK(label) {
			return nil, fmt.Errorf("%s:%d: %q is not a valid label (1-32 chars of a-z, 0-9, '-')", path, n, label)
		}
		if !strings.HasPrefix(value, hashPrefix) {
			// The likeliest mistake is pasting the token itself, which would
			// silently never match anything. Say so.
			return nil, fmt.Errorf("%s:%d: second field must start with %q — this file stores the hash, never the token (see `hopd mint`)", path, n, hashPrefix)
		}
		h := strings.ToLower(strings.TrimPrefix(value, hashPrefix))
		if len(h) != sha256.Size*2 {
			return nil, fmt.Errorf("%s:%d: hash is %d hex chars, want %d", path, n, len(h), sha256.Size*2)
		}
		if _, err := hex.DecodeString(h); err != nil {
			return nil, fmt.Errorf("%s:%d: hash is not hex", path, n)
		}

		// Duplicates are refused rather than resolved, because either answer
		// would be a guess about which line the operator meant to keep.
		if byLabel[label] {
			return nil, fmt.Errorf("%s:%d: label %q appears twice", path, n, label)
		}
		if prev, ok := byHash[h]; ok {
			return nil, fmt.Errorf("%s:%d: this token is already listed as %q", path, n, prev)
		}
		byLabel[label] = true
		byHash[h] = label
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return byHash, nil
}
