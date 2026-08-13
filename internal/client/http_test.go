package client

import (
	"bufio"
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestReadHead(t *testing.T) {
	raw := "GET /a HTTP/1.1\r\nHost: x\r\n\r\nBODY"
	r := bufio.NewReader(strings.NewReader(raw))

	head, err := readHead(r)
	if err != nil {
		t.Fatal(err)
	}
	if want := "GET /a HTTP/1.1\r\nHost: x\r\n\r\n"; string(head) != want {
		t.Errorf("head = %q, want %q", head, want)
	}
	// The body must still be readable: the head parser must not swallow it.
	rest, _ := io.ReadAll(r)
	if string(rest) != "BODY" {
		t.Errorf("rest = %q, want %q", rest, "BODY")
	}
}

// A header longer than bufio's buffer arrives in fragments. The terminator
// check must not fire on a fragment that happens to end the line.
func TestReadHeadLongHeaderLine(t *testing.T) {
	long := strings.Repeat("a", 9000)
	raw := "GET / HTTP/1.1\r\nHost: x\r\nCookie: " + long + "\r\n\r\nBODY"
	r := bufio.NewReader(strings.NewReader(raw))

	head, err := readHead(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(head, []byte("\r\n\r\n")) {
		t.Error("head does not end at the blank line")
	}
	if !bytes.Contains(head, []byte(long)) {
		t.Error("long header value was truncated")
	}
	rest, _ := io.ReadAll(r)
	if string(rest) != "BODY" {
		t.Errorf("rest = %q, want %q", rest, "BODY")
	}
}

func TestReadHeadTooLarge(t *testing.T) {
	raw := "GET / HTTP/1.1\r\n" + strings.Repeat("X: y\r\n", maxHead)
	if _, err := readHead(bufio.NewReader(strings.NewReader(raw))); err == nil {
		t.Fatal("want an error for an oversized head, got nil")
	}
}

func TestRequestLine(t *testing.T) {
	method, target := requestLine([]byte("POST /api/users?q=1 HTTP/1.1\r\nHost: x\r\n\r\n"))
	if method != "POST" || target != "/api/users?q=1" {
		t.Errorf("got %q %q, want POST /api/users?q=1", method, target)
	}
	if m, _ := requestLine([]byte("garbage\r\n")); m != "" {
		t.Errorf("method = %q, want empty for a non-request line", m)
	}
}

func TestSetHost(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{{
		name: "replaces the value",
		in:   "GET / HTTP/1.1\r\nHost: public.example\r\nA: b\r\n\r\n",
		want: "GET / HTTP/1.1\r\nHost: 127.0.0.1:3000\r\nA: b\r\n\r\n",
	}, {
		name: "matches case-insensitively",
		in:   "GET / HTTP/1.1\r\nhost: public.example\r\n\r\n",
		want: "GET / HTTP/1.1\r\nHost: 127.0.0.1:3000\r\n\r\n",
	}, {
		name: "replaces only the first",
		in:   "GET / HTTP/1.1\r\nHost: a\r\nHost: b\r\n\r\n",
		want: "GET / HTTP/1.1\r\nHost: 127.0.0.1:3000\r\nHost: b\r\n\r\n",
	}, {
		name: "leaves a head with no Host alone",
		in:   "GET / HTTP/1.1\r\nA: b\r\n\r\n",
		want: "GET / HTTP/1.1\r\nA: b\r\n\r\n",
	}, {
		// Must not match a header that merely starts with "host".
		name: "does not match Host-prefixed header names",
		in:   "GET / HTTP/1.1\r\nHostile: x\r\n\r\n",
		want: "GET / HTTP/1.1\r\nHostile: x\r\n\r\n",
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := string(setHost([]byte(c.in), "127.0.0.1:3000")); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

func TestStatusOf(t *testing.T) {
	cases := map[string]int{
		"HTTP/1.1 200 OK":              200,
		"HTTP/1.1 404 Not Found":       404,
		"HTTP/1.1 101 Switching Proto": 101,
		"HTTP/1.1 999 Nonsense":        0,
		"not a status line":            0,
		"":                             0,
		"HTTP/1.1":                     0,
		"HTTP/1.1 twohundred":          0,
	}
	for line, want := range cases {
		if got := statusOf(line); got != want {
			t.Errorf("statusOf(%q) = %d, want %d", line, got, want)
		}
	}
}

// The sniffer must report the first line without altering the stream.
func TestSniffer(t *testing.T) {
	const raw = "HTTP/1.1 200 OK\r\nA: b\r\n\r\nbody"
	var got string
	s := &sniffer{r: strings.NewReader(raw), onLine: func(l string) { got = l }}

	out, err := io.ReadAll(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != raw {
		t.Errorf("stream was altered:\ngot  %q\nwant %q", out, raw)
	}
	if want := "HTTP/1.1 200 OK"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

// A line split across reads must still be assembled correctly.
func TestSnifferAcrossReads(t *testing.T) {
	var got string
	s := &sniffer{r: &chunkReader{chunks: []string{"HTTP/1.", "1 200 O", "K\r\nrest"}}, onLine: func(l string) { got = l }}

	if _, err := io.ReadAll(s); err != nil {
		t.Fatal(err)
	}
	if want := "HTTP/1.1 200 OK"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

// A stream with no newline must not buffer without bound.
func TestSnifferGivesUpWithoutNewline(t *testing.T) {
	called := false
	s := &sniffer{r: strings.NewReader(strings.Repeat("x", 8<<10)), onLine: func(string) { called = true }}
	io.ReadAll(s)

	if called {
		t.Error("onLine fired on a stream with no line terminator")
	}
	if s.buf != nil {
		t.Errorf("buffer retained %d bytes, want it dropped", len(s.buf))
	}
}

// chunkReader returns its chunks one Read at a time, to exercise line
// assembly across read boundaries.
type chunkReader struct {
	chunks []string
	i      int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}
