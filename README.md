# hop

Self-hosted ngrok alternative. Expose a local port at `https://myapp.hop.vokh.dev`.

See [PLAN.md](PLAN.md) for the architecture, [deploy/](deploy/) for running it
on a VPS.

**Status: M2** — HTTP tunnels over TLS, with a wildcard certificate obtained
automatically via ACME DNS-01. Not yet deployed.

## Using it

```sh
export HOP_SERVER=hop.vokh.dev:7443
export HOP_TOKEN=<your token>

hop http 3000              # https://brave-otter.hop.vokh.dev
hop http 3000 --sub myapp  # https://myapp.hop.vokh.dev
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
- **The local app sees the public Host header** (`myapp.hop.vokh.dev`). Vite and
  a few other dev servers reject unknown hosts; `--host-header rewrite` is M4.

## Tests

```sh
go test -race ./...
```

Covers the registry's claim/eviction logic, Host parsing, and full end-to-end
tunnels in both plaintext and TLS modes. The TLS test uses a throwaway CA rather
than skipping verification, so a broken chain fails the test.
