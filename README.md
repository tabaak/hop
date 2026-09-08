# hop

A fast, lightweight, self-hosted **ngrok alternative**. Expose local ports to the internet with automated TLS, custom subdomains, live terminal request logs, and a built-in web request inspector.

```
https://myapp.hop.vokh.dev  →  http://127.0.0.1:3000
```

---

## ✨ Features

- **🚀 Instant Public HTTPS:** Automatic Let's Encrypt wildcard TLS certificate over DNS-01.
- **🔍 Built-in Request Inspector & Replay:** Watch requests live in your browser (`http://127.0.0.1:4040`), inspect headers/bodies, copy as cURL, or replay failed webhooks locally with one click.
- **🪵 Beautiful Terminal Logs:** Color-coded request logs with method, status, client device detection, and time-to-first-byte response timing.
- **⚡ Background Daemon Mode:** Detach tunnels into the background with `-d`, inspect or tail their logs with `hop log`, and stop them cleanly with `hop stop`.
- **📱 Multi-Device Awareness:** `hop ps` lists active tunnels across all your registered devices (laptop, desktop, staging servers).
- **🛡️ Resilient Reconnects:** Network drops keep your subdomain reserved for 45 seconds (returning HTTP 503 `Retry-After` to callers rather than 404), with full jitter exponential backoff.
- **🔒 Secure by Design:** TLS required on public networks, unguessable 32-byte auth tokens stored only as SHA-256 hashes, and local inspector protected by loopback Host validation.

---

## 📦 Installation

### Homebrew (macOS / Linux)

```sh
brew install <username>/tap/hop
```

*(Or tap your repository and install):*
```sh
brew tap <username>/hop
brew install hop
```

### Go Install

If you have Go 1.22+ installed:

```sh
go install hop.vokh.dev/cmd/hop@latest
```

### From Source

```sh
git clone https://github.com/<username>/hop.git
cd hop
go build -o hop ./cmd/hop
sudo mv hop /usr/local/bin/
```

Verify your installation:
```sh
hop version
# hop v1.0.0
```

---

## 🚀 Quickstart

### 1. Set your credentials

Add the server address and your agent token to your shell profile (`~/.zshrc` or `~/.bashrc`):

```sh
export HOP_SERVER="hop.vokh.dev:7443"
export HOP_TOKEN="<your-device-token>"
```

### 2. Open a tunnel

Expose any local port in two words:

```sh
hop http 3000
```

```
  https://calm-raven.hop.vokh.dev  →  http://127.0.0.1:3000

  GET    200      5ms  Mac Chrome      /
  POST   201      4ms  iPhone Safari   /api/checkout
```

Press `Ctrl-C` to gracefully shut down the tunnel and release your subdomain immediately.

---

## 💻 Everyday Usage

### Claim a Custom Subdomain

```sh
hop http 3000 --sub myapp
# https://myapp.hop.vokh.dev  →  http://127.0.0.1:3000
```

### Working with Modern Dev Servers (Vite, Next.js, Rails)

Frameworks like Vite reject requests with unrecognized `Host` headers. Use `--host-header rewrite` to seamlessly forward the local host header:

```sh
hop http 5173 --host-header rewrite
```

Or specify an explicit host:
```sh
hop http 3000 --host-header app.internal
```

### Forward to LAN IPs, VMs, or Docker Containers

Forward traffic to another machine on your local network:

```sh
hop http 8080 --local-host 192.168.1.42
```

### Running Tunnels in the Background

Run with `-d` (or `--detach`) to immediately return terminal control while keeping the tunnel active:

```sh
hop http 3000 --sub myapp -d
```

```
  https://myapp.hop.vokh.dev  →  http://127.0.0.1:3000

  detached, pid 55899. hop log myapp for the request log,
  hop inspect myapp to watch requests in a browser, hop stop myapp to end it.
```

- The command returns only once the tunnel is confirmed **up** on the server.
- Closing your terminal or laptop lid will not terminate background tunnels.

### Inspecting Background Logs

```sh
hop log myapp          # View last 50 log lines
hop log myapp -n 200   # View last 200 lines
hop log myapp -f       # Stream logs in real-time (follow)
```

Log files are stored at `~/.hop/log/<pid>.log` and cleaned up automatically after two weeks.

