package tokens

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write puts contents at path and returns it, so tests read as a sequence of
// file states.
func write(t *testing.T, path, contents string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func tempFile(t *testing.T, contents string) string {
	t.Helper()
	return write(t, filepath.Join(t.TempDir(), "tokens"), contents)
}

func line(label, secret string) string {
	return label + "  sha256:" + Hash(secret) + "\n"
}

func TestMintIsRandomAndHashable(t *testing.T) {
	a, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Mint()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("Mint returned the same secret twice")
	}
	if len(a) != secretBytes*2 {
		t.Errorf("secret is %d chars, want %d", len(a), secretBytes*2)
	}
	if Hash(a) == Hash(b) {
		t.Error("distinct secrets hashed to the same value")
	}
	// The stored form must never contain the secret, or the whole point of
	// hashing is lost.
	if strings.Contains(Hash(a), a) {
		t.Error("hash contains the secret")
	}
}

func TestLookupReturnsTheLabel(t *testing.T) {
	path := tempFile(t, "# devices\n\n"+line("laptop", "aaa")+line("ci", "bbb"))
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct{ secret, want string }{
		{"aaa", "laptop"},
		{"bbb", "ci"},
	} {
		got, ok := s.Lookup(c.secret)
		if !ok || got != c.want {
			t.Errorf("Lookup(%q) = %q, %v; want %q, true", c.secret, got, ok, c.want)
		}
	}
	if _, ok := s.Lookup("ccc"); ok {
		t.Error("an unlisted secret was accepted")
	}
	// The empty token is what an agent sends when HOP_TOKEN is unset, so it
	// must not match an empty or absent file.
	if _, ok := s.Lookup(""); ok {
		t.Error("the empty token was accepted")
	}
}

func TestStaticAndFileCoexist(t *testing.T) {
	path := tempFile(t, line("laptop", "from-file"))
	s, err := Open(path, map[string]string{Hash("from-env"): "env-x"})
	if err != nil {
		t.Fatal(err)
	}
	if label, ok := s.Lookup("from-env"); !ok || label != "env-x" {
		t.Errorf("env token = %q, %v; want env-x, true", label, ok)
	}
	if label, ok := s.Lookup("from-file"); !ok || label != "laptop" {
		t.Errorf("file token = %q, %v; want laptop, true", label, ok)
	}
	if n := s.Len(); n != 2 {
		t.Errorf("Len = %d, want 2", n)
	}
}

func TestParseRejects(t *testing.T) {
	good := Hash("x")
	cases := []struct {
		name, contents, wantMsg string
	}{
		{"one field", "laptop\n", "want"},
		{"three fields", "laptop sha256:" + good + " extra\n", "want"},
		{"raw token pasted", "laptop deadbeef\n", "stores the hash"},
		{"short hash", "laptop sha256:abcd\n", "hex chars"},
		{"not hex", "laptop sha256:" + strings.Repeat("z", 64) + "\n", "not hex"},
		{"bad label", "My Laptop sha256:" + good + "\n", "want"},
		{"label with underscore", "my_laptop sha256:" + good + "\n", "not a valid label"},
		{"duplicate label", line("a", "1") + line("a", "2"), "appears twice"},
		{"duplicate hash", line("a", "1") + line("b", "1"), "already listed"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Open(tempFile(t, c.contents), nil)
			if err == nil {
				t.Fatalf("Open accepted %q", c.contents)
			}
			if !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error %q does not mention %q", err, c.wantMsg)
			}
		})
	}
}

func TestOpenRejectsMissingFile(t *testing.T) {
	// Naming a file that isn't there is a configuration mistake, and coming up
	// with fewer valid tokens than intended is exactly the failure that should
	// be loud.
	if _, err := Open(filepath.Join(t.TempDir(), "absent"), nil); err == nil {
		t.Fatal("Open accepted a path that does not exist")
	}
}

