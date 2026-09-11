# hop

A fast, lightweight, self-hosted **ngrok alternative**. Expose local ports to the internet with automated Let's Encrypt wildcard TLS, custom subdomains, live terminal request logs, and a built-in web request inspector.

```
https://myapp.hop.yourdomain.com  →  http://127.0.0.1:3000
```

---

## ⚠️ Important: How Hop Works & Requirements (Read First)

Hop is a **self-hosted client/server system**. Unlike commercial SaaS services (like ngrok.com), **there is no public shared cloud cluster**. You (or your organization) host your own server.

Hop consists of two parts:
1. **`hopd` (The Server Daemon):** Runs 24/7 on a Linux VPS with a public IP. It receives public HTTPS traffic on your domain and multiplexes it through encrypted tunnels to your devices.
2. **`hop` (The Client CLI):** Runs on your local laptop or workstation (macOS / Linux). It dials out to your `hopd` server and exposes your local dev servers (e.g. port 3000).

```
[ Internet ] ── HTTPS ──> [ hopd (Your VPS) ] ── Yamux/TLS (Port 7443) ──> [ hop (Your Laptop) ] ──> http://localhost:3000
```

### 📋 Prerequisites Checklist

Before you can open a tunnel, determine which setup applies to you:

#### Scenario A: Your team already runs a Hop server
If an administrator has already deployed `hopd`:
- ✅ You only need the **`hop` client CLI** on your laptop.
- 🔑 You need two values from your admin:
  1. `HOP_SERVER`: The server address (e.g. `hop.yourdomain.com:7443`).
  2. `HOP_TOKEN`: A private device token minted for you by the admin.
