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
type HelloAck struct { URL, Subdomain string; Err, Code string }
type Listing struct { Tunnels []TunnelInfo; Err string }
type Bye struct { Reason string }   // on its own stream, after the handshake
```

`Op` picks what the connection is for: `tunnel` (the default, and what an empty
value means) or `list`, which answers `hop ps` and hangs up without upgrading to
yamux or claiming a name.

`Code` classifies a refusal — `taken`, `bad-name`, `bad-token` — because the
agent's reaction differs: a name someone else holds is worth waiting out, a bad
token is worth exiting over. An empty code means "assume nothing will change",
which is what a server too old to send one produces and how every refusal was
treated before the field existed.

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

### M6 — Resilience & self-healing ✅ done

A tunnel should survive a Wi-Fi switch, a closed lid, or a minute of packet
loss without anyone typing anything. The agent already reconnects; what it
doesn't do is keep its name, and a URL that changes under you is worse than a
tunnel that dropped — the drop you notice, the rename silently invalidates the
endpoint you gave Stripe an hour ago.

**The invariant**, stated so the scope stops sliding: *for a given owner label,
the mapping `subdomain → that owner` survives any transient loss of the
transport, and a request arriving in the gap is told to come back rather than
told the endpoint is gone.* Everything below is the least mechanism that holds
it. Anything that doesn't serve it is in "not in scope" at the bottom.

#### The lease is a tombstone, not a subsystem

`Reserve` already does the interesting half: a same-owner reclaim of a live
name evicts the predecessor and hands the name over. That *is* the reconnect
path. The only thing that breaks it is `defer s.reg.Release(sub, gen)` in
`handleAgent`, which deletes the claim the moment yamux notices the session is
gone.

So M6's registry change is not a lease manager. On an **unexpected** exit the
entry stays: `tunnel` goes nil, `expires` is stamped, `owner` is untouched.
`Lookup` keeps treating it as absent — nothing may be routed to a name with no
session behind it — and the claim frees itself when the window passes.

Expiry is a `time.AfterFunc` calling `Release(sub, gen)`, not a reaper
goroutine. The generation guard that already protects against a slow-exiting
predecessor protects against a stale timer for free: a successor that reclaimed
the name holds a newer `gen`, so the old timer's `Release` no-ops. A reaper
scanning the map would be the third party that makes `gen` insufficient.

`Reserve` grows a three-way branch where it currently has one:

- unbound and **never leased** — a claim still mid-handshake — stays `ErrTaken`,
  even for the same owner. Two of your own agents must not be able to steal
  each other's half-built claims.
- **leased, unexpired, same owner** — reclaim: new generation, same name,
  nothing to evict.
- **leased and expired** — the name is free, for anyone.

**Grace window: 45s.** The number is arbitrary and worth saying so. Nothing
takes the name from you but you; the window exists only to bound how long a
dead laptop can squat a name, not to win a race.

#### Three latent bugs this trips, all of which must land first

1. **A naive lease bricks the agent.** `Reserve` returns `ErrTaken` for an
   unbound entry, `client.Run` wrapped any `ack.Err` as `ErrRefused`, and
   `runHTTP` treated `ErrRefused` as fatal. Ship the lease on top of that and
   the first Wi-Fi switch is: hold the name → agent reconnects → refused by its
   own lease → `os.Exit(1)`. A feature whose entire effect is converting a
   survivable blip into a hard exit.

   **The agent half of this is done**, ahead of any lease code, since it is the
   half that has to exist first. Refusals now carry a `Code`, and a refusal is
   still terminal *unless* it is a taken name met by an agent that had already
   been up — that agent is reclaiming a name whose holder is almost certainly
   its own predecessor. The private-peer check in `peer.go` wraps `ErrRefused`
   without a code and stays terminal, which is why the test is `errors.Is` for
   the refusal and `errors.As` for the retry, rather than one or the other.
   The `Reserve` half lands with the tombstone below: until an entry can be
   leased there is no new state for it to branch on.
2. **Lease only what actually bound.** The old `defer` fired on every exit
   path, including a refused claim and a failed yamux upgrade. A `hold` flag
   set only after `Bind` keeps those instant.

   The scenario this was justified with — a crash-looping agent locking itself
   out, one fresh window per attempt — turns out not to exist, and the reason
   is worth recording: a hold never blocks *its own owner*, so an agent that
   dies and comes straight back reclaims its name every time. What the flag
   actually buys is that a claim which was refused, or which never became
   reachable, doesn't take a name out of circulation for everyone else. Note
   also that `yamux.Client` does not fail on an already-dead connection, so the
   unbound-exit path is nearly unreachable in practice; the flag is about the
   guarantee, not about a case seen in the wild.
3. **Revocation stops reaching leased names.** `CloseRevoked`, `Snapshot` and
   `Count` all iterated `e.tunnel != nil`, so a tombstone was invisible to all
   three: deleting a leaked token would leave that label's subdomains held
   anyway. `CloseRevoked` now sweeps held names too — and takes the write lock
   to **delete before closing** rather than after. That order is the fix, not a
   tidiness: closing a tunnel wakes the goroutine serving it, whose last act is
   to lease the name, which would hand a revoked credential its subdomain back
   for another 45s. `Lease` is generation-guarded against an entry that is
   gone, so removing the claim first is what makes revocation stick.

   Same class of bug: `Reserve`'s random-name loop tested `_, exists :=
   r.entries[c]`, so expired tombstones would have permanently shrunk the
   namespace. Both that loop and the claim path now treat an expired entry as
   absent, whichever way the timer happens to have gone — so no answer depends
   on timer latency.

   `Snapshot` and `Count` still skip held names, deliberately: `hop ps` lists
   what is serving, and a name with nothing behind it is not.

#### Ingress during the gap: 503, not 404 ✅

A 404 tells a webhook sender the endpoint is gone, which is the outcome the
milestone exists to prevent. When `Lookup` misses but the name is leased, hopd
answers **503 with `Retry-After: 5`** and a body naming hop and the subdomain,
so it reads as a tunnel being down rather than a URL being wrong.

Worth being honest about what that buys: `Retry-After` is widely ignored, and
senders differ — some retry on any non-2xx, so the status barely matters to
them; GitHub's repository webhooks don't redeliver automatically at all, so
that delivery is lost either way. 503 is still right, because repeated 404s are
what get an endpoint auto-disabled, and because it is the truthful answer.

**Requests are not parked.** Holding client sockets waiting for an agent to
come back sounds like it saves the delivery, and doesn't: parking only engages
once `Lookup` misses, but the server doesn't know the session is dead for ~45s.
For that whole window `Lookup` returns a live `*Tunnel`, ingress opens a stream
on a corpse, and the request hangs into `ConnectionWriteTimeout` and 502s — the
park never runs. Fixing that means faster detection, and then the ceiling is
the sender's own ~10s timeout, not our grace window. Unbounded held sockets on
a single VPS for a few seconds of a sender's patience is a bad trade.

Which means the arithmetic must be stated plainly: server-side detection is
still keepalive-bound, so a 45s lease is really a **~90s worst-case name hold**.

#### Clean exit: `Bye` on a yamux stream ✅

Ctrl-C and `hop stop` must free the name now, not in 45s. The agent opens one
stream and writes a `Bye` frame before closing the session; `handleAgent`
releases instead of leasing when it saw one.

It goes **on a stream, not on a new dial**, for a specific reason. Unknown ops
are not ignored — `handleAgent` branches on `OpList` and everything else falls
through to the tunnel path, so an `Op:"release"` dial against an older hopd
would be read as a tunnel request: it would *reserve* the name and ack it. That
is exactly the hazard `Listing.URL` exists to warn about. A stream is safe for
the mirror-image reason: the server never calls `sess.Accept()` today, so an
old server discards the Bye silently and an old agent simply never sends one.
No `Version` bump, no skew to design around.

**Correctness never depends on the goodbye arriving.** A lost Bye — SIGKILL, a
panic, a half-dead connection — degrades to the lease expiring normally, which
is today's behaviour. The write gets a short deadline of its own, since a
`Bye` blocked for the full `ConnectionWriteTimeout` would hang the shutdown it
was added to speed up.

Ordering on the server is the part that needed care: the goodbye is recorded
*before* the session close, and that close is what wakes `Wait`. Had the agent
closed its own session after writing, `handleAgent` would have raced the
goroutine reading the frame and held the name of an agent that had just asked
it not to.

Two things only a real run surfaced, both now covered:

- **`Say` ends the session, which makes `Run` return exactly as a drop does.**
  The loop dialled straight back and re-claimed the name it had just handed
  over — visible in `hopd`'s log as a `tunnel down` followed immediately by a
  `tunnel up`. `Farewell.Said` is what the loop asks to tell the two apart.
- **The re-raise is not guaranteed to kill.** A background job started by a
  non-interactive shell inherits SIGINT ignored; `Notify` overrides that so the
  handler runs, but `Reset` restores the ignore and the re-raise does nothing.
  `cleanupOnSignal` now returns a channel that closes when the handler is done,
  so the loop exits on that rather than on being killed. In a terminal the
  process still dies inside the wait, with the exit status the shell expects.

#### Detection and backoff

Keepalive **stays at 30s**. Shortening it makes every phone and every metered
connection pay for the lid-close case, and the server holds the lease either
way, so faster detection buys only a shorter 503 window.

Backoff becomes full jitter: `sleep = rand(0, min(30s, 1s << attempt))`, pulled
out into a pure function so it can be tested without waiting. The attempt
counter is capped, and not for tidiness: `1s << 34` overflows to a negative
duration and `rand.Int63n` panics on it, which an agent left running through a
long outage would reach.

The reset rule is `served || elapsed > 30s`, and it needs both halves. Elapsed
time alone believes a server that accepts connections and then ignores them.
Traffic alone punishes an idle webhook endpoint that sat there correctly for
six hours without being called — which is the ordinary state of the thing this
tool exists to serve, so "reset only on work done" would have been a
regression. Elapsed time is monotonic, and darwin's monotonic clock doesn't
advance across sleep: a session that spanned a closed lid is judged on the part
of it that was awake, which is the only part that says anything about health.

The agent never gives up on a tunnel that was once up. A bad token still exits.

If a wall-clock-jump detector for sleep/wake lands later, note that the obvious
version doesn't work: `time.Since` won't see the jump. It needs
`time.Now().Round(0)` to strip the monotonic reading and compare the wall delta
against the monotonic one.

#### What it looks like

The drop is narrated, because ~30s of silence after an error is
indistinguishable from a crash: what dropped, that the URL is held and for how
long, and when the next attempt is. On recovery, how long it was down.

The one thing that must be loud is a **changed name**. `OnUp` currently
rewrites the state file with a new subdomain and prints nothing, which is the
single most expensive surprise this tool can produce — it silently invalidates
whatever the old URL was registered with. If the name changed, say so where it
can't be missed.

`hopd` logs the lease and its expiry, the reclaim, and the release, so a name
that is held has a reason on the server. A refusal for a name held by someone
else can say so, and for how long.

#### Order of work

Backoff first ✅: a pure function, no protocol, ships alone. Then refusal codes
and the non-terminal reclaim ✅ — before any lease code exists, since until that
lands every other piece here converts a blip into an exit. Then the tombstone,
the `Reserve` branch, the `hold` flag, and the revocation/random-name sweeps
together ✅. Then 503 ✅. Then `Bye` last ✅, as the only piece that adds a frame.

Tests, all in the existing harnesses:

- `nextBackoff` — bounds over a thousand draws, cap holds, no panic at a large
  attempt count, and the reset rule.
- `registry_test.go`, beside `TestReserveEvictsOwnSessionOnReconnect`: reclaim
  of one's own lease; another owner refused while it holds; expiry frees the
  name; `Lookup` treats a lease as absent; a stale timer cannot drop a
  successor; a mid-handshake claim still refuses its own owner; revocation
  clears leases; the random-name loop skips expired ones.
- `integration_test.go`, beside `TestUnknownSubdomainIs404`: a leased name
  answers 503; a hard-closed connection reconnects onto the same subdomain;
  `Bye` frees the name immediately. The harness needs one hook — a handle on
  the agent's `net.Conn` — so a test can kill the transport the way a network
  does, rather than closing the session politely.

#### Not in scope

Parking requests, per-lease resume secrets (the label already gates eviction of
*live* tunnels, so a nonce guarding dead ones defends nothing), device handoff,
persisting leases across a `hopd` restart, reserved/durable names, OS
network-change notifications, a shorter keepalive, a persistent status line, and
a state column in `hop ps`. Each is a reasonable feature; none of them is this
invariant.

One thing genuinely missing and worth a follow-up: two devices sharing a label
can now evict each other indefinitely, each reconnecting and re-evicting, which
is a permanently flapping URL. The fix is an instance id and refusing the loser
outright rather than letting it spin.

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
