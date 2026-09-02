package inspect

import (
	"net"
	"net/http"
	"os"
	"time"
)

// ListenUnix binds the inspector's HTTP surface to a unix socket and serves it
// in the background. The agent keeps one open for its whole life, which is how
// `hop inspect <name>` reaches a tunnel that was started without --inspect.
//
// A socket file rather than a TCP port: nothing is listening on the network
// until someone asks for the page, there is no port to clash over, and the
// kernel — not hop — enforces that only the same user can connect. It lives in
// ~/.hop/run, which is 0700 already; the chmod is for umasks looser than that.
//
// The bind error comes back synchronously, like Start's: a tunnel that cannot
// be inspected later is still worth having.
func (h *Hub) ListenUnix(path string) error {
	// A previous agent with this pid leaves the file behind when it dies; the
	// kernel does not clean up unix sockets. Nobody else owns this name, so
	// removing it is safe, and without it the bind fails with "address already
	// in use" until something sweeps the directory.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		return err
	}
	srv := &http.Server{
		Handler:           h.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go srv.Serve(ln)
	return nil
}
