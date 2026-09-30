// Package dnscheck confirms that the two records hop depends on — the bare
// domain agents dial and the wildcard tunnels are served under — resolve
// before hopd goes any further.
//
// Without it, a missing record surfaces much later and far from its cause: as
// an agent that cannot connect, or a browser that cannot find a tunnel, while
// the server logs look perfectly healthy. Checking up front turns that into a
// message naming the exact records to create.
package dnscheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/miekg/dns"
	"go.uber.org/zap"
)

// probeLabel is looked up under the domain to test the wildcard. Any label
// would do; a recognisable one makes it obvious in DNS query logs who asked.
const probeLabel = "hop-dns-check"

// Status is what one name resolved to, from best to worst news.
type Status int

const (
	// OK means the name resolves to this server, or resolves at all when the
	// server's public address is unknown.
	OK Status = iota
	// Mismatch means it resolves, but not to this server. That can be
	// deliberate (a floating IP, a load balancer), so it warns rather than
	// blocks.
	Mismatch
	// Proxied means it resolves to Cloudflare's proxy. Tunnel HTTP would
	// survive that, but the raw TLS control connection on :7443 does not.
	Proxied
	// Missing means the name does not resolve at all. Nothing can work until
	// it does, so this is the one status worth waiting on.
	Missing
)

// Name is the outcome for one of the two records.
type Name struct {
	// Record is what the operator creates: "hop.example.com" or
	// "*.hop.example.com".
	Record string
	// Host is what was actually looked up; for the wildcard, a name under it.
	Host   string
	Addrs  []netip.Addr
	Status Status
}

// Result covers both records, bare domain first.
type Result struct {
	Names []Name
	// PublicIP is this server's address as the internet sees it; invalid when
	// it could not be determined.
	PublicIP netip.Addr
}

// Missing reports whether any record does not resolve yet.
func (r Result) Missing() bool {
	for _, n := range r.Names {
		if n.Status == Missing {
			return true
		}
	}
	return false
}

// Resolver is the subset of *net.Resolver the check needs, so tests can supply
// answers without a network.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// Check resolves the bare domain and a name under the wildcard and compares
// both against publicIP. An invalid publicIP skips the comparison, so the check
// then only establishes that the records exist.
func Check(ctx context.Context, r Resolver, domain string, publicIP netip.Addr) Result {
	res := Result{PublicIP: publicIP}
	for _, n := range []Name{
		{Record: domain, Host: domain},
		{Record: "*." + domain, Host: probeLabel + "." + domain},
	} {
		addrs, err := r.LookupNetIP(ctx, "ip", n.Host)
		n.Addrs = unmap(addrs)
		n.Status = classify(n.Addrs, err, publicIP)
		res.Names = append(res.Names, n)
	}
	return res
}

func classify(addrs []netip.Addr, err error, publicIP netip.Addr) Status {
	if err != nil || len(addrs) == 0 {
		return Missing
	}
	for _, a := range addrs {
		if publicIP.IsValid() && a == publicIP {
			return OK
		}
	}
	for _, a := range addrs {
		if IsCloudflare(a) {
			return Proxied
		}
	}
	if !publicIP.IsValid() {
		return OK
	}
	return Mismatch
}

// unmap turns IPv4-in-IPv6 answers into plain IPv4, so they compare equal to
// the IPv4 public address.
func unmap(addrs []netip.Addr) []netip.Addr {
	out := make([]netip.Addr, len(addrs))
	for i, a := range addrs {
		out[i] = a.Unmap()
	}
	return out
}

// Wait checks until no record is missing or ctx ends, then returns the last
// result. Mismatched and proxied records don't hold it up — they are reported
// and left to the operator — because waiting forever on a setup that is
// deliberately unusual would be worse than a warning.
//
// logf is called with the instructions the first time, and again only when
// the answer changes, so a long wait does not bury the log.
func Wait(ctx context.Context, r Resolver, domain string, publicIP netip.Addr, interval time.Duration, logf func(string, ...any)) (Result, error) {
	var last string
	for {
		res := Check(ctx, r, domain, publicIP)
		if !res.Missing() {
			for _, w := range res.Warnings() {
				logf("%s", w)
			}
			return res, nil
		}
		if msg := res.Instructions(); msg != last {
			logf("%s", msg)
			last = msg
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Instructions describes what to create, for a result with missing records.
func (r Result) Instructions() string {
	ip := "<this server's public IP>"
	if r.PublicIP.IsValid() {
		ip = r.PublicIP.String()
	}
	typ := "A"
	if r.PublicIP.Is6() {
		typ = "AAAA"
	}

	var b strings.Builder
	b.WriteString("DNS is not set up yet. Create these records at your DNS provider:\n\n")
	width := 0
	for _, n := range r.Names {
		width = max(width, len(n.Record))
	}
	for _, n := range r.Names {
		mark := "✓"
		if n.Status == Missing {
			mark = "✗"
		}
		fmt.Fprintf(&b, "  %s  %-4s %-*s  %s\n", mark, typ, width, n.Record, ip)
	}
	b.WriteString("\nWaiting for them to resolve — hopd continues on its own once they do.")
	return b.String()
}

// Warnings describes records that resolve, but not the way hop needs.
func (r Result) Warnings() []string {
	var out []string
	for _, n := range r.Names {
		switch n.Status {
		case Proxied:
			out = append(out, fmt.Sprintf("dns: %s resolves to Cloudflare's proxy (%s). Turn the orange cloud OFF (DNS only) for this record — agents cannot connect through the proxy.", n.Record, join(n.Addrs)))
		case Mismatch:
			out = append(out, fmt.Sprintf("dns: %s resolves to %s, but this server's public IP is %s. If that is not intentional (floating IP, load balancer), point the record here.", n.Record, join(n.Addrs), r.PublicIP))
		}
	}
	return out
}

func join(addrs []netip.Addr) string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// NewResolver asks the domain's authoritative nameservers, falling back to
// public and then system resolvers when they cannot be reached. Going to the
// source matters while waiting: a recursive resolver that answered "does not
// exist" before the record was created keeps repeating that for the zone's
// negative-caching TTL — half an hour on Cloudflare — and hopd would sit
// waiting on a record that is already live.
func NewResolver() Resolver {
	recursive := fallback{primary: publicResolver(), secondary: net.DefaultResolver}
	return &authoritative{recursive: recursive, servers: map[string][]string{}}
}

// publicNameservers are the recursive resolvers used to find a zone and its
// nameservers.
var publicNameservers = []string{"1.1.1.1:53", "8.8.8.8:53"}

func publicResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			var err error
			for _, ns := range publicNameservers {
				var c net.Conn
				if c, err = d.DialContext(ctx, network, ns); err == nil {
					return c, nil
				}
			}
			return nil, err
		},
	}
}

