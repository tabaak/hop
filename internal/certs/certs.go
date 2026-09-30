// Package certs obtains and renews the wildcard certificate hop serves on.
//
// A wildcard name can only be validated by the DNS-01 challenge — there is no
// HTTP-01 or TLS-ALPN-01 path to `*.example.com` — so this always talks to the
// DNS provider (see package dnsprovider), and both other challenge types are disabled so a
// misconfiguration fails loudly instead of quietly issuing a non-wildcard cert.
package certs

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/caddyserver/certmagic"
)

type Config struct {
	// Domain is the zone tunnels live under, e.g. "hop.vokh.dev". The bare name
	// serves the control listener agents dial; see Wildcard for tunnel traffic.
	Domain string
	// Email is the ACME account contact. Let's Encrypt uses it for expiry
	// warnings; optional but strongly advised.
	Email string
	// DNSProvider writes the challenge TXT record. For Cloudflare, its token
	// needs Zone:Read *and* DNS:Edit on the zone: Read alone isn't enough to
	// write the record, and Edit alone isn't enough to find the zone ID.
	DNSProvider certmagic.DNSProvider
	// StorageDir holds the account key and certificates. It must survive
	// restarts — losing it means re-issuing, which burns rate limit.
	StorageDir string
	// Staging uses the Let's Encrypt staging CA, which issues untrusted certs
	// against far looser rate limits. Always start here.
	Staging bool
	// Wildcard also covers "*." + Domain, which is needed only when we
	// terminate TLS for tunnel traffic ourselves. Behind a reverse proxy the
	// proxy holds the wildcard and we only need the bare name for the control
	// listener — so don't ask for a wildcard we won't serve.
	Wildcard bool
}

// TLSConfig obtains the certificate (blocking until it's in hand) and returns a
// tls.Config that serves it and keeps it renewed in the background.
func TLSConfig(ctx context.Context, cfg Config) (*tls.Config, error) {
	if cfg.Domain == "" {
		return nil, errors.New("certs: no domain")
	}
	if cfg.DNSProvider == nil {
		return nil, errors.New("certs: no DNS provider")
	}
	if cfg.StorageDir == "" {
		return nil, errors.New("certs: no storage directory")
	}

	certmagic.Default.Storage = &certmagic.FileStorage{Path: cfg.StorageDir}

	acme := certmagic.DefaultACME
	acme.Agreed = true
	acme.Email = cfg.Email
	acme.CA = certmagic.LetsEncryptProductionCA
	if cfg.Staging {
		acme.CA = certmagic.LetsEncryptStagingCA
	}
	acme.DisableHTTPChallenge = true
	acme.DisableTLSALPNChallenge = true
	acme.DNS01Solver = &certmagic.DNS01Solver{
		DNSManager: certmagic.DNSManager{
			DNSProvider: cfg.DNSProvider,
			// Most providers publish quickly, but the ACME server checks from
			// several vantage points; give propagation room before failing.
			PropagationTimeout: 5 * time.Minute,
		},
	}

	magic := certmagic.NewDefault()
	magic.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(magic, acme)}

	names := []string{cfg.Domain}
	if cfg.Wildcard {
		names = append(names, "*."+cfg.Domain)
	}
	if err := magic.ManageSync(ctx, names); err != nil {
		return nil, fmt.Errorf("certs: obtaining %v: %w", names, err)
	}

	return magic.TLSConfig(), nil
}