- ➡️ Jump directly to **[Part 2: Client Setup (`hop`)](#-part-2-client-setup--usage-hop)**.

#### Scenario B: You are self-hosting your own Hop server
If you are deploying Hop from scratch, **you must set up `hopd` first** before the client can connect. You will need:
- 🖥️ **A Linux VPS** with a public IPv4/IPv6 address (Hetzner, DigitalOcean, AWS, Oracle Cloud, etc.).
- 🌐 **A Domain Name** where you can add DNS records (e.g. `hop.yourdomain.com` and `*.hop.yourdomain.com`).
- 🔑 **TLS Certificates**:
  - *Default & automated:* A **Cloudflare API Token** (with `Zone:DNS:Edit` and `Zone:Zone:Read` permissions) to automatically issue Let's Encrypt wildcard certificates via DNS-01.
  - *Or custom:* Your own wildcard certificate files (`fullchain.pem` and `privkey.pem`) from Certbot or your DNS provider.
- 🚪 **Firewall Ports Open**: Ports `80` (HTTP redirect), `443` (HTTPS ingress), and `7443` (agent control connection).
- 🐳 **Docker & Docker Compose** on your VPS (recommended, or Go 1.22+ for bare metal).
- ➡️ Start with **[Part 1: Server Setup (`hopd`)](#-part-1-server-setup-hopd)**.

#### Scenario C: Local offline testing
If you just want to test Hop on your laptop without a VPS, domain, or TLS certificates:
- ➡️ Jump to **[Local Offline Development](#-local-offline-development)**.

---

## ✨ Features

- **🚀 Instant Public HTTPS:** Automatic Let's Encrypt wildcard TLS certificate over DNS-01 (or bring your own certificates).
- **🔍 Built-in Request Inspector & Replay:** Watch requests live in your browser (`http://127.0.0.1:4040`), inspect headers and bodies, copy as cURL, or replay failed webhooks locally with one click.
- **🪵 Live Terminal Logs:** Color-coded request logs with HTTP method, status, client device detection, and time-to-first-byte measurements.
- **⚡ Background Daemon Mode:** Detach tunnels into the background with `-d`, inspect or tail their logs with `hop log`, and stop them cleanly with `hop stop`.
- **📱 Multi-Device Awareness:** `hop ps` lists active tunnels across all your registered devices (laptop, desktop, staging servers).
- **🛡️ Resilient Reconnects:** Temporary network drops keep your subdomain reserved for 45 seconds (returning HTTP 503 `Retry-After` to webhook callers rather than 404), with full-jitter exponential backoff.
- **🔒 Secure by Design:** TLS required on public networks, unguessable 32-byte auth tokens stored only as SHA-256 hashes, and local inspector protected by loopback Host validation.

---

## 🖥️ Part 1: Server Setup (`hopd`)

Follow this section to deploy the `hopd` daemon on your VPS. Once deployed, you will generate an agent token to use on your laptop.

### 1. DNS Configuration

In your DNS provider, create two **A** records pointing to your VPS IP address:

| Type | Name | Content | Proxy Status |
|------|------|---------|--------------|
| A | `hop` | `<VPS_IP>` | **DNS only (Grey Cloud)** |
| A | `*.hop` | `<VPS_IP>` | **DNS only (Grey Cloud)** |

> [!IMPORTANT]
> If using Cloudflare DNS, the orange proxy cloud **must be OFF (Grey Cloud / DNS-only)**. Cloudflare's proxy terminates TLS, which prevents hopd from solving the ACME challenge and breaks the raw TCP control connection on port 7443.

Verify DNS propagation before continuing:
```sh
dig +short hop.yourdomain.com
dig +short anything.hop.yourdomain.com
```

### 2. DNS-01 Credentials or Custom Certificates

Wildcard certificates (`*.hop.yourdomain.com`) **cannot** be issued via standard HTTP-01 challenges; they require ACME **DNS-01**.

- **Option 1 (Cloudflare DNS - Automated):** In Cloudflare Dashboard → **My Profile** → **API Tokens** → **Create Token** → **Custom Token**:
  - Permissions: `Zone` → `DNS` → `Edit` **and** `Zone` → `Zone` → `Read`.
  - Zone Resources: `Include` → `Specific zone` → `<yourdomain.com>`.
- **Option 2 (Non-Cloudflare / Custom Certs):** If your DNS is on Route53, Porkbun, DuckDNS, etc., generate a wildcard cert with Certbot/acme.sh and pass `-tls-cert` and `-tls-key`. See [Non-Cloudflare DNS Guide](#-what-if-your-dns-is-not-on-cloudflare).

### 3. Open Firewall Ports

Ensure your VPS firewall allows traffic on ports `80`, `443`, and `7443`:

```sh
sudo ufw allow 22/tcp     # Don't lock yourself out!
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw allow 7443/tcp
sudo ufw enable
```
*(On AWS, Oracle Cloud, or Hetzner, also ensure ingress rules for 80, 443, and 7443 are allowed in your cloud provider's Security List / Security Group).*

---

### Option A: Deploy with Docker Compose (Recommended)

The fastest and cleanest way to run `hopd` in production.

#### 1. Set up the project on your VPS
```sh
# Clone the repository:
git clone https://github.com/tabaak/hop.git
cd hop
cp .env.example .env

# Or without git (download files directly):
mkdir -p hop && cd hop
curl -sO https://raw.githubusercontent.com/tabaak/hop/main/docker-compose.yml
curl -sO https://raw.githubusercontent.com/tabaak/hop/main/.env.example
cp .env.example .env
```

#### 2. Configure `.env`
Edit `.env` with your domain and Cloudflare token:
```env
DOMAIN=hop.yourdomain.com
CLOUDFLARE_API_TOKEN=your-cloudflare-api-token
EMAIL=you@example.com
STAGING=true
```

#### 3. Start the Container
```sh
docker compose up -d
docker compose logs -f
```
Look for `obtaining certificate ...` and `certificate ready` in the logs.

#### 4. Mint Your First Agent Token
Mint an agent token inside the container and append its hash to the tokens volume:
```sh
docker compose exec hopd hopd mint laptop -a /etc/hop/tokens
```
This prints the secret token for your laptop:
```
Token for "laptop". Copy it now — it is not stored anywhere and cannot be shown again:

  399c2d76589419d2...

Appended to /etc/hop/tokens. hopd picks it up within seconds, no restart!
```
Save this token! You will use it on your laptop in Part 2.

#### 5. Switch to Production Certificates
Once the staging test succeeds, edit `.env` to set `STAGING=false`, clear the staging certs, and recreate the container:
```sh
docker compose down
docker compose run --rm --entrypoint rm hopd -rf /var/lib/hop/certs/*
docker compose up -d
```

Verify reachability from your laptop:
```sh
curl -I https://hop.yourdomain.com
# HTTP/2 404 (Healthy response — TLS terminated correctly!)
```

---

### Option B: Bare Metal Systemd Setup

If you prefer running `hopd` natively as a Linux systemd service:

#### 1. Build and Install Binary
Cross-compile locally and copy to your VPS:
```sh
# Intel/AMD VPS (x86_64)
GOOS=linux GOARCH=amd64 go build -o hopd ./cmd/hopd

# ARM VPS (Hetzner ARM, Oracle Ampere, AWS Graviton)
GOOS=linux GOARCH=arm64 go build -o hopd ./cmd/hopd

scp hopd root@<VPS_IP>:/usr/local/bin/hopd
```

#### 2. System User & Storage Directories
On your VPS:
```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin hop
sudo chmod 755 /usr/local/bin/hopd

sudo mkdir -p /etc/hop /var/lib/hop/certs
sudo chown hop:hop /var/lib/hop/certs
sudo chmod 700 /var/lib/hop/certs

sudo bash -c 'cat > /etc/hop/hopd.env <<EOF
CLOUDFLARE_API_TOKEN=<your-scoped-cloudflare-token>
EOF'
sudo chmod 600 /etc/hop/hopd.env
```

#### 3. Mint Device Tokens
```sh
# On the VPS:
hopd mint laptop -a /etc/hop/tokens
```
Save the secret token output for your laptop.

#### 4. Install Systemd Service (Staging Test First!)
Copy the systemd unit from [`deploy/hopd.service`](deploy/hopd.service):
```sh
sudo cp deploy/hopd.service /etc/systemd/system/hopd.service
```
Edit `/etc/systemd/system/hopd.service` with your domain and email, then:
```sh
sudo systemctl daemon-reload
sudo systemctl start hopd
sudo journalctl -u hopd -f
```
Confirm `certificate ready` appears in the logs.

#### 5. Switch to Production Certificates
Edit `/etc/systemd/system/hopd.service` to set `-staging=false`, then restart:
```sh
sudo rm -rf /var/lib/hop/certs/*
sudo systemctl daemon-reload
sudo systemctl restart hopd
sudo systemctl enable hopd
```

---

### 🌐 What If Your DNS Is Not on Cloudflare?

You have three straightforward alternatives:

1. **Point Your Nameservers to Cloudflare (Free):** You do not need to move your domain registration or pay anything. In your domain registrar (Namecheap, GoDaddy, Porkbun, etc.), keep your registrar and simply set the domain's NS (nameserver) records to Cloudflare's free DNS.
2. **Bring Your Own Wildcard Certificate (`-tls-cert` & `-tls-key`):** If your DNS is on AWS Route 53, DigitalOcean, Porkbun, DuckDNS, etc., generate a wildcard cert with Certbot or acme.sh:
   ```sh
   certbot certonly --dns-route53 -d "hop.yourdomain.com" -d "*.hop.yourdomain.com"
   ```
   Supply the cert files directly to `hopd`:
   ```sh
   hopd -domain hop.yourdomain.com \
        -tls-cert /etc/letsencrypt/live/hop.yourdomain.com/fullchain.pem \
        -tls-key /etc/letsencrypt/live/hop.yourdomain.com/privkey.pem
   ```
   *(Or in Docker via `HOP_TLS_CERT` and `HOP_TLS_KEY`).* `hopd` will load your cert directly and skip ACME.
3. **Run Behind an Existing Reverse Proxy (Caddy / Nginx / Traefik):** If your server already runs a reverse proxy that owns `:80` and `:443`, have it terminate wildcard TLS and proxy HTTP ingress to `hopd` on an internal port (`127.0.0.1:8080`), while `hopd` terminates TLS on `:7443` directly. See [`deploy/README.md`](deploy/README.md#4b-behind-an-existing-caddy).

---

## 💻 Part 2: Client Setup & Usage (`hop`)

Once your `hopd` server is running (or your team administrator has given you your credentials), install and use the `hop` client on your laptop.

### 📦 Installation

#### Homebrew (macOS / Linux)
```sh
brew install tabaak/tap/hop
```

*(Or tap the repository manually):*
```sh
brew tap tabaak/hop
brew install hop
```

#### Go Install
```sh
go install hop.vokh.dev/cmd/hop@latest
```

#### From Source
```sh
git clone https://github.com/tabaak/hop.git
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

### 🚀 Quickstart: Open Your First Tunnel

#### 1. Set your credentials
Add your server address and secret agent token (minted in Part 1) to your shell profile (`~/.zshrc` or `~/.bashrc`):

```sh
export HOP_SERVER="hop.yourdomain.com:7443"
export HOP_TOKEN="<your-device-token>"
```

#### 2. Open a tunnel
Expose any local port:

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

### 🛠️ Everyday Client Workflows

#### Claim a Custom Subdomain
```sh
hop http 3000 --sub myapp
# https://myapp.hop.yourdomain.com  →  http://127.0.0.1:3000
```

#### Working with Modern Dev Servers (Vite, Next.js, Rails)
Dev servers like Vite reject requests with unrecognized `Host` headers. Use `--host-header rewrite` to transparently rewrite the Host header to your local address:

```sh
hop http 5173 --host-header rewrite
```

Or specify an explicit host header:
```sh
hop http 3000 --host-header app.internal
```

#### Forward to LAN IPs, VMs, or Docker Containers
Forward traffic to another device or container on your local network:
```sh
hop http 8080 --local-host 192.168.1.42
```

#### Running Tunnels in the Background
Run with `-d` (or `--detach`) to immediately return terminal control while keeping the tunnel alive:

```sh
hop http 3000 --sub myapp -d
```

```
  https://myapp.hop.yourdomain.com  →  http://127.0.0.1:3000

  detached, pid 55899. hop log myapp for the request log,
  hop inspect myapp to watch requests in a browser, hop stop myapp to end it.
```

- The command returns only once the tunnel is confirmed **up** on the server.
- Closing your terminal window or laptop lid will not kill detached tunnels.

#### Inspecting Background Logs
```sh
hop log myapp          # View last 50 log lines
hop log myapp -n 200   # View last 200 lines
hop log myapp -f       # Stream logs in real-time (follow)
```

#### Managing & Stopping Tunnels
Stop running tunnels by name, local port, or PID:

```sh
hop stop myapp        # By subdomain name
hop stop 3000         # "Stop whatever is forwarding port 3000"
hop stop 55899        # By process PID
hop stop web docs     # Multiple tunnels at once
hop stop --all        # Stop every tunnel running on this machine
```

#### Checking Active Tunnels Across Devices (`hop ps`)
See active tunnels across all your registered devices (`hop ls` and `hop status` are aliases):

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
- **Headers & Bodies:** Full inspection of request and response payloads.
- **🔁 Replay Request:** Send recorded requests directly to your local application with one click—ideal for debugging webhooks without asking the provider to re-trigger.
- **📋 Copy as cURL:** Export public cURL commands for sharing or reproduction.
- **Security:** Binds strictly to `127.0.0.1` and blocks non-loopback `Host` headers to protect against DNS rebinding attacks.

---

## 🧪 Local Offline Development

You can test Hop end-to-end on your local machine without a VPS, domain, or TLS certificates:

```sh
# Terminal 1: Run a mock web service
python3 -m http.server 3000

# Terminal 2: Run hopd locally (plaintext mode)
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

## 🧠 Architecture & Behaviour Worth Knowing

- **Wildcard certificates require DNS-01:** There is no HTTP-01 path to issue wildcard certificates (`*.yourdomain.com`). `hopd` disables non-DNS challenge types so configuration errors fail immediately and loudly.
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
| `-tls-cert` | *(empty)* | Path to custom TLS certificate fullchain.pem (skips ACME/Cloudflare) |
| `-tls-key` | *(empty)* | Path to custom TLS private key.pem |

---

## 🧪 Tests

Run the complete test suite with race detection:

```sh
go test -race ./...
```