type fallback struct{ primary, secondary Resolver }

func (f fallback) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	addrs, err := f.primary.LookupNetIP(ctx, network, host)
	if err == nil || isNotFound(err) {
		// A definite answer, including "no such name", is the public view.
		return addrs, err
	}
	return f.secondary.LookupNetIP(ctx, network, host)
}

func isNotFound(err error) bool {
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) && dnsErr.IsNotFound
}

func notFound(host string) error {
	return &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// errDelegated means the authoritative answer points elsewhere (a CNAME out of
// the zone), which only a recursive resolver can follow.
var errDelegated = errors.New("answer is a CNAME")

type authoritative struct {
	recursive Resolver

	mu      sync.Mutex
	servers map[string][]string // zone → "ip:53" of its nameservers
}

func (a *authoritative) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	addrs, err := a.lookup(ctx, host)
	if err == nil || isNotFound(err) {
		return addrs, err
	}
	return a.recursive.LookupNetIP(ctx, network, host)
}

func (a *authoritative) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	fqdn := dns.Fqdn(host)
	zone, err := certmagic.FindZoneByFQDN(ctx, zap.NewNop(), fqdn, publicNameservers)
	if err != nil {
		return nil, err
	}
	servers, err := a.nameservers(ctx, zone)
	if err != nil {
		return nil, err
	}

	var out []netip.Addr
	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
		m := new(dns.Msg)
		m.SetQuestion(fqdn, qtype)
		m.RecursionDesired = false
		in, err := exchange(ctx, servers, m)
		if err != nil {
			return nil, err
		}
		switch in.Rcode {
		case dns.RcodeSuccess:
		case dns.RcodeNameError:
			return nil, notFound(host)
		default:
			return nil, fmt.Errorf("%s: %s", host, dns.RcodeToString[in.Rcode])
		}
		cname := false
		for _, rr := range in.Answer {
			switch v := rr.(type) {
			case *dns.A:
				if ip, ok := netip.AddrFromSlice(v.A); ok {
					out = append(out, ip.Unmap())
				}
			case *dns.AAAA:
				if ip, ok := netip.AddrFromSlice(v.AAAA); ok {
					out = append(out, ip)
				}
			case *dns.CNAME:
				cname = true
			}
		}
		if cname && len(out) == 0 {
			return nil, errDelegated
		}
	}
	if len(out) == 0 {
		return nil, notFound(host)
	}
	return out, nil
}

func (a *authoritative) nameservers(ctx context.Context, zone string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.servers[zone]; ok {
		return s, nil
	}
	nss, err := net.DefaultResolver.LookupNS(ctx, zone)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ns := range nss {
		ips, err := a.recursive.LookupNetIP(ctx, "ip4", ns.Host)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			out = append(out, net.JoinHostPort(ip.String(), "53"))
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no reachable nameservers for %s", zone)
	}
	a.servers[zone] = out
	return out, nil
}

func exchange(ctx context.Context, servers []string, m *dns.Msg) (*dns.Msg, error) {
	c := &dns.Client{Timeout: 5 * time.Second}
	var err error
	for _, s := range servers {
		var in *dns.Msg
		if in, _, err = c.ExchangeContext(ctx, m, s); err == nil {
			return in, nil
		}
	}
	return nil, err
}

// ipEchoURLs return the caller's address as plain text. More than one, so a
// single service being down doesn't cost the comparison.
var ipEchoURLs = []string{
	"https://checkip.amazonaws.com",
	"https://api.ipify.org",
	"https://icanhazip.com",
}

// PublicIP asks an echo service for this server's IPv4 address. It returns an
// invalid address rather than an error when none answer: the check still works
// without it, just less precisely.
func PublicIP(ctx context.Context) netip.Addr {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			// IPv4 first: it is what nearly every A record is compared against,
			// and an IPv6 answer from a dual-stack host would never match one.
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp4", addr)
			},
		},
	}
	for _, u := range ipEchoURLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if a, err := netip.ParseAddr(strings.TrimSpace(string(body))); err == nil {
			return a.Unmap()
		}
	}
	return netip.Addr{}
}

// Cloudflare's published proxy ranges, https://www.cloudflare.com/ips/. They
// change rarely; a stale list only costs a less specific warning.
var cloudflareRanges = mustPrefixes(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
)

// IsCloudflare reports whether a is a Cloudflare proxy address.
func IsCloudflare(a netip.Addr) bool {
	for _, p := range cloudflareRanges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}
