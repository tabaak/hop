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
	"sync"
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
	// Tap, if set, receives a copy of every exchange for the local inspector.
	// Also called from the per-stream goroutine.
	Tap Tap
	// OnUp, if set, is called each time the tunnel comes up, including after a
	// reconnect — the name can change if the old one was taken while the agent
	// was away. Called from Run's goroutine, before any request is served.
	OnUp func(sub, url string)
	// Farewell, if set, lets a signal handler end the tunnel deliberately. Run
	// keeps it pointed at whichever session is live.
	Farewell *Farewell
	// Release is this agent's release version, sent so the server's log can
	// say which hop each device runs. Empty sends nothing.
	Release string
	// OnServer, if set, is told what the server reported about itself after
	// each successful handshake — empty fields from a server older than
	// v1.1.0. Called from Run's goroutine, before OnUp.
	OnServer func(proto.ServerInfo)
}

// Farewell ends a tunnel on purpose.
//
// A dropped connection and a deliberate stop look identical from the server's
// side — both are a session that stopped answering — so the name is held for
// the grace window either way unless the agent says otherwise. This is how it
// says otherwise: one frame, on one stream, on the session that happens to be
// live at the time.
//
// Nothing depends on the goodbye arriving. A lost one — SIGKILL, a panic, a
// connection that is already half open — leaves the name to be released when
// its window expires, which is what would have happened anyway.
type Farewell struct {
	mu   sync.Mutex
	sess *yamux.Session
	said bool
}

// Said reports whether a goodbye has been sent. The reconnect loop has to ask:
// Say ends the session, which makes Run return exactly as a dropped connection
// would, and a loop that could not tell them apart would dial straight back and
// re-claim the name it had just handed over.
func (f *Farewell) Said() bool {
	if f == nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.said
}

// hold points the farewell at the live session, or at nothing once it ends.
func (f *Farewell) hold(sess *yamux.Session) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.sess = sess
	f.mu.Unlock()
}

