package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"hop.vokh.dev/internal/client"
)

// Colour is on only when stderr is a terminal, so redirecting the log to a
// file or piping it into grep produces plain text rather than escape codes.
// NO_COLOR is honoured because it is the de facto standard for this
// (https://no-color.org), as is a --no-color flag for the times it isn't.
var colour = false

func initColour(disabled bool) {
	if disabled || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return
	}
	info, err := os.Stderr.Stat()
	colour = err == nil && info.Mode()&os.ModeCharDevice != 0
}

const (
	reset   = "\033[0m"
	dim     = "\033[2m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	blue    = "\033[34m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
)

func paint(s, colourCode string) string {
	if !colour {
		return s
	}
	return colourCode + s + reset
}

// methodColour distinguishes reads from writes at a glance: green for the
// safe ones, warmer colours as the request gets more destructive.
func methodColour(method string) string {
	switch method {
	case "GET":
		return green
	case "HEAD", "OPTIONS":
		return dim
	case "POST":
		return blue
	case "PUT", "PATCH":
		return yellow
	case "DELETE":
		return red
	default:
		return magenta
	}
}

func statusColour(status int) string {
	switch {
	case status == 101:
		return magenta // protocol upgrade: a WebSocket opening
	case status >= 500:
		return red
	case status >= 400:
		return yellow
	case status >= 300:
		return cyan
	case status >= 200:
		return green
	default:
		return dim
	}
}

var logMu sync.Mutex

// logRequest prints one line per request. Everything before the target is
// fixed-width so the columns line up; the target goes last because it is the
// one field with no useful bound on its length.
func logRequest(r client.Request) {
	method, target := r.Method, r.Target
	if method == "" {
		method, target = "?", "?"
	}

	logMu.Lock()
	defer logMu.Unlock()
	fmt.Fprintf(os.Stderr, "  %s %s %s  %s  %s\n",
		pad(paint(method, methodColour(method)), method, 6),
		pad(paint(statusText(r.Status), statusColour(r.Status)), statusText(r.Status), 3),
		pad(paint(duration(r.Took), dim), duration(r.Took), 8),
		pad(paint(device(r.UserAgent), dim), device(r.UserAgent), 14),
		target,
	)
}

// pad widens s to n columns based on plain, since s may carry escape codes
// that occupy no width on screen but would otherwise break alignment. Width is
// counted in runes rather than bytes: the truncation ellipsis is three bytes
// and one column, and counting bytes would indent the following column short.
func pad(s, plain string, n int) string {
	if d := n - utf8.RuneCountInString(plain); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// statusText renders an unparseable status as "---" rather than 0, which would
// read as a real code.
func statusText(status int) string {
	if status == 0 {
		return "---"
	}
	return strconv.Itoa(status)
}

func duration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	default:
		return fmt.Sprintf("%.2fs", d.Seconds())
	}
}

// device condenses a User-Agent into something readable at a glance, which is
// the point of showing it at all: when you're testing a page on your phone and
// your laptop at the same time, you want to see which one made the request.
//
// User-Agent strings are a well-known swamp of mutual imitation — every
// browser claims to be Mozilla, Chrome claims to be Safari, Edge claims to be
// Chrome — so the order of these checks matters and the result is a good
// guess, not a fact.
func device(ua string) string {
	if ua == "" {
		return "-"
	}

	// Tools first: they are unambiguous and usually announce themselves at the
	// very start of the string.
	switch {
	case strings.HasPrefix(ua, "curl/"):
		return "curl"
	case strings.HasPrefix(ua, "Wget/"):
		return "wget"
	case strings.HasPrefix(ua, "Go-http-client/"):
		return "Go"
	case strings.HasPrefix(ua, "python-requests/"):
		return "python"
	case strings.Contains(ua, "PostmanRuntime"):
		return "Postman"
	case strings.Contains(ua, "Googlebot"), strings.Contains(ua, "bingbot"):
		return "bot"
	}

	platform := ""
	switch {
	case strings.Contains(ua, "iPhone"):
		platform = "iPhone"
	case strings.Contains(ua, "iPad"):
		platform = "iPad"
	case strings.Contains(ua, "Android"):
		platform = "Android"
	case strings.Contains(ua, "Macintosh"), strings.Contains(ua, "Mac OS X"):
		platform = "Mac"
	case strings.Contains(ua, "Windows"):
		platform = "Win"
	case strings.Contains(ua, "Linux"):
		platform = "Linux"
	}

	// Most specific first: Edge and Opera both carry "Chrome", and Chrome
	// carries "Safari".
	browser := ""
	switch {
	case strings.Contains(ua, "Edg/"):
		browser = "Edge"
	case strings.Contains(ua, "OPR/"):
		browser = "Opera"
	case strings.Contains(ua, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(ua, "CriOS/"):
		browser = "Chrome" // Chrome on iOS, which is Safari underneath
	case strings.Contains(ua, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(ua, "Safari/"):
		browser = "Safari"
	}

	switch {
	case platform != "" && browser != "":
		return platform + " " + browser
	case browser != "":
		return browser
	case platform != "":
		return platform
	}
	return truncate(firstToken(ua), 14)
}

func firstToken(s string) string {
	if i := strings.IndexAny(s, " /"); i > 0 {
		return s[:i]
	}
	return s
}

// truncate shortens s to n columns, counted in runes so a multi-byte agent
// string isn't cut mid-character.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
