# hop

Self-hosted ngrok alternative. Expose a local port at `https://myapp.hop.vokh.dev`.

See [PLAN.md](PLAN.md) for the architecture and roadmap.

**Status: M1 complete** — HTTP tunnels work end to end in the clear on localhost.
TLS, the wildcard certificate and deployment are M2.

## Try it locally

Three terminals:

```sh
# 1. something to expose
python3 -m http.server 3000

# 2. the server
go run ./cmd/hopd -tokens dev-token

# 3. the agent
go run ./cmd/hop http 3000 --sub myapp --token dev-token
```

Then, since `*.localhost` doesn't resolve on macOS, send the Host header yourself:

```sh
curl -H "Host: myapp.localhost" http://127.0.0.1:8080/
```

Omit `--sub` and the server assigns a name like `brave-otter`.

## Ports

| Port  | Purpose                                            |
|-------|----------------------------------------------------|
| 8080  | public HTTP ingress (becomes 443 + TLS in M2)      |
| 7443  | agent control connections                          |

Not 7000: macOS binds it for AirPlay Receiver.

## Behaviour worth knowing

- **One agent per subdomain.** A second agent presenting the *same token* takes
  over the name — that's the reconnect path, so a dropped wifi connection doesn't
  cost you your URL for the ~45s it takes keepalive to reap the dead session. A
  different token is refused.
- **Reconnects keep their URL.** The agent remembers the assigned name and asks
  for it again, so a server-assigned `brave-otter` stays `brave-otter`.
- **Refusals are terminal.** A bad token or a name someone else holds exits
  rather than spinning.
- **The local app sees the public Host header** (`myapp.hop.vokh.dev`). Vite and
  a few other dev servers reject unknown hosts; `--host-header rewrite` is M4.
- Running two agents on one name with the same token makes them fight over it,
  each taking over on reconnect. Don't do that.

## Tests

```sh
go test -race ./...
```
