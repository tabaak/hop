# hop

Self-hosted ngrok alternative. Expose a local port at `https://myapp.hop.vokh.dev`.

See [PLAN.md](PLAN.md) for the architecture, [deploy/](deploy/) for running it
on a VPS.

**Status: M2** — HTTP tunnels over TLS, with a wildcard certificate obtained
automatically via ACME DNS-01. Live at `*.hop.vokh.dev`.

## hop — the agent

Three subcommands. Anything else prints usage and exits 2.

```sh
hop http <port> [flags]   # open a tunnel
hop ps [flags]            # list the tunnels currently up (aliases: ls, status)
hop stop <tunnel>...      # stop tunnels running on this machine
hop log <tunnel>          # read a detached tunnel's output (alias: logs)
hop inspect <tunnel>      # open the request inspector for a running tunnel
```

A `<tunnel>` is named by its subdomain, its agent's PID, or the local port it
serves — `hop stop 8080` stops whatever is forwarding your `:8080`, which is
usually the one fact you still remember about a tunnel started an hour ago. A
number that fits more than one tunnel (two subs sharing a port, say) lists
them rather than picking.

| Flag                  | Default                                    | Applies to |
|-----------------------|--------------------------------------------|------------|
| `--sub <name>`        | server picks a random name                 | `http`     |
| `-d`, `--detach`      | off; runs in the background                | `http`     |
| `--local-host <ip>`   | `127.0.0.1`                                | `http`     |
| `--host-header <v>`   | `preserve`; or `rewrite`, or a literal value | `http`   |
| `--inspect`           | off; serves the request inspector on `127.0.0.1:4040` from startup | `http` |
| `--no-open`           | off; `hop inspect` opens the page in your browser | `inspect` |
| `--quiet`             | off; suppresses the request log            | `http`     |
| `--json`              | off; prints the listing as JSON            | `ps`       |
| `-a`, `--all`         | off; stops every tunnel on this machine    | `stop`     |
| `-n <count>`          | `50`; lines of log to show                 | `log`      |
| `-f`, `--follow`      | off; keeps printing as the agent writes    | `log`      |
| `--server <host:port>`| `$HOP_SERVER`, else `hop.vokh.dev:7443`    | `http`, `ps` |
| `--token <token>`     | `$HOP_TOKEN`                               | `http`, `ps` |
| `--no-color`          | off; also honours `NO_COLOR`               | all        |
| `--no-tls`            | off; refused unless the peer is private    | `http`, `ps` |

`stop` needs no token or server: it signals processes here.

Put the server and token in your shell profile once, and the everyday
invocation is two words:

```sh
export HOP_SERVER="hop.vokh.dev:7443"
export HOP_TOKEN="<your token>"
```

```sh
# Random name
hop http 3000
#   https://brave-otter.hop.vokh.dev  →  http://127.0.0.1:3000

# Claim a specific name
hop http 3000 --sub myapp
#   https://myapp.hop.vokh.dev  →  http://127.0.0.1:3000

# Two tunnels at once — separate terminals, separate names
hop http 3000 --sub web
hop http 8000 --sub docs

# Something running in a VM, a container, or elsewhere on the LAN
hop http 8080 --local-host 192.168.1.42

# A different token or server for a one-off, without editing the profile
hop http 3000 --token "$OTHER_TOKEN"
hop http 3000 --server staging.example.com:7443

# Vite and friends reject a Host they don't recognise — show them their own
hop http 5173 --host-header rewrite

# Or name the Host explicitly
hop http 3000 --host-header app.internal

# No request log
hop http 3000 --quiet

# With the request inspector on http://127.0.0.1:4040
hop http 3000 --inspect
```

Requests are logged live to stderr as they complete, with the calling device
worked out from the User-Agent:

```
  GET    200      5ms  iPhone Safari   /probe.txt
  GET    200      1ms  Mac Chrome      /
  GET    404      1ms  curl            /nope
  POST   201      4ms  Mac Chrome      /submit
  DELETE 204      2ms  Android Chrome  /item/7
  GET    101      2ms  Mac Safari      /ws
```

