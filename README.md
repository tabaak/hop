# hop

Self-hosted ngrok alternative. Expose a local port at `https://myapp.hop.vokh.dev`.

See [PLAN.md](PLAN.md) for the architecture, [deploy/](deploy/) for running it
on a VPS.

**Status: M2** — HTTP tunnels over TLS, with a wildcard certificate obtained
automatically via ACME DNS-01. Live at `*.hop.vokh.dev`.

## hop — the agent

One subcommand. Anything else prints usage and exits 2.

```sh
hop http <port> [flags]
```

| Flag                  | Default                                    |
|-----------------------|--------------------------------------------|
| `--sub <name>`        | server picks a random name                 |
| `--server <host:port>`| `$HOP_SERVER`, else `hop.vokh.dev:7443`    |
| `--token <token>`     | `$HOP_TOKEN`                               |
| `--local-host <ip>`   | `127.0.0.1`                                |
| `--host-header <v>`   | `preserve`; or `rewrite`, or a literal value |
| `--quiet`             | off; suppresses the request log            |
| `--no-tls`            | off; local development only                |

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
```

Requests are logged live to stderr as they complete:

```
  GET    200     12ms  /api/users
  POST   201      4ms  /api/users
  GET    404    0.8ms  /favicon.ico
  GET    101      2ms  /ws
```

The duration is **time to the first byte of the response**, not time to close.
For a WebSocket or an SSE stream those differ by the whole life of the
connection, and the latter would mean seeing nothing in the log until the user
navigated away.

`Ctrl-C` releases the name immediately.

Subdomains must match `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`, and a handful of
names are reserved (`www`, `api`, `admin`, `app`, `mail`, `hop`, …).

## hopd — the server

Standard Go flags, so `-domain` and `--domain` are equivalent. Tokens come from
`-tokens` or `HOP_TOKENS`; with neither, it refuses to start.

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
| `-tokens`       | `$HOP_TOKENS`         | comma-separated                              |
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
- **One agent per subdomain.** A second agent presenting the *same token* takes
  over the name — that's the reconnect path, so a dropped connection doesn't
  cost you your URL for the ~45s it takes keepalive to reap the dead session. A
  different token is refused.
- **Reconnects keep their URL.** The agent remembers the assigned name and asks
  for it again, so a server-assigned `brave-otter` stays `brave-otter`.
- **Refusals are terminal.** A bad token or a name someone else holds exits
  rather than spinning.
- **The agent verifies the server certificate** against the system roots. There
  is no skip-verify flag; the control connection carries your auth token.
- **The local app sees the public Host header** (`myapp.hop.vokh.dev`) unless
  you pass `--host-header`. That default is right for most apps — links and
  redirects they build point back through the tunnel — but dev servers with
  host allowlists reject it, which is what `--host-header rewrite` is for.
- **The agent parses only the request head.** Everything after it is a raw byte
  copy, which is what lets an upgraded connection carry arbitrary framing. This
  is safe only because the server disables keep-alives on its side of the
  tunnel, so each stream carries exactly one request.
- **Reconnects back off** from 1s to 30s. A session that survives 30s resets the
  backoff, so an overnight tunnel doesn't crawl after one blip.
- **A dead local app returns a readable 502** through the tunnel rather than a
  bare connection reset.

## Not yet

- **One tunnel per process.** Two ports means two terminals.
- **HTTP only.** No raw TCP, so no tunnelling Postgres or SSH.
- **The token must be in the environment or on the command line.** A
  `~/.hop.yaml` config is an open M3 item.
- **hopd's own server-side log reports 200 for upgraded connections.** The
  agent-side log gets this right; the server's status recorder doesn't see the
  101 because ReverseProxy hijacks the connection.

## Tests

```sh
go test -race ./...
```

Covers the registry's claim/eviction logic, Host parsing, and full end-to-end
tunnels in both plaintext and TLS modes. The TLS test uses a throwaway CA rather
than skipping verification, so a broken chain fails the test.
