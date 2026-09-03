// Package proto defines the handshake exchanged on the raw control connection
// before both sides upgrade it to a yamux session.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Version is the protocol version an agent announces in Hello.
const Version = "1"

// What an agent is dialling in to do. A connection does one or the other and
// the choice is made in the first frame, so the server knows before it touches
// the registry whether a name is about to be claimed.
const (
	// OpTunnel opens a tunnel. The empty string means the same thing, so an
	// agent built before this field existed still works.
	OpTunnel = "tunnel"
	// OpList asks for the live tunnels and closes. It still authenticates:
	// which names are in use is worth as much to someone choosing a target as
	// it is to you.
	OpList = "list"
)

// maxFrame bounds a single handshake frame so a hostile peer can't make us
// allocate arbitrarily.
const maxFrame = 64 << 10

// Hello is the agent's opening frame.
type Hello struct {
	Token string `json:"token"`
	// Op is OpTunnel or OpList. Empty is OpTunnel.
	Op string `json:"op,omitempty"`
	// Subdomain is the name the agent would like. Empty means "assign me one".
	Subdomain string `json:"subdomain,omitempty"`
	// Local is the address this agent forwards to, reported so `hop ps` can
	// show what each device is exposing. Nothing depends on it: the server
	// stores it, echoes it back in a listing, and never acts on it.
	Local   string `json:"local,omitempty"`
	Version string `json:"version"`
}

// Why a tunnel was refused, in a form the agent can branch on. Err carries the
// sentence for a human; Code says whether waiting could change the answer.
//
// Only CodeTaken is worth retrying, and only by an agent reclaiming a name it
// was already using: the holder is most likely its own previous session, or
// the grace lease left behind by one. Everything else is a fact about the
// request that a second attempt would meet again.
const (
	// CodeTaken — the subdomain belongs to someone else right now.
	CodeTaken = "taken"
	// CodeBadName — the subdomain isn't a legal DNS label.
	CodeBadName = "bad-name"
	// CodeBadToken — the token isn't one this server accepts.
	CodeBadToken = "bad-token"
)

// HelloAck is the server's reply to OpTunnel. Err is set iff the tunnel was
// refused.
type HelloAck struct {
	URL       string `json:"url,omitempty"`
	Subdomain string `json:"subdomain,omitempty"`
	Err       string `json:"err,omitempty"`
	// Code is one of the Code* constants, or empty from a server too old to
	// send one. Empty means "assume nothing will change", which is how every
	// refusal was treated before this field existed — so an old server costs
	// an agent its reconnect, never a wrong decision.
	Code string `json:"code,omitempty"`
}

// Bye is the agent saying a stop was deliberate — Ctrl-C, or `hop stop` —
// rather than a connection that failed. It travels on its own yamux stream
// after the handshake, not as a new dial and not as a new op.
//
// A stream is what makes this safe to add without a version bump, and the
// reason is worth stating: a server too old to know about it never calls
// Accept, so the stream is discarded and the agent's name is held for the
// usual window, which is that server's existing behaviour. A new op would be
// far worse — anything that isn't OpList falls through to the tunnel path, so
// an old server would read a goodbye as a request to *claim* a name.
type Bye struct {
	// Reason is for the log, not for logic. Empty means an ordinary stop.
	Reason string `json:"reason,omitempty"`
}

// Listing is the server's reply to OpList.
type Listing struct {
	Tunnels []TunnelInfo `json:"tunnels"`
	Err     string       `json:"err,omitempty"`

	// URL is never set by a server answering OpList. It exists so the client
	// can recognise a server old enough to ignore Op — which would have read
	// this connection as a tunnel request, claimed a name, and replied with a
	// HelloAck. Without the check that lands as an empty list, which reads as
	// "nothing is running" rather than "ask a newer server".
	URL string `json:"url,omitempty"`
}

// TunnelInfo is one live tunnel.
type TunnelInfo struct {
	Subdomain string `json:"subdomain"`
	URL       string `json:"url"`
	// Owner is the token label serving it, not the token.
	Owner string `json:"owner"`
	// Local is what the agent forwards to, as the agent reported it. Empty for
	// an agent too old to send it.
	Local string `json:"local,omitempty"`
	// UptimeSeconds is measured on the server. Sending an elapsed time rather
	// than a start timestamp keeps the number meaningful when the two clocks
	// disagree — a VPS a few minutes off would otherwise produce tunnels that
	// started in the future.
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// MaxLocal bounds the address an agent reports about itself.
const MaxLocal = 64

// CleanLocal makes a reported address safe to print.
//
// It is the one field in a listing that comes from somewhere other than the
// server's own state — an agent sends it, and it is then printed in someone
// else's terminal, where an escape sequence could clear the screen, rewrite
// earlier lines, or forge a row. Anything non-printable is dropped rather than
// escaped, since a real address contains none.
//
// Applied by the server on the way in, and again by the client when rendering
// a locally recorded address: the local file is written from this machine's own
// command line, but a display path that assumes its input is well-formed is one
// hand-edited file away from the same mess.
func CleanLocal(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if b.Len() >= MaxLocal {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Write encodes v as JSON behind a 4-byte big-endian length prefix.
func Write(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrame {
		return errors.New("proto: frame too large")
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Read decodes one length-prefixed JSON frame into v.
func Read(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return errors.New("proto: frame too large")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