Method and status are colourised — green for `GET` and 2xx, blue for `POST`,
yellow for `PUT`/`PATCH` and 4xx, red for `DELETE` and 5xx, magenta for a 101
upgrade — so a failing request is findable without reading. Colour is emitted
only when stderr is a terminal, so redirecting to a file or piping into `grep`
gives plain text. `NO_COLOR` and `--no-color` both turn it off.

The device column is a **guess**. User-Agent strings imitate each other
relentlessly — Chrome's contains "Safari", Edge's contains both — so the checks
run most-specific first, and an unrecognised agent falls back to its leading
token.

The duration is **time to the first byte of the response**, not time to close.
For a WebSocket or an SSE stream those differ by the whole life of the
connection, and the latter would mean seeing nothing in the log until the user
navigated away.

`Ctrl-C` releases the name immediately.

### The request inspector

Every tunnel records its requests as they pass — a bounded tee into the last
50, in memory, on this machine only. `--inspect` serves that record as a page
on `http://127.0.0.1:4040` from the moment the tunnel starts; `hop inspect`
opens it for a tunnel that is already running, by name, PID or local port:

```sh
hop http 3000 --inspect     # page up from startup
hop inspect myapp           # or attach to a running tunnel
hop inspect 55899           # PIDs work too
hop inspect 3000            # so does the port the tunnel forwards
```

The page shows headers and bodies both ways, status, time to first byte, and
total duration. Methods are badged and statuses coloured on the same scheme as
the terminal log — green for `GET` and 2xx, blue for `POST`, amber for
`PUT`/`PATCH` and 4xx, red for `DELETE` and 5xx, purple for anything else and
for a 101 upgrade, `HEAD` and `OPTIONS` dimmed — so a request looks the same in
both places. The feed is live over server-sent events, so requests appear as
they arrive rather than on a refresh.

Because capture runs whether or not anyone is looking, an inspector attached
afterwards still shows the requests you missed — which is usually exactly why
you are opening it.

```sh
hop http 3000 --inspect

#   inspector  →  http://127.0.0.1:4040
#
#   https://myapp.hop.vokh.dev  →  http://127.0.0.1:3000
```

`hop inspect myapp` prints the same kind of banner, opens the page in your
browser (`$BROWSER` picks which; `--no-open` skips it), and stays in the
foreground, like `hop log -f`: Ctrl-C ends the page (the tunnel carries on),
and so does the tunnel ending — its records live in the agent's memory, so an
exited tunnel has nothing left to show. One page per machine, since 4040 is
fixed: a second `--inspect` or `hop inspect` says so rather than stealing it.

Two things you can do with a request once you have it:

- **Replay request** sends the recorded bytes straight to the local app,
  bypassing the tunnel — with `Connection: close` in place of whatever
  connection header the request carried, since it goes out on a connection of
  its own. Useful for the webhook that arrived once and failed:
  fix the handler, replay, repeat, without asking the sender to try again. The
  replay shows up in the feed marked as one.
- **Copy as cURL** builds the command against the public URL, so you can hand
  the request to a colleague or a shell script.

Worth knowing:

- **Capture is a tee, not a buffer.** Bytes are copied as they pass, so
  streaming responses, SSE and WebSockets are unaffected by watching them — a
  long-lived connection shows up with its status as soon as the head arrives,
  and its total duration when it closes.
- **Bodies are capped at 64KB per direction.** Past that the request still
  works in full; only the copy stops, and the record says how much was dropped.
  A request whose body was truncated can't be replayed, since its
  `Content-Length` would no longer match — the button says so.
- **A body that isn't valid UTF-8 is reported, not shown.** Its size is there;
  the bytes aren't.
- **Loopback only, in both directions.** The listener binds `127.0.0.1`, and
  requests whose `Host` isn't a loopback name are refused, so a public page
  can't read the inspector through a rebound DNS name. Everything it holds —
  cookies, auth headers, whole bodies — is exactly what must not leave the
  machine.