// Say tells the server this stop was deliberate, and waits up to wait for it
// to let go. It returns at once when there is no session to say it on, which
// is the case whenever the agent is between reconnects — a Ctrl-C during a
// backoff should not pause at the prompt for something nobody is listening to.
func (f *Farewell) Say(wait time.Duration) {
	if f == nil {
		return
	}
	f.mu.Lock()
	sess := f.sess
	f.sess = nil
	// Recorded even when there is no session to say it on: the stop was still
	// deliberate, and an agent sitting in a backoff must not dial again.
	f.said = true
	f.mu.Unlock()
	if sess == nil {
		return
	}

	deadline := time.Now().Add(wait)
	st, err := sess.OpenStream()
	if err != nil {
		sess.Close()
		return
	}
	// Deadlined rather than left to the session's own write timeout, which is
	// fifteen seconds: a goodbye that hung would delay the shutdown it exists
	// to hurry along.
	st.SetWriteDeadline(deadline)
	proto.Write(st, proto.Bye{})
	st.Close()

	// The server closes the session once it has read the frame, so the close
	// is the acknowledgement. Past the deadline, stop waiting and drop the
	// connection: the name will expire on its own.
	select {
	case <-sess.CloseChan():
	case <-time.After(time.Until(deadline)):
		sess.Close()
	}
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

// Tap observes traffic without altering it. The agent knows nothing about what
// is on the other end of this interface, which keeps the inspector — the only
// implementation — off the forwarding path's list of concerns.
type Tap interface {
	// Begin starts recording the exchange whose request head is head. It may
	// return nil to skip one.
	Begin(head []byte) Capture
}

// Capture collects one exchange. Its writers are teed off the byte stream in
// both directions, so they must never block and never fail: a Write that
// returned an error would tear down the request being inspected.
type Capture interface {
	// Request is fed the request body, Response the response head and body.
	Request() io.Writer
	Response() io.Writer
	// Fail records an exchange that never reached the local app.
	Fail(status int, msg string)
	// Close ends the exchange, and may be called more than once.
	Close()
}

// ErrRefused means the server rejected the request. Worded without a noun
// because both a tunnel and a listing can be refused; each caller supplies its
// own.
var ErrRefused = errors.New("refused")

// Refusal is a refused tunnel, with the reason the server gave. It matches
// errors.Is(err, ErrRefused) like every other refusal, so callers that only
// care that they were turned away are unaffected; callers deciding whether to
// try again use Retryable.
type Refusal struct {
	// Code is one of the proto.Code* constants, or empty from a server too old
	// to send one.
	Code string
	// Reason is the server's sentence, meant for a human.
	Reason string
}

func (r *Refusal) Error() string { return "refused: " + r.Reason }

// Is makes errors.Is(err, ErrRefused) true for a Refusal.
func (r *Refusal) Is(target error) bool { return target == ErrRefused }

// Retryable reports whether waiting could change the answer.
//
// Only a taken name qualifies. An agent that has been up and is reclaiming its
// own name is very likely racing its own previous session — or, once the
// server holds grace leases, the lease that session left behind — and the
// holder lets go on its own. A bad token or an illegal name is a fact about
// the request, and the second attempt meets the same fact.
func (r *Refusal) Retryable() bool { return r.Code == proto.CodeTaken }

// Result is what one session did, which is what the caller needs to decide how
// to reconnect.
type Result struct {
	// Subdomain is the name the server assigned, so a reconnect can ask for the
	// same one and the printed URL stays valid across a network blip.
	Subdomain string
	// Served is true once a request has come down the tunnel. It says the
	// session did real work, which elapsed time does not: a server that accepts
	// connections and then sits on them looks long-lived and is not healthy.
	Served bool
}

// Run connects once and serves until the tunnel drops.
func Run(cfg Config) (Result, error) {
	var res Result

	conn, err := dial(cfg)
	if err != nil {
		return res, fmt.Errorf("dial %s: %w", cfg.Server, err)
	}

	ack, err := handshake(conn, cfg)
	if err != nil {
		conn.Close()
		return res, err
	}
	res.Subdomain = ack.Subdomain
	if cfg.OnServer != nil {
		cfg.OnServer(ack.ServerInfo)
	}

	// The agent takes the yamux server role: the hop server is the side that
	// opens a stream per inbound request.
	sess, err := yamux.Server(conn, tunnel.Config())
	if err != nil {
		conn.Close()
		return res, fmt.Errorf("yamux upgrade: %w", err)
	}
	defer sess.Close()

	cfg.Farewell.hold(sess)
	defer cfg.Farewell.hold(nil)

	fmt.Fprintf(os.Stderr, "\n  %s  →  http://%s\n\n", ack.URL, cfg.Local)
	if cfg.OnUp != nil {
		cfg.OnUp(ack.Subdomain, ack.URL)
	}

	for {
		stream, err := sess.Accept()
		if err != nil {
			return res, fmt.Errorf("tunnel closed: %w", err)
		}
		res.Served = true
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
		Release:   cfg.Release,
	}
	if err := proto.Write(conn, hello); err != nil {
		return ack, fmt.Errorf("send hello: %w", err)
	}
	if err := proto.Read(conn, &ack); err != nil {
		return ack, fmt.Errorf("read ack: %w", err)
	}
	if ack.Err != "" {
		return ack, &Refusal{Code: ack.Code, Reason: ack.Err}
	}
	// A server that reports its protocol has already checked ours. One that
	// doesn't predates the check, and would accept an agent it can't serve and
	// then fail in ways nobody could read — so the agent does the check itself.
	if !proto.SpeaksWith(ack.Protocol) {
		return ack, &Refusal{
			Code: proto.CodeVersion,
			Reason: fmt.Sprintf("the server is too old for this hop (it speaks protocol %s, this hop speaks %s); ask whoever runs hopd to upgrade it, or use an older hop",
				proto.ServerProtocol(ack.Protocol), proto.Version),
		}
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
	var capture Capture
	if headErr == nil {
		rec.Method, rec.Target = requestLine(head)
		rec.UserAgent = headerValue(head, "user-agent")
		if cfg.Tap != nil {
			// Started from the head as it arrived, before any Host rewrite, so
			// the inspector shows the request the caller actually sent.
			if capture = cfg.Tap.Begin(head); capture != nil {
				defer capture.Close()
			}
		}
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
		if capture != nil {
			capture.Fail(http.StatusBadGateway, err.Error())
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
	// Teed rather than buffered: the copy goes to the inspector as the bytes
	// pass, so streaming responses and WebSockets are unaffected by watching
	// them. Both tees sit outside the splice, which still moves the real bytes.
	var body io.Reader = src
	if capture != nil {
		body = io.TeeReader(src, capture.Request())
		down = io.TeeReader(down, capture.Response())
	}
	splice(stream, body, up, down)
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
