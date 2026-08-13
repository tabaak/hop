package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDevice(t *testing.T) {
	cases := []struct {
		name, ua, want string
	}{
		{"empty", "", "-"},
		{"curl", "curl/8.4.0", "curl"},
		{"wget", "Wget/1.21.4", "wget"},
		{"go", "Go-http-client/1.1", "Go"},
		{"postman", "PostmanRuntime/7.36.0", "Postman"},

		{"iphone safari",
			"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
			"iPhone Safari"},

		// Chrome's UA also contains "Safari"; Safari must not win.
		{"mac chrome",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
			"Mac Chrome"},

		// Edge's UA contains both "Chrome" and "Safari".
		{"windows edge",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 Edg/120.0.0.0",
			"Win Edge"},

		// Opera carries "Chrome" too.
		{"opera",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36 OPR/106.0.0.0",
			"Win Opera"},

		{"android chrome",
			"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36",
			"Android Chrome"},

		{"firefox",
			"Mozilla/5.0 (X11; Linux x86_64; rv:121.0) Gecko/20100101 Firefox/121.0",
			"Linux Firefox"},

		{"ipad", "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) Version/17.0 Safari/604.1", "iPad Safari"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := device(c.ua); got != c.want {
				t.Errorf("device(%q) = %q, want %q", c.ua, got, c.want)
			}
		})
	}
}

// An unrecognised agent falls back to its leading token, bounded so it can't
// blow the column apart.
func TestDeviceUnknownIsTruncated(t *testing.T) {
	got := device("SomeVeryLongUnknownAgentName/1.0")
	// Counted in runes: the ellipsis is one column but three bytes.
	if n := utf8.RuneCountInString(got); n > 14 {
		t.Errorf("device() = %q, %d columns; want at most 14", got, n)
	}
}

// Without a terminal, colour must stay off so redirected output is plain.
func TestNoColourWhenNotATerminal(t *testing.T) {
	colour = false
	initColour(false)
	if colour {
		t.Error("colour enabled although stderr is not a terminal under go test")
	}
	if got := paint("GET", green); got != "GET" {
		t.Errorf("paint() = %q, want unstyled %q", got, "GET")
	}
}

// pad must measure the plain text, not the escape codes, or coloured columns
// would come out narrower than uncoloured ones.
func TestPadIgnoresEscapeCodes(t *testing.T) {
	styled := green + "GET" + reset
	got := pad(styled, "GET", 6)
	if !strings.HasSuffix(got, "   ") {
		t.Errorf("pad() = %q, want three trailing spaces", got)
	}
	if strings.Count(got, " ") != 3 {
		t.Errorf("pad() added %d spaces, want 3", strings.Count(got, " "))
	}
}