- **How `hop inspect` reaches a running agent** without either of them
  listening on an extra port: every agent serves the same inspector surface on
  a private unix socket under `~/.hop/run/`, reachable only by your user, and
  the command binds `4040` itself and proxies to it. The agent's loopback-Host
  guard survives the hop, so the page is no more exposed than `--inspect`'s.
- **If something already holds 4040**, both paths say so and the tunnel
  carries on without the page.

### Running in the background

`-d` puts the tunnel in its own session and gives the terminal back:

```sh
hop http 3000 --sub myapp -d
```

```
  https://myapp.hop.vokh.dev  →  http://127.0.0.1:3000

  detached, pid 55899. hop log myapp for the request log,
  hop inspect myapp to watch requests in a browser, hop stop myapp to end it.
```

The prompt returns only once the tunnel is **up**, not once the process has
started — so a bad token or an unreachable server is still an error in your
terminal, with the reason quoted from the agent's log, rather than a success
message followed by a process that quietly died. Until it has connected once, a
detached agent doesn't retry, for the same reason: at startup someone is waiting
to hear whether this worked. After it has been up, it reconnects with the usual
backoff and survives a server restart.

Closing the terminal doesn't take the tunnel with it.

### Reading a detached tunnel's log

```sh
hop log myapp          # last 50 lines
hop log myapp -n 200   # more
hop log myapp -f       # and keep watching
```

```
  GET    200      1ms  Mac Chrome      /
  GET    404      3ms  curl            /nope
```

The request log of a detached agent goes to `~/.hop/log/<pid>.log`; `hop log`
finds the right file from the name. Flags work on either side of the name.

**The file on disk is plain text and the colour is re-applied when you read
it.** A detached agent writes to a file, so it correctly emits no escape codes —
which would otherwise corrupt the log for `grep`, an editor, or anything else
that reads it. `hop log` knows where *its* output is going, so it colours the
method and status there, and stays plain when piped or when `NO_COLOR` is set.

Two cases it answers rather than failing vaguely:

- a **foreground** tunnel has no log file, and it says so — its output is in the
  terminal that started it
- an agent that has **exited** keeps its log, so `hop log <pid>` still works
  afterwards, which is when you most want to read one. `hop log` with no
  argument lists what's available.

Logs of finished agents are deleted after two weeks, pruned whenever a new
detached tunnel starts. Nothing else would ever remove them.

### Stopping tunnels

```sh
hop stop myapp        # by name, PID, or the local port it forwards
hop stop 8080         # "stop whatever is serving my :8080"
hop stop web docs     # several at once
hop stop --all        # or -a
```

```
  stopped myapp  pid 55899 → 127.0.0.1:13000
```

This works on **any** agent this machine is running, detached or not — a tunnel
started in another terminal is no harder to stop than a detached one. It sends
`SIGTERM`, waits five seconds, then `SIGKILL`s anything still there.

`hop stop` is deliberately local: `hop ps` lists what every device is serving,
but the process behind your phone's tunnel is on your phone. Naming one says so
rather than failing vaguely.

### What's up right now

An agent only knows about its own tunnel, so `hop ps` asks the server. `hop ls`
and `hop status` are the same command.

```sh
hop ps
```

```
  NAME        UP       PID      FORWARDS TO        URL
  calm-raven  38s      55901    127.0.0.1:3000     https://calm-raven.hop.vokh.dev
  myapp       2d3h     55899    192.168.1.42:8080  https://myapp.hop.vokh.dev

  2 tunnel(s) up, all on laptop.
```

`FORWARDS TO` is what each agent points at locally, which is how you tell two
tunnels apart when the names don't. The server can't work this out — the agent
reports it in the handshake, and nothing routes on it.

Columns appear only when they distinguish something. `OWNER` shows up once a
second device connects; `PID` once any listed tunnel is running here, which is
exactly the set `hop stop` can reach:

```
  NAME        OWNER   UP       PID      FORWARDS TO      URL
  blog        phone   4h12m    -        10.0.0.9:5173    https://blog.hop.vokh.dev
  calm-raven  laptop  38s      55901    127.0.0.1:3000   https://calm-raven.hop.vokh.dev
  myapp       laptop  2d3h     55899    127.0.0.1:3000   https://myapp.hop.vokh.dev

  3 tunnel(s) up.
```

