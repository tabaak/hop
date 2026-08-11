// Package tunnel holds the yamux configuration shared by both sides, so the
// agent and the server can't drift apart on keepalive or window settings.
package tunnel

import (
	"io"
	"time"

	"github.com/hashicorp/yamux"
)

// Config returns the yamux settings used on both ends of a hop tunnel.
//
// Keepalive is what keeps a NAT or a home router from silently dropping an
// idle tunnel; yamux enables it by default, we just make the interval explicit.
func Config() *yamux.Config {
	c := yamux.DefaultConfig()
	c.EnableKeepAlive = true
	c.KeepAliveInterval = 30 * time.Second
	c.ConnectionWriteTimeout = 15 * time.Second
	c.LogOutput = io.Discard
	return c
}
