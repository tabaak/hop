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
type Hello struct { Token, Op, Subdomain, Local, Version string }
type HelloAck struct { URL, Subdomain string; Err string }
type Listing struct { Tunnels []TunnelInfo; Err string }
```

`Op` picks what the connection is for: `tunnel` (the default, and what an empty
value means) or `list`, which answers `hop ps` and hangs up without upgrading to
yamux or claiming a name.

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
internal/inspect/   request capture, local inspector UI
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

Untested at the time: WebSocket upgrades — since covered in M4.


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

Tokens now come from a file (`-tokens-file`), one labelled line per device,
polled every 5s. `hopd mint <label>` generates a token and prints the line to
add. Only the SHA-256 is stored — plain, not bcrypt or argon2, because a
32-byte random secret has no dictionary to attack and a KDF would buy nothing
but handshake latency.

The label, not the secret, is the identity: it names the device in the log and
decides subdomain ownership. So rotating a token keeps the names its owner
holds, while separate devices can't evict each other. `HOP_TOKENS` still works
alongside the file, labelled `env-<hash prefix>` — derived from the hash rather
than from position, since a label that shifted when the list was reordered
would silently reassign ownership.

Revoking a line **closes that device's live tunnels** rather than only refusing
its next connection; the agent then reconnects once, is refused, and exits. A
file that fails to parse is rejected in favour of the loaded set, so a
truncated write costs a log line rather than every tunnel.

`hop ps` lists what is currently served — name, owning label, uptime, URL —
with `--json` for scripting. Once tunnels can come from several devices, no
single agent knows the answer, so it has to come from the server.

`hop http <port> -d` detaches, `hop log <name>` (with `-f`) reads what a
detached agent has been doing, and `hop stop <name>` / `--all` ends tunnels
running on this machine. The prompt comes back only once the tunnel is up, so a
refused token is still an error in the terminal rather than a background process
that quietly died.

Remaining:
- `~/.hop.yaml` client config, so `--token` isn't needed on every invocation

### M4 — Polish ✅ done

- Live request log in the agent terminal, on by default, `--quiet` to silence.
  Method and status are colourised, and each line carries the calling device
  derived from the User-Agent — the point being to tell your phone from your
  laptop when testing on both. Colour only when stderr is a terminal; honours
  `NO_COLOR` and `--no-color`.
- `--host-header preserve|rewrite|<literal>`
- systemd: the deployed unit needs no low ports at all (Caddy owns 80/443), so
  it keeps an empty `CapabilityBoundingSet`. The standalone recipe, with
  `AmbientCapabilities=CAP_NET_BIND_SERVICE`, is documented in the unit.
- `hop http 3000`, `hop http 3000 --sub myapp` — landed in M1

The log required teaching the agent a little HTTP, which it had deliberately
avoided. The compromise: read the request head, then splice the rest raw. That
is only sound because the server sets `DisableKeepAlives`, making each yamux
stream carry exactly one request — if that ever changes, the head parsing has
to become a loop. A head that is malformed or larger than 64KB falls back to a
plain splice, so a parse failure costs a log line rather than the connection.

Response status is captured by a sniffer that watches bytes flow past without
buffering them, so the reported duration is time-to-first-byte. Waiting for the
connection to close would report nothing until a WebSocket disconnected.

**WebSocket upgrades are now tested** (`TestUpgradeSurvivesTunnel`) — 101 plus
bidirectional traffic afterwards, using a hijacking handler rather than a real
WebSocket library so the suite gains no dependency. This closes the gap M2 left
open.

Known wart: hopd's own server-side log records 200 for an upgraded connection,
since `ReverseProxy` hijacks and the status recorder never sees the 101. The
agent-side log reports it correctly.

### M5 — Local request inspector ✅ done

`hop http <port> --inspect` serves a page on `127.0.0.1:4040`: the last 50
requests through the tunnel, headers and bodies both ways, live over SSE, with
**Replay request** and **Copy as cURL**.

Capture is a tee (`io.TeeReader`) into a bounded buffer, never a buffer of the
exchange — the same constraint the request log lives under, for the same
reason: a streaming response or a WebSocket must not be held back by something
watching it. Bodies are capped at 64KB per direction, and the record says what
was dropped rather than pretending it has everything.

What the hub stores are **snapshots**. An exchange writes to its own buffers
under its own mutex, and hands the browser side an immutable copy; nothing the
UI reads is written to again. That is what keeps `-race` quiet with a goroutine
per stream teeing into the same ring buffer.

Each exchange is published twice: once when the response head lands, which is
when the status is known and the duration means time-to-first-byte, and once at
close, which for a WebSocket is much later. The UI keys on the record id and
upserts, so an in-flight request appears immediately and fills in.

Replay dials the local port directly, bypassing the tunnel, and re-sends the
recorded bytes with `Connection: close` swapped in — and, importantly, **does
not half-close** the request afterwards. Half-closing is the tidy way to say
"that's all of it", and Go's `net/http` copes, but Node's HTTP server reads the
FIN as the client giving up and closes without answering at all: the replay
came back with no status and an empty response. Go-only tests missed it, so the
regression test uses a raw listener that aborts on half-close the way Node
does. A request whose body was truncated is **refused** rather than
replayed short: its `Content-Length` would no longer match and the local app
would hang until the deadline.

The listener is loopback, and the handler also **requires a loopback `Host`**.
Binding 127.0.0.1 alone doesn't stop a public page pointing a rebound name at
it and reading the answer, and what's in there is cookies and auth headers.

Deliberately not configurable: no port flag, no persistence, no capture rules.
If 4040 is taken the inspector says so and the tunnel carries on — losing the
debugging aid is not a reason to lose the tunnel.

**Attaching later (`hop inspect <name|pid>`).** `--inspect` had to be chosen at
start-up, which is the wrong moment: you open an inspector because something
already went wrong. So every agent now captures from birth and serves the same
handler on `~/.hop/run/<pid>.sock` — a unix socket rather than a TCP port,
since nothing should be listening on the network until someone asks for the
page, and the kernel enforces same-user access for free. The command binds 4040
itself and reverse-proxies over the socket with flushing enabled (the feed is
SSE), staying foreground like `hop log -f`; preserving the browser's Host keeps
the loopback guard meaningful at the far end. Capture-always costs one bounded
ring of fifty records and skips event encoding entirely while no browser is
subscribed — the alternative, capture-on-attach, would miss precisely the
requests worth inspecting.

Attaching also opens the page in the browser: the command exists to *look*,
and making you copy the URL out of its own banner was one step of ceremony for
every use. `$BROWSER` overrides the platform opener, `--no-open` opts out, and
a failure to launch (headless box, bare SSH session) is silent by design — the
URL is in the banner either way.

## Decisions

**Separate control port (:7443) rather than ALPN-muxing onto :443.** Simpler.
Cost: breaks behind firewalls that only permit 443. Irrelevant for personal use,
and ALPN mux can be added later without touching anything else.

**Auth is mandatory even solo.** An open tunnel service gets discovered and used
for phishing within days. A static token file is sufficient.

**One token per device, keyed on a label rather than the secret.** A single
shared token makes revocation an all-or-nothing event: you rotate, then hunt
down every copy. Labels make it one line to delete, and make the log say which
of your machines is connected. Keying ownership on the label rather than the
token bytes is what lets a secret be rotated without the server treating the
same device as a stranger and refusing it its own subdomain.

**The tokens file is polled, not watched with inotify.** No dependency, and it
survives the atomic-rename that editors and config management do — a watch
registered on the original inode would not. Five seconds of delay is
irrelevant for adding a device and acceptable for removing one.

**`hop ps` reuses the control port instead of adding an HTTP endpoint.** The
listing inherits the TLS and the token auth already protecting that port, and
needs nothing exposed through whatever fronts the ingress — which, behind
Caddy, would have meant a second thing to route and secure. The cost is one
field in `Hello` and one reply frame. Every valid token sees every tunnel:
labels distinguish an operator's devices, not tenants, and hiding other labels'
names would break the one question worth asking ("which machine is still
serving that?").

**A detached agent is tracked by a locked file, not by a PID file.** Every
running agent — detached or not — holds an advisory lock on
`~/.hop/run/<pid>.json` for its lifetime. Liveness is then "is the lock held?",
which the kernel answers correctly however the process died, including SIGKILL
and power loss. The obvious alternative, checking whether the PID exists, is
wrong in a way that only shows up later: after enough process churn or a reboot,
the number belongs to something else and `hop stop` signals a stranger.

Tracking foreground agents too is deliberate. `hop stop myapp` failing because
that tunnel happened to be started in a terminal would be a distinction the user
never made.

**A tunnel can be named by its local port.** The subdomain is what the server
hands out, the PID is what the process table knows — and neither is what you
remember about a tunnel started an hour ago. You remember that it was serving
`:8080`. So every command that takes a tunnel (`stop`, `log`, `inspect`)
resolves names, then PIDs, then ports: a number matches PIDs and ports
together, since until checked they are indistinguishable, and one record
matching both counts once. When a reference fits several tunnels — two subs
sharing a port, or a PID colliding with one — every match is listed and the
command refuses to pick; silently stopping the wrong tunnel is the failure mode
that must not happen quietly.

**The agent tells the server what it forwards to, and the server sanitises it.**
`Local` is carried in the handshake purely so a listing can show it; nothing
routes on it. It is also the only listing field that originates with a peer
rather than with the server, and it is printed straight into another operator's
terminal — so control characters are stripped and the length capped on the way
in. An agent that could inject `\r` or an ANSI sequence could forge a row in
somebody else's `hop ps`. The client applies the same cleaning to the addresses
it reads from its own state files, because a display path that trusts its input
is one hand-edited file away from the same problem.

**`hop stop` is local-only.** Reaching another device's agent would mean the
server could tell an agent to exit — a much larger idea, and one that turns a
listing credential into a remote kill switch. Naming a tunnel that belongs to
another device says so explicitly instead.

**`--no-tls` is refused for non-private peers rather than warned about.** The
token is the first thing written to a new connection, so plaintext leaks it to
anyone on the path. A confirmation prompt would be answered reflexively within
a week and would break non-interactive callers; a check is a guarantee. It runs
against the connected peer, not a resolved name, so a hostile resolver can't
answer loopback for the check and something public for the dial. No override
flag: SSH port-forwarding covers the remote-plaintext case and makes the peer
loopback honestly.

## Environment

- DNS: **Cloudflare** → `github.com/libdns/cloudflare` for the certmagic DNS-01 solver.
  Needs a scoped API token (not the global key) with **both** `Zone:Read` and
  `DNS:Edit` on vokh.dev. Keep the orange cloud **off** (DNS-only) for
  `*.hop.vokh.dev` — proxying through Cloudflare terminates TLS at their edge,
  which defeats the certificate and adds a hop you don't want.
- VPS: **Ubuntu** → systemd unit, `ufw` for firewall.

## Deferred (not in scope)

Raw TCP tunnels, accounts + Postgres, web dashboard, custom domains,
multi-region. (The request inspector landed in M5 — as a local page, not a
hosted dashboard.)
