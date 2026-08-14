package client

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestRequirePrivatePeer(t *testing.T) {
	cases := []struct {
		name  string
		ip    string
		allow bool
	}{
		{"loopback v4", "127.0.0.1", true},
		{"loopback v6", "::1", true},
		{"rfc1918 10", "10.0.0.5", true},
		{"rfc1918 192.168", "192.168.1.42", true},
		{"rfc1918 172.16", "172.16.0.1", true},
		{"docker bridge", "172.17.0.1", true},
		{"link-local", "169.254.10.1", true},
		{"ipv6 unique-local", "fd00::1", true},
		{"tailscale cgnat", "100.101.102.103", true},

		{"the hop VPS", "79.76.51.163", false},
		{"public dns", "8.8.8.8", false},
		{"public v6", "2606:4700:4700::1111", false},
		// 172.32 is outside RFC 1918: the private range stops at 172.31.
		{"just outside 172.16/12", "172.32.0.1", false},
		// 100.128 is outside the CGNAT /10.
		{"just outside cgnat", "100.128.0.1", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			addr := &net.TCPAddr{IP: net.ParseIP(c.ip), Port: 7443}
			err := requirePrivatePeer(addr)

			if c.allow && err != nil {
				t.Fatalf("requirePrivatePeer(%s) = %v, want nil", c.ip, err)
			}
			if !c.allow {
				if err == nil {
					t.Fatalf("requirePrivatePeer(%s) = nil, want a refusal", c.ip)
				}
				// Must be terminal, or the agent retries something that can
				// never be allowed.
				if !errors.Is(err, ErrRefused) {
					t.Errorf("error does not wrap ErrRefused: %v", err)
				}
				if !strings.Contains(err.Error(), "--no-tls") {
					t.Errorf("error should name the flag at fault: %v", err)
				}
			}
		})
	}
}

func TestRequirePrivatePeerUnparseable(t *testing.T) {
	err := requirePrivatePeer(fakeAddr("not-an-address"))
	if err == nil {
		t.Fatal("want a refusal for an unparseable peer, got nil")
	}
	if !errors.Is(err, ErrRefused) {
		t.Errorf("error does not wrap ErrRefused: %v", err)
	}
}

type fakeAddr string

func (fakeAddr) Network() string  { return "tcp" }
func (a fakeAddr) String() string { return string(a) }
