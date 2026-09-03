package client

import (
	"net"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"hop.vokh.dev/internal/tunnel"
)

// A server too old to know about the goodbye never accepts the stream it
// arrives on, so nothing ever acknowledges it. That is the case the frame was
// put on a stream for — it costs the agent its bound and nothing else — and it
// must not be the case where Ctrl-C appears to hang.
func TestFarewellStopsWaitingForAServerThatNeverReads(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })

	sess, err := yamux.Server(a, tunnel.Config())
	if err != nil {
		t.Fatal(err)
	}
	// The far end runs a session but never calls Accept, exactly like a hopd
	// built before Bye existed.
	peer, err := yamux.Client(b, tunnel.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })

	var f Farewell
	f.hold(sess)

	const bound = 300 * time.Millisecond
	start := time.Now()
	f.Say(bound)
	elapsed := time.Since(start)

	if elapsed > 5*bound {
		t.Fatalf("Say took %v against a server that never reads, want it bounded near %v", elapsed, bound)
	}
	select {
	case <-sess.CloseChan():
	default:
		t.Error("Say gave up without closing the session, leaving the connection open")
	}
}

// Say hands the session over exactly once. A second call — two signals, or a
// signal racing a shutdown — must not open a stream on a session that is
// already gone.
func TestFarewellSaysGoodbyeOnlyOnce(t *testing.T) {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })

	sess, err := yamux.Server(a, tunnel.Config())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := yamux.Client(b, tunnel.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })

	var f Farewell
	f.hold(sess)
	f.Say(200 * time.Millisecond)

	start := time.Now()
	f.Say(10 * time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the second Say took %v, want immediate", elapsed)
	}
}

// A nil Farewell is what an agent has when nothing wired one up, and hold and
// Say are called unconditionally on the tunnel's path.
func TestNilFarewellIsInert(t *testing.T) {
	var f *Farewell
	f.hold(nil)
	f.Say(time.Second)
}
