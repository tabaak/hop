package main

import (
	"strings"
	"testing"

	"hop.vokh.dev/internal/proto"
)

func TestOlderRelease(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v1.0.0", "v1.1.0", true},
		{"v1.1.0", "v1.1.0", false},
		// A patch adds nothing a client would miss.
		{"v1.1.0", "v1.1.5", false},
		{"v1.9.0", "v2.0.0", true},
		{"v2.0.0", "v1.9.0", false},
		{"v1.0.0-rc.1", "v1.1.0", true},
		// Development builds can't be placed, so they never warn.
		{"(devel)", "v1.1.0", false},
		{"v1.0.0", "v0.0.0-20260930-abcdef+dirty", false},
		{"1.0.0", "v1.1.0", false},
	}
	for _, c := range cases {
		if got := olderRelease(c.a, c.b); got != c.want {
			t.Errorf("olderRelease(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestServerBehind(t *testing.T) {
	if note := serverBehind(proto.ServerInfo{Release: "v1.1.0"}, "v1.1.0"); note != "" {
		t.Errorf("same release warned: %q", note)
	}
	if note := serverBehind(proto.ServerInfo{Release: "v1.1.0"}, "v1.2.0"); !strings.Contains(note, "hopd v1.1.0") {
		t.Errorf("older server: note = %q, want it to name the server's release", note)
	}
	// Silent servers are v1.0.x: behind a v1.1 hop, level with a v1.0 one.
	if note := serverBehind(proto.ServerInfo{}, "v1.1.0"); !strings.Contains(note, "from before v1.1.0") {
		t.Errorf("silent server: note = %q", note)
	}
	if note := serverBehind(proto.ServerInfo{}, "v1.0.0"); note != "" {
		t.Errorf("silent server vs v1.0 hop warned: %q", note)
	}
}
