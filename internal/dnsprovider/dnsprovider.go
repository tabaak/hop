// Package dnsprovider builds a libdns provider for whichever DNS host the
// operator uses, from environment variables.
//
// One credential does two jobs: it answers the ACME DNS-01 challenge for the
// wildcard certificate, and it creates the domain's A records on first start
// so nobody has to click through a DNS dashboard. Both need nothing beyond
// appending and deleting records, which every libdns provider supports.
package dnsprovider

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/libdns/bunny"
	"github.com/libdns/cloudflare"
	"github.com/libdns/desec"
	"github.com/libdns/digitalocean"
	"github.com/libdns/duckdns"
	"github.com/libdns/gandi"
	"github.com/libdns/godaddy"
	"github.com/libdns/hetzner"
	"github.com/libdns/libdns"
	"github.com/libdns/linode"
	"github.com/libdns/namecheap"
	"github.com/libdns/ovh"
	"github.com/libdns/porkbun"
	"github.com/libdns/route53"
	"github.com/libdns/vultr/v2"
	"go.uber.org/zap"
)

// Provider is what both callers need: certmagic appends and deletes the
// challenge TXT record, and hopd appends A records.
type Provider = certmagic.DNSProvider

type spec struct {
	// required env vars; the error for a missing one names them all, so the
	// operator fixes the configuration in one pass rather than one per restart.
	required []string
	// optional env vars, listed in help text only.
	optional []string
	build    func(env func(string) string) Provider
}

var specs = map[string]spec{
	"cloudflare": {
		required: []string{"CLOUDFLARE_API_TOKEN"},
		build: func(env func(string) string) Provider {
			return &cloudflare.Provider{APIToken: env("CLOUDFLARE_API_TOKEN")}
		},
	},
	"route53": {
		// Credentials may also come from an instance role or ~/.aws, which the
		// AWS SDK finds on its own, so nothing is strictly required.
		optional: []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_REGION", "AWS_PROFILE", "AWS_HOSTED_ZONE_ID"},
		build: func(env func(string) string) Provider {
			return &route53.Provider{
				AccessKeyId:        env("AWS_ACCESS_KEY_ID"),
				SecretAccessKey:    env("AWS_SECRET_ACCESS_KEY"),
				SessionToken:       env("AWS_SESSION_TOKEN"),
				Region:             env("AWS_REGION"),
				Profile:            env("AWS_PROFILE"),
				HostedZoneID:       env("AWS_HOSTED_ZONE_ID"),
				WaitForRoute53Sync: true,
			}
		},
	},
	"digitalocean": {
		required: []string{"DIGITALOCEAN_TOKEN"},
		build: func(env func(string) string) Provider {
			return &digitalocean.Provider{APIToken: env("DIGITALOCEAN_TOKEN")}
		},
	},
	"hetzner": {
		required: []string{"HETZNER_API_TOKEN"},
		build: func(env func(string) string) Provider {
			return &hetzner.Provider{AuthAPIToken: env("HETZNER_API_TOKEN")}
		},
	},
	"porkbun": {
		required: []string{"PORKBUN_API_KEY", "PORKBUN_SECRET_API_KEY"},
		build: func(env func(string) string) Provider {
			return &porkbun.Provider{APIKey: env("PORKBUN_API_KEY"), APISecretKey: env("PORKBUN_SECRET_API_KEY")}
		},
	},
	"duckdns": {
		required: []string{"DUCKDNS_TOKEN"},
		build: func(env func(string) string) Provider {
			return &duckdns.Provider{APIToken: env("DUCKDNS_TOKEN")}
		},
	},
	"gandi": {
		required: []string{"GANDI_BEARER_TOKEN"},
		build: func(env func(string) string) Provider {
			return &gandi.Provider{BearerToken: env("GANDI_BEARER_TOKEN")}
		},
	},
	"vultr": {
		required: []string{"VULTR_API_KEY"},
		build: func(env func(string) string) Provider {
			return &vultr.Provider{APIToken: env("VULTR_API_KEY")}
		},
	},
	"linode": {
		required: []string{"LINODE_TOKEN"},
		build: func(env func(string) string) Provider {
			return &linode.Provider{APIToken: env("LINODE_TOKEN")}
		},
	},
	"namecheap": {
		required: []string{"NAMECHEAP_API_KEY", "NAMECHEAP_API_USER"},
		// Namecheap only accepts API calls from allowlisted IPs; left empty,
		// the provider discovers this server's address itself.
		optional: []string{"NAMECHEAP_CLIENT_IP"},
		build: func(env func(string) string) Provider {
			return &namecheap.Provider{APIKey: env("NAMECHEAP_API_KEY"), User: env("NAMECHEAP_API_USER"), ClientIP: env("NAMECHEAP_CLIENT_IP")}
		},
	},
	"ovh": {
		required: []string{"OVH_ENDPOINT", "OVH_APPLICATION_KEY", "OVH_APPLICATION_SECRET", "OVH_CONSUMER_KEY"},
		build: func(env func(string) string) Provider {
			return &ovh.Provider{
				Endpoint:          env("OVH_ENDPOINT"),
				ApplicationKey:    env("OVH_APPLICATION_KEY"),
				ApplicationSecret: env("OVH_APPLICATION_SECRET"),
				ConsumerKey:       env("OVH_CONSUMER_KEY"),
			}
		},
	},
	"godaddy": {
		// GoDaddy's key and secret travel together as "key:secret".
		required: []string{"GODADDY_TOKEN"},
		build: func(env func(string) string) Provider {
			return &godaddy.Provider{APIToken: env("GODADDY_TOKEN")}
		},
	},
	"desec": {
		required: []string{"DESEC_TOKEN"},
		build: func(env func(string) string) Provider {
			return &desec.Provider{Token: env("DESEC_TOKEN")}
		},
	},
	"bunny": {
		required: []string{"BUNNY_API_KEY"},
		build: func(env func(string) string) Provider {
			return &bunny.Provider{AccessKey: env("BUNNY_API_KEY")}
		},
	},
}