That's the token label, so "which machine is still serving that, and can I shut
the laptop?" is answered in the table. Every valid token sees every tunnel:
labels name the devices of one operator, not separate tenants. `--json` always
carries `owner`, whatever the table shows, so a script's parsing doesn't change
when you mint a second token.

`--json` prints the server's answer verbatim for scripting:

```sh
hop ps --json | jq -r '.[] | select(.owner == "laptop") | .url'
```

It dials the same control port a tunnel does, with the same token over the same
TLS, and hangs up without claiming a name — so `hop ps` never appears in its own
output, and there is no HTTP endpoint to secure separately. Uptime is measured
on the server and sent as an elapsed time, so a VPS clock a few minutes out
can't produce a tunnel that started in the future.

Subdomains must match `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`, and a handful of
names are reserved (`www`, `api`, `admin`, `app`, `mail`, `hop`, …).

## hopd — the server

Standard Go flags, so `-domain` and `--domain` are equivalent. Tokens come from
`-tokens-file`, `-tokens` or `HOP_TOKENS`; with none of them, it refuses to
start.

| Flag            | Default               |                                              |
|-----------------|-----------------------|----------------------------------------------|
| `-domain`       | `hop.vokh.dev`        | zone tunnels live under                      |
| `-ingress`      | `:443`                | public ingress address                       |
| `-redirect`     | `:80`                 | HTTP→HTTPS redirect; empty disables          |
| `-control`      | `:7443`               | agent control address                        |
| `-ingress-tls`  | `true`                | terminate TLS on the ingress                 |
| `-control-tls`  | `true`                | terminate TLS on the control listener        |
| `-scheme`       | auto                  | `https` if `-ingress-tls`, else `http`       |
| `-public-port`  | empty                 | port appended to agent-facing URLs           |
| `-tokens-file`  | empty                 | labelled token hashes, reloaded when changed |
| `-tokens`       | `$HOP_TOKENS`         | comma-separated, unlabelled                  |
| `-email`        | empty                 | ACME account, for expiry notices             |
| `-staging`      | `true`                | staging CA; `false` for real certificates    |
| `-cert-dir`     | `/var/lib/hop/certs`  | ACME account key and certificates            |

```sh
# Standalone, owning :80 and :443, real wildcard certificate
hopd -domain hop.vokh.dev -email you@example.com -staging=false

# Behind a reverse proxy that already owns those ports
hopd -domain hop.vokh.dev -email you@example.com -staging=false \
    -ingress 172.17.0.1:8080 -ingress-tls=false -scheme https -control :7443
```

`-staging` **defaults to true**, so a hand-run `hopd` issues untrusted
certificates unless told otherwise. That's deliberate: Let's Encrypt allows five
duplicate certificates per week, and a misconfigured DNS token burns through
that in an afternoon.

`-scheme https` matters in the proxied form. The request arriving from the proxy
is plaintext, so without it hopd hands agents `http://` URLs and sends the wrong
`X-Forwarded-Proto` downstream.

The second form is what [`deploy/hopd.service`](deploy/hopd.service) runs, so in
production you drive it through systemd rather than by hand:

```sh
sudo systemctl restart hopd
sudo journalctl -u hopd -f
```

### Tokens

One token per device, in a file hopd re-reads whenever it changes:

```
# /etc/hop/tokens
laptop  sha256:260404a88f965b027ccaf72644869dfc0da6f36303bef89fbb5273ed19fc46ad
phone   sha256:00644a2931730760559c0be984bf4f8fedff582a3c9d2ff09cd34bf6dfda637b
ci      sha256:9f2b1c4d...
```

`hopd mint <label>` generates one and prints both halves — the token, which
goes to the device, and the line, which goes in the file:

```sh
hopd mint laptop
```

Only the hash is stored, so the file is safe to back up and a leaked copy
yields nothing usable. The price is that hopd can never show you a token again:
it is displayed once, at mint time, and a lost one is replaced rather than
recovered.

