package main

import (
	"context"
	"log"
	"net/netip"
	"strings"
	"time"

	"hop.vokh.dev/internal/dnscheck"
	"hop.vokh.dev/internal/dnsprovider"
)

// dnsPoll is how often hopd re-checks records it is waiting on. Authoritative
// nameservers see a new record within seconds, so this is mostly about not
// hammering them during a long wait.
const dnsPoll = 10 * time.Second

// prepareDNS makes sure the bare domain and its wildcard resolve before hopd
// asks for certificates or tells agents to connect. With a DNS provider it
// creates whichever records are missing; without one it prints them and
// waits for the operator.
func prepareDNS(domain, publicIPFlag, providerName string, p dnsprovider.Provider, manage bool) {
	ctx := context.Background()

	var ip netip.Addr
	if publicIPFlag != "" {
		var err error
		if ip, err = netip.ParseAddr(publicIPFlag); err != nil {
			log.Fatalf("-public-ip: %v", err)
		}
	} else if ip = dnscheck.PublicIP(ctx); !ip.IsValid() {
		log.Print("dns: could not determine this server's public IP; set -public-ip (HOP_PUBLIC_IP) to check records point here")
	}

	r := dnscheck.NewResolver()
	res := dnscheck.Check(ctx, r, domain, ip)
	if res.Missing() && manage && p != nil && ip.IsValid() {
		var names []string
		for _, n := range res.Names {
			if n.Status == dnscheck.Missing {
				names = append(names, n.Record)
			}
		}
		log.Printf("dns: creating %s → %s via %s", strings.Join(names, ", "), ip, providerName)
		if err := dnsprovider.CreateRecords(ctx, p, names, ip); err != nil {
			// Not fatal: the token may be scoped for challenges only. The
			// operator can still create the records by hand while we wait.
			log.Printf("dns: could not create records automatically: %v", err)
		}
	}

	if _, err := dnscheck.Wait(ctx, r, domain, ip, dnsPoll, log.Printf); err != nil {
		log.Fatalf("dns: %v", err)
	}
	log.Printf("dns: %s and *.%s resolve", domain, domain)
}

// skipDNSCheck reports whether the domain is one no public record could exist
// for — local development, where waiting would only ever hang.
func skipDNSCheck(domain string) bool {
	d := strings.TrimSuffix(strings.ToLower(domain), ".")
	if d == "localhost" || strings.HasSuffix(d, ".localhost") {
		return true
	}
	_, err := netip.ParseAddr(d)
	return err == nil
}