func TestReloadKeepsThePreviousSetOnError(t *testing.T) {
	path := tempFile(t, line("laptop", "good"))
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, broken := range []struct{ name, contents string }{
		{"unparseable", "laptop this-is-not-a-hash\n"},
		{"truncated to nothing", ""},
		{"comments only", "# everything commented out\n"},
	} {
		t.Run(broken.name, func(t *testing.T) {
			write(t, path, broken.contents)
			if _, err := s.Reload(); err == nil {
				t.Fatal("Reload accepted a broken file")
			}
			// The running server must keep working. A truncated write during an
			// edit should not disconnect every agent.
			if label, ok := s.Lookup("good"); !ok || label != "laptop" {
				t.Errorf("previous set was lost: Lookup = %q, %v", label, ok)
			}
		})
	}
}

func TestReloadComplainsOncePerEdit(t *testing.T) {
	path := tempFile(t, line("laptop", "good"))
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, "garbage\n")

	// The first poll after a bad edit should see a change; the next one should
	// not, or the log fills with the same complaint until the file is fixed.
	if !s.changed() {
		t.Fatal("changed() = false after the file was rewritten")
	}
	if _, err := s.Reload(); err == nil {
		t.Fatal("Reload accepted garbage")
	}
	if s.changed() {
		t.Error("changed() = true after a failed reload; the complaint will repeat every poll")
	}
}

func TestWatchPicksUpAddedAndRevokedTokens(t *testing.T) {
	path := tempFile(t, line("laptop", "laptop-secret"))
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	defer close(stop)
	go s.Watch(stop, time.Millisecond, nil)

	// Added without a restart.
	write(t, path, line("laptop", "laptop-secret")+line("phone", "phone-secret"))
	waitFor(t, func() bool { _, ok := s.Lookup("phone-secret"); return ok },
		"the added token was never accepted")

	// Revoked by deleting its line — the case that has to work under pressure.
	write(t, path, line("phone", "phone-secret"))
	waitFor(t, func() bool { _, ok := s.Lookup("laptop-secret"); return !ok },
		"the revoked token was still accepted")

	if _, ok := s.Lookup("phone-secret"); !ok {
		t.Error("revoking one token took the other with it")
	}
}

func TestWatchReportsSurvivingLabels(t *testing.T) {
	// The callback is how a revocation reaches an agent that is already
	// connected, so it must fire with the *post*-reload set.
	path := tempFile(t, line("laptop", "a")+line("phone", "b"))
	s, err := Open(path, map[string]string{Hash("c"): "env-x"})
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan map[string]bool, 4)
	stop := make(chan struct{})
	defer close(stop)
	go s.Watch(stop, time.Millisecond, func(labels map[string]bool) { got <- labels })

	write(t, path, line("phone", "b"))

	select {
	case labels := <-got:
		if labels["laptop"] {
			t.Error("the revoked label is still reported as accepted")
		}
		if !labels["phone"] {
			t.Error("a surviving file label went missing")
		}
		// Env tokens are not in the file and must not be swept up by an edit
		// to it.
		if !labels["env-x"] {
			t.Error("the static label went missing")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("onReload was never called")
	}
}

func TestLabelsIncludesBothSets(t *testing.T) {
	path := tempFile(t, line("laptop", "a"))
	s, err := Open(path, map[string]string{Hash("b"): "env-x"})
	if err != nil {
		t.Fatal(err)
	}
	labels := s.Labels()
	if !labels["laptop"] || !labels["env-x"] || len(labels) != 2 {
		t.Errorf("Labels() = %v, want {laptop, env-x}", labels)
	}
}

func TestWatchSurvivesAtomicRename(t *testing.T) {
	// Editors and config management replace a file rather than writing into it,
	// which swaps the inode. Polling by path handles that; a watch registered
	// on the original inode would not.
	dir := t.TempDir()
	path := write(t, filepath.Join(dir, "tokens"), line("laptop", "old"))
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	defer close(stop)
	go s.Watch(stop, time.Millisecond, nil)

	next := write(t, filepath.Join(dir, "tokens.new"), line("laptop", "new"))
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}

	waitFor(t, func() bool { _, ok := s.Lookup("new"); return ok },
		"the replacement file was never picked up")
}

func TestWatchIsANoOpWithoutAFile(t *testing.T) {
	s := New(map[string]string{Hash("env"): "env-x"})
	stop := make(chan struct{})
	close(stop)
	// Returns immediately rather than ticking forever over an empty path.
	done := make(chan struct{})
	go func() { s.Watch(stop, time.Millisecond, nil); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return for a store with no file")
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}