Plain SHA-256 is deliberate rather than an oversight. bcrypt and argon2 exist to
make *guessable* secrets expensive to attack; a 32-byte random token has no
dictionary to run against it, so a password KDF would add latency to every
handshake and nothing else.

**Adding or revoking a device needs no restart.** The file is polled every five
seconds:

```sh
# add — append the line hopd mint printed
echo 'phone  sha256:...' | sudo tee -a /etc/hop/tokens

# revoke — delete that device's line
sudo sed -i '/^phone /d' /etc/hop/tokens
```

Revoking **disconnects the device immediately** rather than only blocking its
next connection. An agent already holding a tunnel on a deleted credential is
closed within the poll interval, tries once to reconnect, and exits with
`tunnel refused: invalid token`. A revocation that let a stolen token keep
serving traffic for as long as its holder kept the socket open would not be
worth much.

The label is the identity everything else works in. It names the device in the
log:

```
agent 46.63.126.216:45174 (laptop): tunnel up for "myapp" (2 live)
```

and it decides who may take over a subdomain. Two devices with separate tokens
cannot evict each other's names — which is the point of issuing them
separately — while rotating the secret under an existing label keeps the names
that label holds.

A file that fails to parse is **rejected in favour of the set already loaded**,
and complained about once per edit. A truncated write mid-`vim` therefore costs
a log line rather than every live tunnel. The same protection is why an empty
file is refused: it is nearly always an accident, and to deny every agent at
once you stop the service.

`-tokens` and `HOP_TOKENS` still work, unchanged, so an existing deployment
keeps running while its tokens migrate. They carry no label, so hopd derives
one from the hash — `env-260404a8` — which is stable across restarts and
matches the beginning of the hash `hopd mint` prints.

## Local development

No certificates, no DNS, no VPS. Three terminals:

```sh
# 1. something to expose
python3 -m http.server 3000

# 2. the server, plaintext
go run ./cmd/hopd -ingress-tls=false -control-tls=false \
    -domain localhost -ingress :8080 -public-port 8080 -tokens dev-token

# 3. the agent, plaintext
go run ./cmd/hop http 3000 --sub myapp --token dev-token --server localhost:7443 --no-tls
```

`*.localhost` doesn't resolve on macOS, so send the Host header yourself:

```sh
curl -H "Host: myapp.localhost" http://127.0.0.1:8080/
```

TLS is **on by default** on both sides — a forgotten flag fails closed rather
than silently serving plaintext in production.

## Ports

| Port  | Purpose                                        |
|-------|------------------------------------------------|
| 443   | public HTTPS ingress                           |
| 80    | redirect to HTTPS                              |
| 7443  | agent control connections, TLS                 |

Not 7000: macOS binds it for AirPlay Receiver.

hopd also runs behind an existing reverse proxy that already owns 80/443:
`-ingress-tls=false -ingress 172.17.0.1:8080 -scheme https`. The proxy holds the
wildcard; hopd keeps its own TLS on 7443, since the control connection speaks
hop's protocol rather than HTTP and can't be proxied. See [deploy/](deploy/).

## Behaviour worth knowing

- **Wildcard certificates require DNS-01.** There is no HTTP-01 path to
  `*.hop.vokh.dev`. hopd disables the other challenge types outright so a
  misconfiguration fails loudly instead of quietly issuing a non-wildcard cert.
- **Staging is the default.** `-staging=false` gets real certificates. Let's
  Encrypt's production rate limits are easy to burn while you're still getting
  the DNS token right.
- **One agent per subdomain.** A second agent under the *same token label* takes
  over the name — that's the reconnect path, so a dropped connection doesn't
  cost you your URL for the ~45s it takes keepalive to reap the dead session. A
  different label is refused.
- **Reconnects keep their URL.** The agent remembers the assigned name and asks
  for it again, so a server-assigned `brave-otter` stays `brave-otter`.
- **Refusals are terminal.** A bad token or a name someone else holds exits
  rather than spinning.
- **The agent verifies the server certificate** against the system roots. There
  is no skip-verify flag; the control connection carries your auth token.
