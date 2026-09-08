# hop

A fast, lightweight, self-hosted **ngrok alternative**. Expose local ports to the internet with automated Let's Encrypt wildcard TLS, custom subdomains, live terminal request logs, and a built-in web request inspector.

```
https://myapp.hop.vokh.dev  →  http://127.0.0.1:3000
```

---

## ✨ Features

- **🚀 Instant Public HTTPS:** Automatic Let's Encrypt wildcard TLS certificate over DNS-01.
- **🔍 Built-in Request Inspector & Replay:** Watch requests live in your browser (`http://127.0.0.1:4040`), inspect headers/bodies, copy as cURL, or replay failed webhooks locally with one click.
- **🪵 Live Terminal Logs:** Color-coded request logs with HTTP method, status, client device detection, and time-to-first-byte measurements.
- **⚡ Background Daemon Mode:** Detach tunnels into the background with `-d`, inspect or tail their logs with `hop log`, and stop them cleanly with `hop stop`.
- **📱 Multi-Device Awareness:** `hop ps` lists active tunnels across all your registered devices (laptop, desktop, staging servers).
- **🛡️ Resilient Reconnects:** Temporary network drops keep your subdomain reserved for 45 seconds (returning HTTP 503 `Retry-After` to webhook callers rather than 404), with full-jitter exponential backoff.
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

```sh
go install hop.vokh.dev/cmd/hop@latest
```

### From Source

```sh
git clone https://github.com/<username>/hop.git
cd hop
go build -o hop ./cmd/hop
sudo install -m 755 hop /usr/local/bin/hop
```

Verify your installation:
```sh
hop version
# hop v1.0.0
```

---

## 🎯 How Hop Works (Client vs. Server)

Hop consists of two binaries:
1. **`hop` (The Agent CLI):** Installed on your workstation/laptop. It connects out to a Hop server over TLS and tunnels local traffic.
2. **`hopd` (The Server Daemon):** Runs on a VPS with a public IP. It handles public HTTP/HTTPS ingress, manages the wildcard TLS certificate via DNS-01, and multiplexes client tunnels.