### Managing & Stopping Tunnels

Stop running tunnels by name, port, or PID:

```sh
hop stop myapp        # By subdomain name
hop stop 3000         # "Stop whatever is forwarding port 3000"
hop stop 55899        # By process PID
hop stop web docs     # Multiple tunnels at once
hop stop --all        # Stop every tunnel on this machine
```

### Checking Active Tunnels (`hop ps`)

See all active tunnels across all your devices (`hop ls` and `hop status` are aliases):

```sh
hop ps
```

```
  NAME        OWNER   UP       PID      FORWARDS TO      URL
  blog        phone   4h12m    -        10.0.0.9:5173    https://blog.hop.vokh.dev
  calm-raven  laptop  38s      55901    127.0.0.1:3000   https://calm-raven.hop.vokh.dev
  myapp       laptop  2d3h     55899    127.0.0.1:3000   https://myapp.hop.vokh.dev

  3 tunnel(s) up.
```

Output as JSON for shell scripting or automation:

```sh
hop ps --json | jq -r '.[] | select(.owner == "laptop") | .url'
```

---

## 🔍 Request Inspector

Every tunnel continuously records its last 50 HTTP exchanges in memory. You can launch the web inspector interface anytime:

```sh
# Start tunnel with inspector opened from launch:
hop http 3000 --inspect

# Or attach inspector to an already-running tunnel:
hop inspect myapp
hop inspect 3000
```

The inspector runs on `http://127.0.0.1:4040` and provides:
- **Live Stream:** Real-time request and response feeds via Server-Sent Events (SSE).
- **Headers & Bodies:** Full request and response inspection (up to 64KB per payload).
- **🔁 Replay Request:** Send recorded requests directly to your local application with one click—perfect for debugging webhooks without asking the sender to re-fire.
- **📋 Copy as cURL:** Export public cURL commands for reproduction or sharing.
- **Security:** Binds strictly to `127.0.0.1` and blocks non-loopback `Host` headers to prevent DNS rebinding attacks.

---

## 🛠️ CLI Reference

### Commands

| Command | Description |
|---------|-------------|
| `hop http <port> [flags]` | Open a public tunnel to a local port |
| `hop ps [flags]` | List all running tunnels (`aliases: ls, status`) |
| `hop stop <tunnel>...` | Stop tunnels running on this machine |
| `hop log <tunnel> [flags]` | View output of a background tunnel (`alias: logs`) |
| `hop inspect <tunnel>` | Open the web request inspector in your browser |
| `hop version [flags]` | Print version information (`aliases: --version, -v`) |

### Flags

| Flag | Default | Applies To | Description |
|------|---------|------------|-------------|
| `--sub <name>` | *(random)* | `http` | Request a custom subdomain |
| `-d`, `--detach` | `false` | `http` | Run agent in the background |
| `--local-host <ip>` | `127.0.0.1` | `http` | Local host to forward to |
| `--host-header <v>` | `preserve` | `http` | Host header sent to local app (`preserve`, `rewrite`, or literal) |
| `--inspect` | `false` | `http` | Serve request inspector on `http://127.0.0.1:4040` |
| `--quiet` | `false` | `http` | Suppress terminal request logs |
| `--no-open` | `false` | `inspect` | Do not open browser automatically |
| `-n <count>` | `50` | `log` | Number of log lines to show |
| `-f`, `--follow` | `false` | `log` | Stream log output continuously |
| `-a`, `--all` | `false` | `stop` | Stop all tunnels running on this machine |
| `--json` | `false` | `ps` | Output tunnel listing as JSON |
| `-s`, `--short` | `false` | `version` | Print bare version number |
| `--server <addr>` | `hop.vokh.dev:7443` | `http`, `ps` | Control server address (or `$HOP_SERVER`) |
| `--token <token>` | `$HOP_TOKEN` | `http`, `ps` | Agent auth token |
| `--no-color` | `false` | all | Disable ANSI colors (also honors `$NO_COLOR`) |
| `--no-tls` | `false` | `http`, `ps` | Connect without TLS (allowed on private/loopback networks only) |

---

## 🖥️ Server Deployment (`hopd`)

`hopd` is the server daemon that runs on your VPS or cloud instance. It manages public HTTP/HTTPS ingress, issues wildcard certificates via Let's Encrypt DNS-01, and multiplexes agent tunnels over Yamux.

