// Package client implements the hop agent: dial the server, hold the tunnel
// open, and splice each inbound stream to the local port.
package client

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/hashicorp/yamux"

	"hop.vokh.dev/internal/proto"
	"hop.vokh.dev/internal/tunnel"
)

type Config struct {
	// Server is the control endpoint, e.g. "hop.vokh.dev:7443".
	Server string
	// Local is the address to forward to, e.g. "127.0.0.1:3000".
	Local string
	// Subdomain is the requested name; empty asks the server to pick.
	Subdomain string
	Token     string
}

// ErrRefused means the server rejected the tunnel for a reason that won't
// change by retrying (bad token, taken name).
var ErrRefused = errors.New("tunnel refused")

// Run connects once and serves until the tunnel drops. It returns the
// subdomain the server assigned, so a reconnect can ask for the same one and
// the printed URL stays valid across a network blip.
func Run(cfg Config) (assigned string, err error) {
	conn, err := net.DialTimeout("tcp", cfg.Server, 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial %s: %w", cfg.Server, err)
	}

	ack, err := handshake(conn, cfg)
	if err != nil {
		conn.Close()
		return "", err
	}
	assigned = ack.Subdomain

	// The agent takes the yamux server role: the hop server is the side that
	// opens a stream per inbound request.
	sess, err := yamux.Server(conn, tunnel.Config())
	if err != nil {
		conn.Close()
		return assigned, fmt.Errorf("yamux upgrade: %w", err)
	}
	defer sess.Close()

	fmt.Fprintf(os.Stderr, "\n  %s  →  http://%s\n\n", ack.URL, cfg.Local)

	for {
		stream, err := sess.Accept()
		if err != nil {
			return assigned, fmt.Errorf("tunnel closed: %w", err)
		}
		go forward(stream, cfg.Local)
	}
}

func handshake(conn net.Conn, cfg Config) (proto.HelloAck, error) {
	var ack proto.HelloAck

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return ack, err
	}
	hello := proto.Hello{Token: cfg.Token, Subdomain: cfg.Subdomain, Version: proto.Version}
	if err := proto.Write(conn, hello); err != nil {
		return ack, fmt.Errorf("send hello: %w", err)
	}
	if err := proto.Read(conn, &ack); err != nil {
		return ack, fmt.Errorf("read ack: %w", err)
	}
	if ack.Err != "" {
		return ack, fmt.Errorf("%w: %s", ErrRefused, ack.Err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return ack, err
	}
	return ack, nil
}

// forward splices one inbound stream to the local app. Because this is a raw
// byte copy rather than an HTTP round trip, WebSockets, SSE and streaming
// bodies all pass through untouched.
func forward(stream net.Conn, local string) {
	defer stream.Close()

	up, err := net.DialTimeout("tcp", local, 5*time.Second)
	if err != nil {
		log.Printf("local %s unreachable: %v", local, err)
		writeGatewayError(stream, local)
		return
	}
	defer up.Close()

	done := make(chan struct{})
	go func() {
		io.Copy(up, stream)
		// Half-close so the local app sees EOF and can respond to a request
		// whose body has ended.
		if c, ok := up.(*net.TCPConn); ok {
			c.CloseWrite()
		}
		close(done)
	}()
	io.Copy(stream, up)
	<-done
}

// writeGatewayError puts a real HTTP response on the wire when the local app
// isn't listening, so the browser shows the reason instead of a bare reset.
func writeGatewayError(w io.Writer, local string) {
	body := fmt.Sprintf("hop: nothing is listening on %s\n", local)
	fmt.Fprintf(w, "HTTP/1.1 502 Bad Gateway\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n"+
		"Content-Length: %d\r\n"+
		"Connection: close\r\n\r\n%s", len(body), body)
}
