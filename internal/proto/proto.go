// Package proto defines the handshake exchanged on the raw control connection
// before both sides upgrade it to a yamux session.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

// Version is the protocol version an agent announces in Hello.
const Version = "1"

// maxFrame bounds a single handshake frame so a hostile peer can't make us
// allocate arbitrarily.
const maxFrame = 64 << 10

// Hello is the agent's opening frame.
type Hello struct {
	Token string `json:"token"`
	// Subdomain is the name the agent would like. Empty means "assign me one".
	Subdomain string `json:"subdomain,omitempty"`
	Version   string `json:"version"`
}

// HelloAck is the server's reply. Err is set iff the tunnel was refused.
type HelloAck struct {
	URL       string `json:"url,omitempty"`
	Subdomain string `json:"subdomain,omitempty"`
	Err       string `json:"err,omitempty"`
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