### Quick Deployment Overview

See **[`deploy/`](deploy/)** for the step-by-step production deployment guide (systemd service, Cloudflare DNS-01 tokens, and unprivileged user setup).

```sh
# Run standalone (terminates TLS on 80 & 443 with wildcard cert)
hopd -domain hop.vokh.dev -email you@example.com -staging=false

# Run behind an existing reverse proxy (e.g. Caddy/Nginx)
hopd -domain hop.vokh.dev -email you@example.com -staging=false \
    -ingress 172.17.0.1:8080 -ingress-tls=false -scheme https -control :7443
```

#### Server Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-domain` | `hop.vokh.dev` | Base domain tunnels live under |
| `-ingress` | `:443` | Public HTTP/HTTPS ingress address |
| `-redirect` | `:80` | HTTP→HTTPS redirect listener (empty disables) |
| `-control` | `:7443` | Agent control listener address |
| `-ingress-tls` | `true` | Terminate TLS on ingress (`false` when behind reverse proxy) |
| `-control-tls` | `true` | Terminate TLS on control listener |
| `-scheme` | `https` (auto) | Public scheme exposed to agents (`http` or `https`) |
| `-public-port` | *(empty)* | Optional external port appended to tunnel URLs |
| `-tokens-file` | *(empty)* | Path to file with labelled token hashes (`/etc/hop/tokens`) |
| `-tokens` | `$HOP_TOKENS` | Comma-separated token strings (unlabelled fallback) |
| `-email` | *(empty)* | ACME contact email for Let's Encrypt expiry notices |
| `-staging` | `true` | Use Let's Encrypt Staging CA (`false` for production) |
| `-cert-dir` | `/var/lib/hop/certs` | Directory for ACME account keys and certificates |


### Managing Device Tokens

Agent credentials are authenticated via hashed tokens in `/etc/hop/tokens`:

```sh
# Generate a new token for your laptop:
hopd mint laptop
```

This displays the token for the device and appends the SHA-256 hash to `/etc/hop/tokens`.
- **Zero-downtime reloads:** The file is polled every 5 seconds; tokens can be added or revoked without restarting the server.
- **Immediate revocation:** Deleting a token line terminates active tunnels instantly.

---

## 🧪 Local Development

You can test Hop entirely on your machine without a VPS, domain, or TLS certificates:

```sh
# Terminal 1: Run a dummy local web service
python3 -m http.server 3000

# Terminal 2: Run hopd server locally
go run ./cmd/hopd -ingress-tls=false -control-tls=false \
    -domain localhost -ingress :8080 -public-port 8080 -tokens dev-token

# Terminal 3: Connect the hop client
go run ./cmd/hop http 3000 --sub myapp --token dev-token --server localhost:7443 --no-tls
```

Test the connection:
```sh
curl -H "Host: myapp.localhost" http://127.0.0.1:8080/
```

---

## 🧠 Architecture & Design Highlights

- **DNS-01 ACME Wildcards:** Wildcard TLS certificates (`*.yourdomain.com`) are generated automatically via Let's Encrypt DNS-01 challenge.
- **45-Second Grace Hold:** If an agent temporarily drops or switches Wi-Fi networks, its subdomain is held for 45 seconds. Inbound webhooks receive HTTP 503 (`Retry-After`) rather than 404, preventing webhook providers from disabling endpoints.
- **Instant Intentional Teardown:** Explicit exits (`Ctrl-C` or `hop stop`) send a protocol `Bye` frame, releasing subdomains immediately so they can be reused without delay.
- **Full Jitter Exponential Backoff:** Automatic reconnects back off with randomised jitter to avoid thundering herd spikes when a server reboots.
- **Private Peer Plaintext Guard:** `--no-tls` is strictly blocked unless connecting to private or loopback IP ranges (RFC 1918, CGNAT, Tailscale, IPv6 ULA) to prevent credential leakage.

---

## 🧪 Tests

Run the comprehensive test suite with the race detector:

```sh
go test -race ./...
```

Covers token minting/revocation, registry claims, yaml/mux protocol framing, request logging, inspector ring buffers, SSE live feeds, request replay, and end-to-end TLS tunnels.
