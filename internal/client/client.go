// Package client implements the hop agent: dial the server, hold the tunnel
// open, and splice each inbound stream to the local port.
package client

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
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
	// TLS verifies the server certificate against the system roots. Off only
	// for local development against a plaintext server.
	TLS bool
	// RootCAs overrides the trust store. Tests set it to trust a throwaway CA;
	// production leaves it nil to use the system roots. Note this *replaces*
	// the trust anchors rather than disabling verification — there is
	// deliberately no skip-verify option.
	RootCAs *x509.CertPool
	// HostHeader replaces the Host the local app sees. Empty passes the public
	// host through untouched, which is the default and usually what you want.
	HostHeader string
	// Log, if set, is called once per request. Called from the per-stream
	// goroutine, so it must be safe for concurrent use.
	Log func(Request)
	// OnUp, if set, is called each time the tunnel comes up, including after a
	// reconnect — the name can change if the old one was taken while the agent
	// was away. Called from Run's goroutine, before any request is served.
	OnUp func(sub, url string)
}

// Request is one logged request. Status and Took are filled in when the
// response status line comes back, so Took is time to first byte.
type Request struct {
	Method    string
	Target    string
	Status    int
	Took      time.Duration
	UserAgent string
}

// ErrRefused means the server rejected the request for a reason that won't
// change by retrying (bad token, taken name). Worded without a noun because
// both a tunnel and a listing can be refused; each caller supplies its own.
var ErrRefused = errors.New("refused")

// Run connects once and serves until the tunnel drops. It returns the
// subdomain the server assigned, so a reconnect can ask for the same one and
// the printed URL stays valid across a network blip.
func Run(cfg Config) (assigned string, err error) {
	conn, err := dial(cfg)
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
	if cfg.OnUp != nil {
		cfg.OnUp(ack.Subdomain, ack.URL)
	}

	for {
		stream, err := sess.Accept()
		if err != nil {
			return assigned, fmt.Errorf("tunnel closed: %w", err)
		}
		go forward(stream, cfg)
	}
}

// dial opens the control connection, wrapped in TLS unless disabled. The
// server's certificate is checked against the system roots like any HTTPS
// client would — there is no pinning and no skip-verify escape hatch, because
// this connection carries the auth token.
func dial(cfg Config) (net.Conn, error) {
	netDialer := &net.Dialer{Timeout: 10 * time.Second}
	if !cfg.TLS {
		conn, err := netDialer.Dial("tcp", cfg.Server)
		if err != nil {
			return nil, err
		}
		// Checked on the connected socket rather than on a resolved name.
		// Resolving separately from dialing is a time-of-check/time-of-use
		// gap: a hostile resolver could answer 127.0.0.1 for the check and
		// something public for the dial. Inspecting the peer we actually
		// reached means the token cannot leave the machine unless the
		// connection is already provably local.
		if err := requirePrivatePeer(conn.RemoteAddr()); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
	host, _, err := net.SplitHostPort(cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("server address must be host:port: %w", err)
	}
	dialer := &tls.Dialer{
		NetDialer: netDialer,
		Config: &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
			RootCAs:    cfg.RootCAs,
		},
	}
	return dialer.Dial("tcp", cfg.Server)
}

func handshake(conn net.Conn, cfg Config) (proto.HelloAck, error) {
	var ack proto.HelloAck

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return ack, err
	}
	hello := proto.Hello{
		Token:     cfg.Token,
		Op:        proto.OpTunnel,
		Subdomain: cfg.Subdomain,
		Local:     cfg.Local,
		Version:   proto.Version,
	}
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

// forward hands one inbound stream to the local app. The request head is read
// so it can be logged and, optionally, have its Host rewritten; everything
// after it is a raw byte copy in both directions, which is why WebSockets, SSE
// and streaming bodies pass through untouched.
func forward(stream net.Conn, cfg Config) {
	defer stream.Close()

	src := bufio.NewReader(stream)
	head, headErr := readHead(src)
	var rec Request
	if headErr == nil {
		rec.Method, rec.Target = requestLine(head)
		rec.UserAgent = headerValue(head, "user-agent")
	}

	// Started before the dial so a slow or refused connection to the local app
	// is reflected in the reported duration.
	start := time.Now()

	up, err := net.DialTimeout("tcp", cfg.Local, 5*time.Second)
	if err != nil {
		log.Printf("local %s unreachable: %v", cfg.Local, err)
		if cfg.Log != nil {
			rec.Status, rec.Took = http.StatusBadGateway, time.Since(start)
			cfg.Log(rec)
		}
		writeGatewayError(stream, cfg.Local)
		return
	}
	defer up.Close()

	if headErr != nil {
		// Not HTTP-shaped, or a head too large to buffer. Splice it anyway
		// rather than dropping the connection; only the log line is lost.
		splice(stream, src, up, up)
		return
	}
	if cfg.HostHeader != "" {
		head = setHost(head, cfg.HostHeader)
	}
	if _, err := up.Write(head); err != nil {
		return
	}

	var down io.Reader = up
	if cfg.Log != nil {
		down = &sniffer{r: up, onLine: func(line string) {
			rec.Status, rec.Took = statusOf(line), time.Since(start)
			cfg.Log(rec)
		}}
	}
	splice(stream, src, up, down)
}

// splice copies both directions and returns once each has finished. src and
// down are read from rather than the raw connections, so buffered bytes and
// the response sniffer are not bypassed.
func splice(stream net.Conn, src io.Reader, up net.Conn, down io.Reader) {
	done := make(chan struct{})
	go func() {
		io.Copy(up, src)
		// Half-close so the local app sees EOF and can respond to a request
		// whose body has ended.
		if c, ok := up.(*net.TCPConn); ok {
			c.CloseWrite()
		}
		close(done)
	}()
	io.Copy(stream, down)
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