- **`--no-tls` only reaches private networks.** The agent writes its token as
  the first thing on a new connection, so plaintext puts the credential on the
  wire in the clear. The check runs against the *connected peer* rather than a
  resolved name — resolving separately from dialing is a TOCTOU gap — and it
  refuses before writing anything, so the token stays on your machine.
  Loopback, RFC 1918, IPv6 unique-local, link-local and Tailscale's
  `100.64.0.0/10` are permitted; everything else exits with an explanation.
  There is no override flag: to reach a remote plaintext hopd, forward a local
  port over SSH, which makes the peer loopback and passes naturally.
- **The local app sees the public Host header** (`myapp.hop.vokh.dev`) unless
  you pass `--host-header`. That default is right for most apps — links and
  redirects they build point back through the tunnel — but dev servers with
  host allowlists reject it, which is what `--host-header rewrite` is for.
- **The agent parses only the request head.** Everything after it is a raw byte
  copy, which is what lets an upgraded connection carry arbitrary framing. This
  is safe only because the server disables keep-alives on its side of the
  tunnel, so each stream carries exactly one request.
- **`hop ps` shows every device's tunnels**, not just this machine's, and needs
  a valid token to answer at all — which names are in use is worth as much to
  someone picking a target as it is to you.
- **Every running agent records itself** in `~/.hop/run/<pid>.json`, which is
  how `hop ps` fills the PID column and `hop stop` finds the process. Liveness
  is an advisory lock the agent holds on that file, not a check that its PID
  exists: the kernel drops the lock however the process dies, so a free lock
  proves the record is stale, while a recycled PID would eventually have made
  `hop stop` signal something unrelated. Records of dead agents are swept on the
  next `ps` or `stop`.
- **The forwarded address is sanitised before it is printed.** It is the one
  listing field a *peer* supplies, and it lands in another operator's terminal,
  so control characters are stripped and the length is capped — otherwise an
  agent could clear your screen or forge a row in your table.
- **Detaching is Unix-only.** It needs `setsid` and `flock`; `hop http` in the
  foreground has no such requirement.
- **Reconnects back off with full jitter** — a random wait drawn from a window
  that doubles from 1s to 30s, rather than the window itself. Agents that lost
  the same server lost it at the same instant, so waiting exactly 8s each just
  reconvenes the stampede. A session that carried a request, or that stayed up
  for 30s without one, resets the backoff — so an overnight tunnel doesn't
  crawl after one blip.
- **A dead local app returns a readable 502** through the tunnel rather than a
  bare connection reset.
- **A tunnel that drops keeps its URL** for 45 seconds, so the name a webhook
  is registered against is still yours when the agent reconnects. Requests
  arriving meanwhile get a **503 with `Retry-After`**, not a 404 — senders read
  a 404 as *this endpoint is gone*, drop the delivery, and in some cases
  disable the endpoint. A name nobody ever claimed is still a 404.

## Not yet

- **One tunnel per process.** Two ports means two processes — though with `-d`
  that no longer means two terminals.
- **HTTP only.** No raw TCP, so no tunnelling Postgres or SSH.
- **The agent's token must be in the environment or on the command line.** The
  server side now has a proper token file; the client side still means
  `HOP_TOKEN` in your shell profile. A `~/.hop.yaml` is the open M3 item.
- **The inspector is not configurable.** `--inspect`, `hop inspect` or nothing:
  no port flag, no persistence, no filter rules beyond the search box.
- **hopd's own server-side log reports 200 for upgraded connections.** The
  agent-side log gets this right; the server's status recorder doesn't see the
  101 because ReverseProxy hijacks the connection.

## Tests

```sh
go test -race ./...
```

Covers the registry's claim/eviction logic, Host parsing, the `hop ps` listing
(including that it authenticates and claims no name of its own), the
inspector's ring buffer, capture and replay — including that it doesn't stall
an upgraded connection — the socket-and-proxy path `hop inspect` serves a page
through (records, live events, replay, and the loopback guard surviving the
hop), and full end-to-end tunnels in both plaintext and TLS
modes. The TLS test uses a throwaway CA rather
than skipping verification, so a broken chain fails the test.
