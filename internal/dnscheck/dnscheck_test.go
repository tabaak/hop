package dnscheck

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeResolver answers from a map; names absent from it are NXDOMAIN.
type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
}

func (f *fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.answers[host]; ok {
		return a, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

func (f *fakeResolver) set(host string, addrs ...netip.Addr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[host] = addrs
}

var (
	server = netip.MustParseAddr("203.0.113.7")
	other  = netip.MustParseAddr("198.51.100.9")
	cf     = netip.MustParseAddr("104.21.3.4")
)

func TestCheck(t *testing.T) {
	const domain = "hop.example.com"
	wild := probeLabel + "." + domain

	tests := []struct {
		name     string
		answers  map[string][]netip.Addr
		publicIP netip.Addr
		want     [2]Status
	}{
		{"both missing", nil, server, [2]Status{Missing, Missing}},
		{"wildcard missing", map[string][]netip.Addr{domain: {server}}, server, [2]Status{OK, Missing}},
		{"both ok", map[string][]netip.Addr{domain: {server}, wild: {server}}, server, [2]Status{OK, OK}},
		{"mapped v4 matches", map[string][]netip.Addr{domain: {netip.AddrFrom16(server.As16())}, wild: {server}}, server, [2]Status{OK, OK}},
		{"elsewhere", map[string][]netip.Addr{domain: {other}, wild: {server}}, server, [2]Status{Mismatch, OK}},
		{"orange cloud", map[string][]netip.Addr{domain: {cf}, wild: {cf}}, server, [2]Status{Proxied, Proxied}},
		{"unknown public ip", map[string][]netip.Addr{domain: {other}, wild: {other}}, netip.Addr{}, [2]Status{OK, OK}},
		{"proxied, unknown public ip", map[string][]netip.Addr{domain: {cf}, wild: {other}}, netip.Addr{}, [2]Status{Proxied, OK}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeResolver{answers: tt.answers}
			res := Check(context.Background(), r, domain, tt.publicIP)
			for i, n := range res.Names {
				if n.Status != tt.want[i] {
					t.Errorf("%s: status %d, want %d", n.Record, n.Status, tt.want[i])
				}
			}
		})
	}
}

func TestInstructionsNameRecordsAndIP(t *testing.T) {
	r := &fakeResolver{answers: map[string][]netip.Addr{}}
	res := Check(context.Background(), r, "hop.example.com", server)
	msg := res.Instructions()
	for _, want := range []string{"hop.example.com", "*.hop.example.com", "203.0.113.7", " A "} {
		if !strings.Contains(msg, want) {
			t.Errorf("instructions missing %q:\n%s", want, msg)
		}
	}

	res = Check(context.Background(), r, "hop.example.com", netip.Addr{})
	if !strings.Contains(res.Instructions(), "<this server's public IP>") {
		t.Errorf("unknown IP should leave a placeholder:\n%s", res.Instructions())
	}
}

func TestWaitReturnsOnceRecordsAppear(t *testing.T) {
	const domain = "hop.example.com"
	r := &fakeResolver{answers: map[string][]netip.Addr{}}

	var mu sync.Mutex
	var logs []string
	logf := func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, f)
	}

	done := make(chan Result)
	go func() {
		res, err := Wait(context.Background(), r, domain, server, time.Millisecond, logf)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()

	time.Sleep(20 * time.Millisecond)
	r.set(domain, server)
	r.set(probeLabel+"."+domain, other)

	select {
	case res := <-done:
		if res.Missing() {
			t.Fatal("returned with records still missing")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after records appeared")
	}

	mu.Lock()
	defer mu.Unlock()
	// Instructions once (they did not change during the wait), then one
	// warning for the wildcard pointing elsewhere.
	if len(logs) != 2 {
		t.Fatalf("got %d log lines, want 2: %q", len(logs), logs)
	}
}

func TestWaitHonoursCancel(t *testing.T) {
	r := &fakeResolver{answers: map[string][]netip.Addr{}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := Wait(ctx, r, "hop.example.com", server, time.Millisecond, func(string, ...any) {}); err == nil {
		t.Fatal("want an error once the context ends")
	}
}

func TestIsCloudflare(t *testing.T) {
	if !IsCloudflare(cf) || !IsCloudflare(netip.MustParseAddr("2606:4700::1")) {
		t.Error("Cloudflare addresses not recognised")
	}
	if IsCloudflare(server) {
		t.Error("non-Cloudflare address flagged")
	}
}