> [!NOTE]
> **Using an existing Hop server?** If your team or organization already runs `hopd`, simply ask your administrator for the server address and a minted token, then jump straight to the **[Quickstart](#-quickstart)**.
>
> **Setting up your own server?** Hop is self-hosted—there is no shared public SaaS cluster. If you are running your own infrastructure, see the **[Self-Hosting Guide (`hopd`)](#-self-hosting-guide-hopd)** below before running the client.

---

## 🚀 Quickstart

### 1. Set your credentials

Add the server address and your agent token to your shell profile (`~/.zshrc` or `~/.bashrc`):

```sh
export HOP_SERVER="hop.yourdomain.com:7443"
export HOP_TOKEN="<your-device-token>"
```

### 2. Open a tunnel

Expose any local port in two words:

```sh
hop http 3000
```

```
  https://calm-raven.hop.yourdomain.com  →  http://127.0.0.1:3000

  GET    200      5ms  Mac Chrome      /
  POST   201      4ms  iPhone Safari   /api/checkout
```

Press `Ctrl-C` to gracefully shut down the tunnel and release your subdomain immediately.

---

## 💻 Everyday Agent Usage (`hop`)

### Claim a Custom Subdomain

```sh
hop http 3000 --sub myapp
# https://myapp.hop.yourdomain.com  →  http://127.0.0.1:3000
```

### Working with Modern Dev Servers (Vite, Next.js, Rails)

Frameworks like Vite reject requests with unrecognized `Host` headers. Use `--host-header rewrite` to seamlessly pass your local address:

```sh
hop http 5173 --host-header rewrite
```

Or specify an explicit host header:
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
  https://myapp.hop.yourdomain.com  →  http://127.0.0.1:3000

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

- Logs for detached agents are saved to `~/.hop/log/<pid>.log` as plain text (no ANSI escape codes corrupting the file). Color is re-applied dynamically when reading via `hop log`.
- Inactive logs are automatically pruned after two weeks.

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
  blog        phone   4h12m    -        10.0.0.9:5173    https://blog.hop.yourdomain.com
  calm-raven  laptop  38s      55901    127.0.0.1:3000   https://calm-raven.hop.yourdomain.com
  myapp       laptop  2d3h     55899    127.0.0.1:3000   https://myapp.hop.yourdomain.com

  3 tunnel(s) up.
```

Output as JSON for shell scripting or automation:

```sh
hop ps --json | jq -r '.[] | select(.owner == "laptop") | .url'
```

---

## 🔍 Request Inspector

Every tunnel continuously records its last 50 HTTP exchanges in memory (capped at 64KB per request/response body). You can launch the web inspector interface anytime:

```sh
# Start tunnel with inspector opened from launch:
hop http 3000 --inspect

# Or attach inspector to an already-running tunnel:
hop inspect myapp
hop inspect 3000
```

The inspector runs on `http://127.0.0.1:4040` and provides:
- **Live Stream:** Real-time request and response feeds via Server-Sent Events (SSE).
- **Headers & Bodies:** Full request and response inspection.
- **🔁 Replay Request:** Send recorded requests directly to your local application with one click—perfect for debugging webhooks without asking the sender to re-fire.
- **📋 Copy as cURL:** Export public cURL commands for reproduction or sharing.
- **Security:** Binds strictly to `127.0.0.1` and blocks non-loopback `Host` headers to prevent DNS rebinding attacks.

---

## 🖥️ Self-Hosting Guide (`hopd`)

Setting up your own Hop server requires a Linux VPS with a public IP, a domain with DNS hosted on Cloudflare, and about 10 minutes.

### 1. DNS Configuration

Create two **A** records pointing to your VPS IP:

| Type | Name | Content | Proxy Status |
|------|------|---------|--------------|
| A | `hop` | `<VPS_IP>` | **DNS only (Grey Cloud)** |
| A | `*.hop` | `<VPS_IP>` | **DNS only (Grey Cloud)** |

> [!IMPORTANT]
> Cloudflare proxy (Orange Cloud) **must be OFF**. Cloudflare's proxy terminates TLS itself, which blocks hopd from solving the ACME challenge and prevents end-to-end TLS.

Verify DNS propagation before continuing:
```sh
dig +short hop.yourdomain.com
dig +short anything.hop.yourdomain.com
```

### 2. Create Scoped Cloudflare API Token

A wildcard certificate (`*.hop.yourdomain.com`) **cannot** be validated via HTTP-01; it requires the ACME **DNS-01** challenge. `hopd` needs permission to create temporary TXT records.

In Cloudflare Dashboard → **My Profile** → **API Tokens** → **Create Token** → **Custom Token**:
- **Permissions:**
  - `Zone` → `DNS` → `Edit`
  - `Zone` → `Zone` → `Read`
- **Zone Resources:**
  - `Include` → `Specific zone` → `<yourdomain.com>`

*(Both permissions are required: `Zone:Read` locates the zone ID, and `DNS:Edit` creates the verification record).*

### 3. Open Firewall Ports

`hopd` listens on three ports:
- `80/tcp`: HTTP to HTTPS redirect
- `443/tcp`: Public HTTPS ingress for tunnels
- `7443/tcp`: Agent control connection (TLS)

```sh
sudo ufw allow 22/tcp     # Don't lock yourself out!
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 7443/tcp
sudo ufw enable
```

*(Note: On Oracle Cloud or AWS, also ensure ingress rules for 80, 443, and 7443 are allowed in the Cloud Security List / Security Group).*

### 4. Build and Install `hopd`

Cross-compile locally and copy to your VPS:

```sh
# For Intel/AMD VPS (x86_64)
GOOS=linux GOARCH=amd64 go build -o hopd ./cmd/hopd

# For ARM VPS (Hetzner ARM, Oracle Ampere, AWS Graviton)
GOOS=linux GOARCH=arm64 go build -o hopd ./cmd/hopd

scp hopd root@<VPS_IP>:/usr/local/bin/hopd
```

On your VPS:
```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin hop
sudo chmod 755 /usr/local/bin/hopd

# Create directories and store Cloudflare secret
sudo mkdir -p /etc/hop /var/lib/hop/certs
sudo chown hop:hop /var/lib/hop/certs
sudo chmod 700 /var/lib/hop/certs

sudo bash -c 'cat > /etc/hop/hopd.env <<EOF
CLOUDFLARE_API_TOKEN=<your-scoped-cloudflare-token>
EOF'
sudo chmod 600 /etc/hop/hopd.env
```

### 5. Mint Device Tokens

`hopd` refuses to start without valid tokens configured. Generate tokens with `hopd mint <device-name>`:

```sh
# On the VPS:
hopd mint laptop
```

This outputs:
1. **The secret token:** Copy this to your laptop (used in `HOP_TOKEN`).
2. **The hashed config line:** Put this in `/etc/hop/tokens`.

```sh
sudo touch /etc/hop/tokens
sudo chown root:hop /etc/hop/tokens
sudo chmod 640 /etc/hop/tokens

# Add the minted token line to /etc/hop/tokens:
echo 'laptop  sha256:<hash>' | sudo tee -a /etc/hop/tokens
```

> [!TIP]
> **Zero-downtime token management:** `/etc/hop/tokens` is polled every 5 seconds. You can mint new device tokens or revoke compromised ones anytime without restarting `hopd`. Revoking a token terminates active tunnels immediately.

### 6. Install Systemd Service (Staging Test First!)

Let's Encrypt has a strict limit of 5 duplicate certificates per week. Always test against the Staging CA first!

Copy the systemd unit from [`deploy/hopd.service`](deploy/hopd.service):

```sh
sudo cp deploy/hopd.service /etc/systemd/system/hopd.service
```

Edit `/etc/systemd/system/hopd.service` with your domain and email:
```ini
[Unit]
Description=hop server
After=network.target

[Service]
Type=simple
User=hop
Group=hop
EnvironmentFile=/etc/hop/hopd.env
ExecStart=/usr/local/bin/hopd \
    -domain hop.yourdomain.com \
    -email you@example.com \
    -tokens-file /etc/hop/tokens \
    -cert-dir /var/lib/hop/certs \
    -staging=true

Restart=always
RestartSec=5s
LimitNOFILE=65535
AmbientCapabilities=CAP_NET_BIND_SERVICE
ProtectSystem=strict
ProtectHome=true
StateDirectory=hop/certs

[Install]
WantedBy=multi-user.target
```

Start the service and check the logs:
```sh
sudo systemctl daemon-reload
sudo systemctl start hopd
sudo journalctl -u hopd -f
```

Look for `obtaining certificate ...` followed by `certificate ready`. That confirms your DNS-01 challenge succeeded!

### 7. Switch to Production Certificates

Once the staging challenge succeeds:
1. Edit `/etc/systemd/system/hopd.service` to set `-staging=false`.
2. Clear the staging certificates and restart:

```sh
sudo rm -rf /var/lib/hop/certs/*
sudo systemctl daemon-reload
sudo systemctl restart hopd
sudo systemctl enable hopd
```

Verify your server from your laptop:
```sh
curl -I https://hop.yourdomain.com
```
*(An HTTP 404 response with valid TLS certificate confirms everything is running perfectly!)*

---

### Alternative Topology: Running Behind Existing Reverse Proxy (Caddy / Nginx)

If your VPS already runs websites on `:80` and `:443` (e.g. via Dockerized Caddy):
- The existing proxy terminates wildcard TLS on `:443` and reverse-proxies `*.hop.yourdomain.com` traffic to `hopd` on an internal address (e.g. `172.17.0.1:8080`).
- `hopd` terminates TLS on `:7443` directly (agent control protocol cannot be proxied as HTTP).
- Run `hopd` with:
  ```sh
  hopd -domain hop.yourdomain.com -ingress 172.17.0.1:8080 -ingress-tls=false -scheme https -control :7443
  ```
See **[`deploy/README.md`](deploy/README.md#4b-behind-an-existing-caddy)** for complete Caddyfile and Docker Compose configurations.

---

## 🧪 Local Offline Development

You can test Hop end-to-end on your local machine without a VPS, domain, or TLS certificates:

```sh
# Terminal 1: Run a mock web service
python3 -m http.server 3000

# Terminal 2: Run hopd locally (plaintext mode)
go run ./cmd/hopd -ingress-tls=false -control-tls=false \
    -domain localhost -ingress :8080 -public-port 8080 -tokens dev-token

# Terminal 3: Run the hop client
go run ./cmd/hop http 3000 --sub myapp --token dev-token --server localhost:7443 --no-tls
```

Test the connection:
```sh
curl -H "Host: myapp.localhost" http://127.0.0.1:8080/
```

---

## 🧠 Architecture & Behaviour Worth Knowing

- **Wildcard certificates require DNS-01:** There is no HTTP-01 path to issue wildcard certificates (`*.yourdomain.com`). `hopd` disables non-DNS challenge types so errors fail immediately and loudly.
- **Staging is the default:** `-staging` defaults to `true` to protect your domain from Let's Encrypt production rate limits while setting up DNS tokens.
- **One agent per subdomain:** Tunnels are scoped to token labels. If a connection drops, a new agent under the *same token label* reclaims the name immediately. An agent with a *different* token label is refused.
- **45-Second Grace Hold:** If an agent temporarily drops connection, the server preserves its subdomain for 45 seconds. Requests arriving in the interim receive **HTTP 503 `Retry-After: 5`** instead of 404, preventing webhook providers from dropping or deactivating endpoints.
- **Instant Intentional Teardown:** Explicit exits (`Ctrl-C` or `hop stop`) send a protocol `Bye` frame, releasing subdomains immediately so they can be reused without waiting out the grace window.
- **Full Jitter Exponential Backoff:** Reconnects draw random waits from an exponential window (1s to 30s) to prevent thundering herd storms when a server reboots.
- **Private Peer Plaintext Guard:** `--no-tls` strictly blocks connections unless the peer is a loopback or private network address (RFC 1918, CGNAT `100.64.0.0/10`, IPv6 ULA) to prevent transmitting tokens in plaintext over the internet.
- **Local Inspector Security:** The inspector binds exclusively to `127.0.0.1` and drops any request whose `Host` is not a loopback address, protecting against DNS rebinding attacks.

---

## 🛠️ CLI Reference

### `hop` (Agent) Flags

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

### `hopd` (Server) Flags

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

---

## 🧪 Tests

Run the complete test suite with race detection:

```sh
go test -race ./...
```
