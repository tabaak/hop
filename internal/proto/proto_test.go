package proto

import (
	"strings"
	"testing"
)

// The forwarded address is the one listing field supplied by a peer, and it is
// printed in someone else's terminal. A hostile agent must not be able to put
// escape sequences there, or an unbounded string.
func TestCleanLocal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ordinary address", "127.0.0.1:3000", "127.0.0.1:3000"},
		{"lan address", "192.168.1.42:8080", "192.168.1.42:8080"},
		{"empty", "", ""},
		{"clears the screen", "\x1b[2Jgotcha", "[2Jgotcha"},
		{"rewrites the line", "boring\rmalicious", "boringmalicious"},
		{"forges a row", "a\nb", "ab"},
		{"delete", "a\x7fb", "ab"},
		{"too long", strings.Repeat("x", 200), strings.Repeat("x", MaxLocal)},
	}
	for _, c := range cases {
		got := CleanLocal(c.in)
		if got != c.want {
			t.Errorf("%s: CleanLocal(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if strings.ContainsAny(got, "\x1b\r\n") {
			t.Errorf("%s: output still carries a control character: %q", c.name, got)
		}
	}
}

func TestSupported(t *testing.T) {
	cases := []struct {
		v                  string
		wantOK, wantTooOld bool
	}{
		{Version, true, false},
		{MinVersion, true, false},
		// Read as the legacy protocol rather than rejected.
		{"", true, false},
		{"0", false, true},
		{"99", false, false},
		{"one", false, true},
	}
	for _, c := range cases {
		ok, tooOld := Supported(c.v)
		if ok != c.wantOK || tooOld != c.wantTooOld {
			t.Errorf("Supported(%q) = %v, %v; want %v, %v", c.v, ok, tooOld, c.wantOK, c.wantTooOld)
		}
	}
}

// A server that reports its protocol has already judged the agent's; a silent
// one is pre-v1.1.0 and speaks only the legacy protocol.
func TestSpeaksWith(t *testing.T) {
	if !SpeaksWith(Version) {
		t.Errorf("SpeaksWith(%q) = false for a server that accepted us", Version)
	}
	if got, want := SpeaksWith(""), Version == legacyVersion; got != want {
		t.Errorf("SpeaksWith(\"\") = %v, want %v", got, want)
	}
	if got := ServerProtocol(""); got != legacyVersion {
		t.Errorf("ServerProtocol(\"\") = %q, want %q", got, legacyVersion)
	}
}
