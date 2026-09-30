package client

import (
	"errors"
	"fmt"
	"time"

	"hop.vokh.dev/internal/proto"
)

// listTimeout bounds the whole exchange. It is one small frame each way with no
// session behind it, so anything slower than this is a network problem rather
// than a busy server.
const listTimeout = 10 * time.Second

// List asks the server which tunnels are up.
//
// It dials the same control port an agent does and authenticates with the same
// token, then hangs up without claiming anything. Reusing that connection means
// the listing inherits the TLS and the auth already protecting it, instead of
// needing an HTTP endpoint that would have to be secured separately — and
// exposed through whatever fronts the ingress.
//
// What the server says about itself comes back alongside, with empty fields
// from a server older than v1.1.0.
func List(cfg Config) ([]proto.TunnelInfo, proto.ServerInfo, error) {
	conn, err := dial(cfg)
	if err != nil {
		return nil, proto.ServerInfo{}, fmt.Errorf("dial %s: %w", cfg.Server, err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(listTimeout)); err != nil {
		return nil, proto.ServerInfo{}, err
	}
	hello := proto.Hello{Token: cfg.Token, Op: proto.OpList, Version: proto.Version, Release: cfg.Release}
	if err := proto.Write(conn, hello); err != nil {
		return nil, proto.ServerInfo{}, fmt.Errorf("send hello: %w", err)
	}

	var reply proto.Listing
	if err := proto.Read(conn, &reply); err != nil {
		return nil, proto.ServerInfo{}, fmt.Errorf("read listing: %w", err)
	}
	if reply.Err != "" {
		return nil, reply.ServerInfo, fmt.Errorf("%w: %s", ErrRefused, reply.Err)
	}
	// A server predating the op field ignored it and opened a tunnel for us,
	// answering with a HelloAck. Saying so beats printing the empty table that
	// reply would otherwise decode into. The claim it made is released the
	// moment this connection closes.
	if reply.URL != "" {
		return nil, reply.ServerInfo, errors.New("the server is too old for `hop ps` — upgrade hopd")
	}
	return reply.Tunnels, reply.ServerInfo, nil
}
