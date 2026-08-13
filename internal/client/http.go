package client

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
)

// Just enough HTTP to log a request and rewrite one header. The agent
// deliberately does not parse the whole exchange: it reads the request head,
// adjusts it, and then splices raw bytes, which is what keeps WebSockets, SSE
// and streaming bodies working without special cases.
//
// This is only safe because the server sets DisableKeepAlives on its side of
// the tunnel, so each yamux stream carries exactly one request. If that ever
// changes, everything here has to become a loop.

// maxHead bounds the request head we are willing to buffer. Past this we give
// up parsing and fall back to a plain splice rather than growing without limit.
const maxHead = 64 << 10

var errHeadTooLarge = errors.New("request head too large")

// readHead consumes the request line and headers, up to and including the
// blank line that terminates them, and returns them verbatim.
func readHead(r *bufio.Reader) ([]byte, error) {
	var head []byte
	// True when the previous ReadSlice returned a fragment rather than a whole
	// line, so the next chunk is a continuation and can't be the blank
	// terminator even if it happens to look like one.
	partial := false

	for {
		chunk, err := r.ReadSlice('\n')
		head = append(head, chunk...)
		if len(head) > maxHead {
			return nil, errHeadTooLarge
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			partial = true
			continue
		}
		if err != nil {
			return nil, err
		}
		if !partial && len(bytes.TrimRight(chunk, "\r\n")) == 0 {
			return head, nil
		}
		partial = false
	}
}

// requestLine pulls the method and target out of "GET /path HTTP/1.1".
func requestLine(head []byte) (method, target string) {
	line := head
	if i := bytes.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := bytes.Fields(line)
	if len(fields) < 2 {
		return "", ""
	}
	return string(fields[0]), string(fields[1])
}

// setHost replaces the value of the Host header, leaving every other byte of
// the head untouched. Only the first Host is replaced: HTTP/1.1 permits
// exactly one, and rewriting a duplicate would just be guessing.
//
// If the head carries no Host at all it is returned unchanged, rather than
// having one invented for it.
func setHost(head []byte, host string) []byte {
	out := make([]byte, 0, len(head)+len(host))
	rest := head
	replaced := false

	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			rest = nil
		}
		if !replaced && hasPrefixFold(line, "host:") {
			out = append(out, "Host: "...)
			out = append(out, host...)
			out = append(out, "\r\n"...)
			replaced = true
			continue
		}
		out = append(out, line...)
	}
	return out
}

// headerValue returns the value of the named header, which must be given in
// lower case. Returns "" if absent. Only the first occurrence is considered.
func headerValue(head []byte, name string) string {
	rest := head
	// Skip the request line.
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	}
	prefix := name + ":"

	for len(rest) > 0 {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = nil
		}
		if hasPrefixFold(line, prefix) {
			return string(bytes.TrimSpace(line[len(prefix):]))
		}
	}
	return ""
}

func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && strings.EqualFold(string(b[:len(prefix)]), prefix)
}

// statusOf pulls the code out of "HTTP/1.1 200 OK", returning 0 if the line
// isn't a status line.
func statusOf(line string) int {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil || code < 100 || code > 599 {
		return 0
	}
	return code
}

// sniffer captures the first line passing through it and hands it to onLine,
// without altering or delaying the byte stream. It is used on the response
// direction so the status line can be logged at the moment it is written,
// which makes the reported duration time-to-first-byte rather than
// time-to-connection-close. For a WebSocket or an SSE stream those differ by
// the entire life of the connection.
type sniffer struct {
	r      io.Reader
	buf    []byte
	done   bool
	onLine func(string)
}

func (s *sniffer) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if n > 0 && !s.done {
		s.capture(p[:n])
	}
	return n, err
}

func (s *sniffer) capture(b []byte) {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		s.buf = append(s.buf, b[:i]...)
		s.finish()
		return
	}
	s.buf = append(s.buf, b...)
	// A status line this long means it isn't one. Stop recording rather than
	// buffering an entire response body looking for a newline.
	if len(s.buf) > 4<<10 {
		s.done, s.buf = true, nil
	}
}

func (s *sniffer) finish() {
	s.done = true
	line := strings.TrimRight(string(s.buf), "\r")
	s.buf = nil
	if s.onLine != nil {
		s.onLine(line)
	}
}
