package main

import (
	"testing"
	"time"

	"hop.vokh.dev/internal/proto"
)

func TestUptime(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m30s"},
		{59*time.Minute + 59*time.Second, "59m59s"},
		{time.Hour, "1h0m"},
		{25 * time.Hour, "1d1h"},
		{8 * 24 * time.Hour, "8d0h"},
	}
	for _, c := range cases {
		if got := uptime(c.in); got != c.want {
			t.Errorf("uptime(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The owner column is worth a column only when owners differ; with a single
// token issued it would repeat the same word down the table.
func TestShowOwners(t *testing.T) {
	cases := []struct {
		name   string
		owners []string
		want   bool
	}{
		{"empty", nil, false},
		{"one tunnel", []string{"laptop"}, false},
		{"one device, several tunnels", []string{"laptop", "laptop", "laptop"}, false},
		{"two devices", []string{"laptop", "phone"}, true},
		{"difference in the last row", []string{"laptop", "laptop", "phone"}, true},
	}
	for _, c := range cases {
		var tunnels []proto.TunnelInfo
		for _, o := range c.owners {
			tunnels = append(tunnels, proto.TunnelInfo{Owner: o})
		}
		if got := showOwners(tunnels); got != c.want {
			t.Errorf("%s: showOwners = %v, want %v", c.name, got, c.want)
		}
	}
}
