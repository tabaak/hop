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