// Names lists the supported providers, sorted.
func Names() []string {
	out := make([]string, 0, len(specs))
	for n := range specs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Detect picks a provider when DNS_PROVIDER is unset. Only Cloudflare is
// inferred — from its token, which is how every deployment before
// DNS_PROVIDER existed was configured. Guessing among the others from
// generic-looking variables (AWS_* in particular) would be surprising.
func Detect(env func(string) string) string {
	if env("CLOUDFLARE_API_TOKEN") != "" {
		return "cloudflare"
	}
	return ""
}

// New builds the named provider from env. An empty name means none is
// configured and returns (nil, nil).
func New(name string, env func(string) string) (Provider, error) {
	if name == "" {
		return nil, nil
	}
	name = strings.ToLower(strings.TrimSpace(name))
	s, ok := specs[name]
	if !ok {
		return nil, fmt.Errorf("unknown DNS provider %q; supported: %s", name, strings.Join(Names(), ", "))
	}
	var missing []string
	for _, k := range s.required {
		if env(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("DNS provider %s needs %s", name, strings.Join(missing, ", "))
	}
	return s.build(env), nil
}

// Help describes each provider's variables, for the "no provider" error.
func Help() string {
	var b strings.Builder
	for _, n := range Names() {
		s := specs[n]
		vars := strings.Join(s.required, " ")
		if len(s.optional) > 0 {
			if vars != "" {
				vars += " "
			}
			vars += "[" + strings.Join(s.optional, " ") + "]"
		}
		fmt.Fprintf(&b, "  %-13s %s\n", n, vars)
	}
	return b.String()
}

// resolvers find the zone apex. Public servers, not the system's, so a stale
// local cache cannot send the records to the wrong zone.
var resolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// CreateRecords adds an address record pointing at ip for each of names, in
// whichever zone contains them. It only ever adds: the caller passes names
// that do not resolve at all, so nothing the operator set up is overwritten.
func CreateRecords(ctx context.Context, p Provider, names []string, ip netip.Addr) error {
	for _, name := range names {
		zone, err := certmagic.FindZoneByFQDN(ctx, zap.NewNop(), name, resolvers)
		if err != nil {
			return fmt.Errorf("finding the zone for %s: %w", name, err)
		}
		rec := libdns.Address{
			Name: libdns.RelativeName(name+".", zone),
			TTL:  300 * time.Second,
			IP:   ip,
		}
		if _, err := p.AppendRecords(ctx, zone, []libdns.Record{rec}); err != nil {
			return fmt.Errorf("creating %s in zone %s: %w", name, zone, err)
		}
	}
	return nil
}
