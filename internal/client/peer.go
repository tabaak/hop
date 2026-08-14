package client

import (
	"fmt"
	"net"
)

// The agent's very first act on a new connection is to write its token, before
// anything is negotiated (see handshake). Over TLS that is fine. In plaintext
// it puts the credential on the wire in the clear, and anyone on the path can
// lift it and claim subdomains on your zone.
//
// --no-tls exists for local development against a plaintext hopd, and there is
// no legitimate reason to speak plaintext across the public internet. So
// rather than warning about it, refuse it: a prompt would be answered "yes"
// reflexively within a week, and would break every non-interactive caller
// besides.

// cgnat is RFC 6598 shared address space, which is what Tailscale hands out.
// Allowing it is a deliberate loosening — the same range is used for carrier
// NAT — justified by Tailscale being an encrypted overlay and a common way to
// reach a machine on your own tailnet.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// requirePrivatePeer rejects a plaintext connection to anything that isn't on
// a network you control. The error wraps ErrRefused so the agent exits with it
// rather than entering the reconnect loop: retrying a connection that will
// never be permitted just spins to the 30s backoff ceiling and stays there.
func requirePrivatePeer(addr net.Addr) error {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: --no-tls, and the peer address %q could not be parsed", ErrRefused, host)
	}
	if isPrivate(ip) {
		return nil
	}
	return fmt.Errorf("%w: --no-tls refused, %s is a public address.\n"+
		"  Plaintext sends your token in the clear, where anyone on the path can take it.\n"+
		"  Drop --no-tls, or point --server at a local or private address", ErrRefused, ip)
}

func isPrivate(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() || // RFC 1918 and IPv6 unique-local (fc00::/7)
		ip.IsLinkLocalUnicast() ||
		cgnat.Contains(ip)
}
