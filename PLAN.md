# hop (hostopen)

Self-hosted ngrok alternative. Go, yamux-over-TLS, HTTP tunnels.

Production: `https://myapp.hop.vokh.dev` → `localhost:3000`

## Architecture

The agent dials **out** to the VPS and holds a long-lived TLS connection. The VPS
pushes inbound requests back down that connection as multiplexed yamux streams.
This inverts NAT — the developer machine never needs an inbound port.

```
browser → https://myapp.hop.vokh.dev
            ↓ :443, wildcard TLS
        [ hopd on VPS ]
            │  registry: subdomain → *yamux.Session
            ↓  session.OpenStream()
        ═══ long-lived TLS conn on :7443 ═══
            ↓
        [ hop agent on laptop ]
            ↓ net.Dial("tcp", "localhost:3000")
        local app
```

### Ingress (server)

`httputil.ReverseProxy` with a transport that dials yamux streams instead of sockets:

```go
proxy := &httputil.ReverseProxy{
    Director: func(r *http.Request) { r.URL.Scheme = "http"; r.URL.Host = "tunnel" },
    Transport: &http.Transport{
        DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
            return session.OpenStream()
        },
    },
}
```

### Egress (agent)

For each accepted stream, dial the local port and `io.Copy` both directions.
Raw TCP splice — WebSockets, SSE, streaming bodies and chunked responses all
work with no extra code.

### Handshake

On the raw TLS conn, before yamux: length-prefixed JSON.

```go
type Hello struct { Token, Subdomain, Version string }
type HelloAck struct { URL, Subdomain string; Err string }
```

Then both sides upgrade to yamux. Note the roles are inverted vs. the TCP
direction: the agent dials, but the **server** opens streams, so `hopd` runs
`yamux.Client` and the agent runs `yamux.Server`. Roles only determine odd/even
stream ID assignment; either side may open.

## Layout

```
cmd/hop/            client CLI (cobra)
cmd/hopd/           server daemon
internal/proto/     Hello / HelloAck, framing
internal/server/    registry, ingress handler, control listener
internal/client/    dial, reconnect, local forward
internal/certs/     certmagic DNS-01 setup
```

Deps: `hashicorp/yamux`, `caddyserver/certmagic` (+ matching `libdns` provider),
`spf13/cobra`.

## Milestones

### M0 — DNS

- `*.hop.vokh.dev` A → VPS IP
- `hop.vokh.dev` A → VPS IP
- ufw: allow 80, 443, 7443

### M1 — End-to-end on localhost ✅ done

No TLS, no DNS. `Host: myapp.localhost:8080` → agent → `:3000`. This is the
entire product; everything after is hardening.

Verified: GET through the tunnel, 10MB body byte-identical, 50 concurrent
requests all 200, 502 when the local app is down, 404 for unknown and bare
hosts, bad token / taken name / invalid name all refused and terminal,
agent reconnect with backoff, same-token takeover of a live name.

Note: the control port is **7443**, not 7000 — macOS binds 7000 for AirPlay
Receiver, which makes local development confusing.

### M2 — Wildcard TLS + deploy ✅ live since 2026-08-13

Running at `https://<name>.hop.vokh.dev`, behind the Caddy that already serves
pkpways.vokh.dev on the same VPS. Verified end to end from a laptop: 200 through
the tunnel, valid production wildcard, 5MB body byte-identical, 20 concurrent
requests, correct X-Forwarded-Proto, hop's own 404 for unclaimed names.

Untested: WebSocket upgrades. The design should carry them (raw byte splice,
Caddy handles Upgrade natively) but nothing has demonstrated it.


**A wildcard cert can only be issued via DNS-01, not HTTP-01.** HTTP-01 and
TLS-ALPN-01 are disabled outright so a misconfiguration fails loudly rather than
quietly issuing a non-wildcard certificate.

Built:
- `internal/certs` — certmagic + `libdns/cloudflare`, managing both
  `hop.vokh.dev` and `*.hop.vokh.dev`
- `:80` → 301 to https, `:443` → TLS ingress, `:7443` → TLS agent control
- TLS on by default on both sides; `-tls=false` / `--no-tls` for local dev
- `deploy/hopd.service` + `deploy/README.md`
- End-to-end tests covering both the plaintext and TLS paths, the latter with a
  throwaway CA so a broken chain fails the test

The Cloudflare token needs **`Zone:Read` *and* `DNS:Edit`**. `DNS:Edit` alone
fails at zone lookup with a confusing error.

Deployed to the VPS, staging first, then production.

### M3 — Real sessions ✅ mostly landed early in M1

Done: registry with generation-guarded claims, token auth, `--sub` claiming,
reserved blocklist, random names, reconnect with backoff that re-claims the same
name, same-token takeover, keepalive, 404 for unknown names.

Remaining:
- Tokens from a file rather than a flag/env, so adding one doesn't need a restart
- `~/.hop.yaml` client config, so `--token` isn't needed on every invocation

### M4 — Polish

- Live request log in agent terminal: `GET /api/users 200 12ms`
- `--host-header rewrite` (Vite and some frameworks reject unknown Host values)
- systemd unit with `AmbientCapabilities=CAP_NET_BIND_SERVICE` — do not run as root
- `hop http 3000`, `hop http 3000 --sub myapp`

## Decisions

**Separate control port (:7443) rather than ALPN-muxing onto :443.** Simpler.
Cost: breaks behind firewalls that only permit 443. Irrelevant for personal use,
and ALPN mux can be added later without touching anything else.

**Auth is mandatory even solo.** An open tunnel service gets discovered and used
for phishing within days. A static token file is sufficient.

## Environment

- DNS: **Cloudflare** → `github.com/libdns/cloudflare` for the certmagic DNS-01 solver.
  Needs a scoped API token (not the global key) with **both** `Zone:Read` and
  `DNS:Edit` on vokh.dev. Keep the orange cloud **off** (DNS-only) for
  `*.hop.vokh.dev` — proxying through Cloudflare terminates TLS at their edge,
  which defeats the certificate and adds a hop you don't want.
- VPS: **Ubuntu** → systemd unit, `ufw` for firewall.

## Deferred (not in scope)

Raw TCP tunnels, accounts + Postgres, web dashboard, request inspector/replay,
custom domains, multi-region.
