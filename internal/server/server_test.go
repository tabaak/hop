package server

import "testing"

func TestSubdomainOf(t *testing.T) {
	s := New(Config{Domain: "hop.vokh.dev"})

	tests := []struct {
		host string
		want string
	}{
		{"myapp.hop.vokh.dev", "myapp"},
		{"myapp.hop.vokh.dev:443", "myapp"},
		{"MyApp.Hop.Vokh.Dev", "myapp"},
		{"myapp.hop.vokh.dev.", "myapp"},    // trailing root dot
		{"hop.vokh.dev", ""},                // bare domain, no tunnel
		{"a.b.hop.vokh.dev", ""},            // we only serve one level
		{"myapp.hop.vokh.dev.evil.com", ""}, // suffix must be exact
		{"vokh.dev", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := s.subdomainOf(tc.host); got != tc.want {
			t.Errorf("subdomainOf(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}
