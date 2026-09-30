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
- 🌐 **A Domain Name** (hop will live under a subdomain such as `hop.yourdomain.com`).
- 🔑 **DNS provider API credentials** so hopd can issue its Let's Encrypt wildcard certificate and create its DNS records itself. Supported: Cloudflare, Route 53, DigitalOcean, Hetzner, Porkbun, Namecheap, GoDaddy, Gandi, OVH, Linode, Vultr, DuckDNS, deSEC, Bunny.
  - *Provider not listed?* Bring your own wildcard certificate (`fullchain.pem` + `privkey.pem`) and create two DNS records by hand — hopd tells you exactly which.
- 🚪 **Firewall Ports Open**: Ports `80` (HTTP redirect), `443` (HTTPS ingress), and `7443` (agent control connection) — *(or just `7443` if running behind an existing reverse proxy)*.
- 🐳 **Docker & Docker Compose** on your VPS (recommended, or Go 1.26+ for bare metal).
- 🧰 **Go 1.26+** on your laptop to build the `hop` client (see [Installation](#-installation)).
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

### 1. DNS Provider Credentials

hopd uses your DNS provider's API for two things: issuing the wildcard certificate (`*.hop.yourdomain.com` can only be validated via ACME **DNS-01**), and creating the `hop` and `*.hop` A records on first start. **You do not need to create any DNS records yourself.**

Create an API credential for your provider:

| Provider | `DNS_PROVIDER` | Variables | Where to get it |
|---|---|---|---|
| Cloudflare | `cloudflare` | `CLOUDFLARE_API_TOKEN` | My Profile → API Tokens → Create Token → Custom: `Zone`→`DNS`→`Edit` **and** `Zone`→`Zone`→`Read`, scoped to your zone |
| AWS Route 53 | `route53` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` (or an instance role) | IAM user with `route53:ChangeResourceRecordSets`, `ListHostedZonesByName`, `ListResourceRecordSets`, `GetChange` |
| DigitalOcean | `digitalocean` | `DIGITALOCEAN_TOKEN` | API → Tokens, with write scope |
| Hetzner | `hetzner` | `HETZNER_API_TOKEN` | dns.hetzner.com → API Tokens |
| Porkbun | `porkbun` | `PORKBUN_API_KEY`, `PORKBUN_SECRET_API_KEY` | Account → API Access, then enable API access on the domain |
| Namecheap | `namecheap` | `NAMECHEAP_API_KEY`, `NAMECHEAP_API_USER` | Profile → Tools → API Access; allowlist your VPS IP |
| GoDaddy | `godaddy` | `GODADDY_TOKEN` (`key:secret`) | developer.godaddy.com → API Keys (Production) |
| Gandi | `gandi` | `GANDI_BEARER_TOKEN` | Account → Authentication → Personal Access Token |
| OVH | `ovh` | `OVH_ENDPOINT`, `OVH_APPLICATION_KEY`, `OVH_APPLICATION_SECRET`, `OVH_CONSUMER_KEY` | api.ovh.com/createToken |
| Linode · Vultr · DuckDNS · deSEC · Bunny | `linode` · `vultr` · `duckdns` · `desec` · `bunny` | `LINODE_TOKEN` · `VULTR_API_KEY` · `DUCKDNS_TOKEN` · `DESEC_TOKEN` · `BUNNY_API_KEY` | Provider's API settings |

Provider not listed? See [Unsupported DNS Providers](#-unsupported-dns-providers).

> [!NOTE]
> On every start, hopd checks that `hop.yourdomain.com` and `*.hop.yourdomain.com` resolve. Missing records are created through the provider (never overwritten if they already exist); without provider access, hopd prints the exact records to add and **waits until they resolve** — no `dig` needed. It also warns if a record points somewhere else, or at Cloudflare's proxy (the orange cloud must be **off** — agents cannot connect through it).

### 2. Pick Your Subdomain

Everything lives under one subdomain, e.g. `hop.yourdomain.com`: tunnels become `<name>.hop.yourdomain.com`, and agents connect to `hop.yourdomain.com:7443`. It must be in a zone your credential can edit.

### 3. Open Firewall Ports

Traffic on ports `80` (HTTP redirect), `443` (HTTPS ingress), and `7443` (tunnel control) must reach your VPS:

- **Cloud Provider Firewalls (AWS, Hetzner, DigitalOcean, Oracle Cloud, etc.):**
  Make sure your cloud provider's **Security Group / Firewall** allows inbound TCP on ports `80`, `443`, and `7443`.
- **Host Firewall (UFW):**
  - If you deploy via **Docker**, Docker manages its own iptables rules and forwards these ports automatically.
  - If you deploy via **Bare Metal (Systemd)** or already have UFW active on the host, allow the ports:
    ```sh
    sudo ufw allow 80/tcp
    sudo ufw allow 443/tcp
    sudo ufw allow 7443/tcp
    ```
    *(If UFW is inactive, you don't need to enable it unless you want a host-level firewall. If enabling it, always ensure your SSH port—default `22/tcp`—is allowed first!)*

---

### Option A: Deploy with Docker Compose (Recommended)

The fastest and cleanest way to run `hopd` in production.

#### 1. Set up the project on your VPS
```sh
# Option 1: Download Compose files directly (no git or source code needed):
mkdir -p hop && cd hop
curl -sO https://raw.githubusercontent.com/tabaak/hop/main/docker-compose.yml
curl -sO https://raw.githubusercontent.com/tabaak/hop/main/.env.example
cp .env.example .env

# Option 2: Or clone the repository:
git clone --depth 1 https://github.com/tabaak/hop.git
cd hop
cp .env.example .env
```

#### 2. Configure `.env`
Edit `.env` with your domain and DNS provider credentials:
```env
DOMAIN=hop.yourdomain.com
DNS_PROVIDER=cloudflare
CLOUDFLARE_API_TOKEN=your-cloudflare-api-token
EMAIL=you@example.com
STAGING=true
```
For another provider, set `DNS_PROVIDER` and its variables from the [table above](#1-dns-provider-credentials) instead — `.env.example` lists them all.

> [!IMPORTANT]
> **Already hosting a website on this VPS (ports 80 or 443 in use)?**
> If you already have Nginx, Caddy, Apache, or another container running on ports 80 or 443, **you must add this line to `.env`**:
> ```env
> COMPOSE_PROFILES=proxy
> ```
> This tells Docker Compose to run in **Proxy mode** on plain-HTTP port `8080` without touching ports 80 or 443, avoiding port collision errors. Then, forward `*.hop.yourdomain.com` from your existing web server to port `8080` (see **[Running Alongside Existing Websites](#-running-alongside-existing-websites-nginx--caddy--reverse-proxy)** for ready-to-copy Nginx and Caddy blocks).

#### 3. Mint Your First Agent Token
Mint an initial device token into the tokens volume before starting the daemon:
```sh
docker compose run --rm hopd mint laptop -a /etc/hop/tokens
```
This prints the secret token for your laptop:
```
Token for "laptop". Copy it now — it is not stored anywhere and cannot be shown again:

  399c2d76589419d2...
```
Save this token! You will use it on your laptop in Part 2.
*(To mint more tokens later while hopd is running, use `docker exec hopd hopd mint <name> -a /etc/hop/tokens` — this works in both standalone and proxy mode. The running daemon picks up the new token within seconds, no restart needed).*

#### 4. Start the Container
```sh
docker compose up -d
docker compose logs -f
```
You will see `1 token(s) accepted`, then `dns: creating hop.yourdomain.com, *.hop.yourdomain.com → <VPS_IP>` on the first start, `dns: ... resolve`, and finally `certificate ready` (in standalone mode) or `ingress listening on :8080` (in proxy mode).

#### 5. Switch to Production Certificates
Once the staging test succeeds, edit `.env` to set `STAGING=false`, clear the staging certs, and recreate the container:
```sh
docker compose down
docker compose run --rm --entrypoint sh hopd -c 'rm -rf /var/lib/hop/certs/*'
docker compose up -d
```
*(The single quotes matter: the `*` must be expanded by the shell inside the container, not by your VPS shell.)*

Verify reachability from your laptop:
```sh
# Standalone mode:
curl -I https://hop.yourdomain.com
# HTTP/2 404 (Healthy response — TLS terminated correctly!)

# Proxy mode (your web server only routes the wildcard, so test a subdomain):
curl https://test.hop.yourdomain.com
# hop: No agent is serving "test" right now.

# Both modes — the agent control port must be reachable:
nc -zv hop.yourdomain.com 7443
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
DNS_PROVIDER=cloudflare
CLOUDFLARE_API_TOKEN=<your-scoped-cloudflare-token>
EOF'
sudo chmod 600 /etc/hop/hopd.env
```

#### 3. Mint Device Tokens
```sh
# On the VPS:
sudo hopd mint laptop -a /etc/hop/tokens
sudo chown root:hop /etc/hop/tokens
sudo chmod 640 /etc/hop/tokens
```
Save the secret token output for your laptop. The service runs as the `hop` user, so without the `chown`/`chmod` it cannot read the tokens file and exits with `no tokens configured`.

#### 4. Install Systemd Service (Staging Test First!)
Copy the systemd unit from [`deploy/hopd.service`](deploy/hopd.service):
```sh
sudo cp deploy/hopd.service /etc/systemd/system/hopd.service
```
Edit the `ExecStart` line in `/etc/systemd/system/hopd.service`:
- Replace `-domain hop.vokh.dev` and `-email ...` with your own domain and email.
- The unit ships configured for running **behind a reverse proxy** (Caddy in Docker): `-ingress 172.17.0.1:8080 -ingress-tls=false -scheme https`. `172.17.0.1` is the Docker bridge address (check with `ip -4 addr show docker0`); use `127.0.0.1:8080` if your proxy runs directly on the host. Then configure your proxy as in [Running Alongside Existing Websites](#-running-alongside-existing-websites-nginx--caddy--reverse-proxy).
- **Standalone instead** (hopd owns `:80`/`:443`): replace those three flags with `-ingress :443`, and replace `CapabilityBoundingSet=` with the two `CAP_NET_BIND_SERVICE` lines described in the unit's comments — the `hop` user cannot bind ports below 1024 otherwise.

Then:
```sh
sudo systemctl daemon-reload
sudo systemctl start hopd
sudo journalctl -u hopd -f
```
Confirm `certificate ready` appears in the logs (followed by `ingress listening on ...`).

#### 5. Switch to Production Certificates
Edit `/etc/systemd/system/hopd.service` to set `-staging=false`, then restart:
```sh
sudo rm -rf /var/lib/hop/certs/*
sudo systemctl daemon-reload
sudo systemctl restart hopd
sudo systemctl enable hopd
```

---

### 🌐 Unsupported DNS Providers

If your DNS host has no API or isn't in the [supported list](#1-dns-provider-credentials), you have two options:

1. **Move just the DNS to a supported provider (free):** Keep your registrar and point the domain's nameservers at Cloudflare, deSEC or Hetzner DNS, all of which are free.
2. **Bring your own wildcard certificate and create the records by hand:** Generate a wildcard cert with Certbot or acme.sh:
   ```sh
   certbot certonly --manual --preferred-challenges dns -d "hop.yourdomain.com" -d "*.hop.yourdomain.com"
   ```
   Supply the cert files directly to `hopd`:
   ```sh
   hopd -domain hop.yourdomain.com \
        -tls-cert /etc/letsencrypt/live/hop.yourdomain.com/fullchain.pem \
        -tls-key /etc/letsencrypt/live/hop.yourdomain.com/privkey.pem
   ```
   *(Or in Docker via `HOP_TLS_CERT` and `HOP_TLS_KEY`.)* On start, hopd prints the two A records to create and waits until they resolve:
   ```
   DNS is not set up yet. Create these records at your DNS provider:

     ✗  A    hop.yourdomain.com    203.0.113.7
     ✗  A    *.hop.yourdomain.com  203.0.113.7

   Waiting for them to resolve — hopd continues on its own once they do.
   ```
   Manual certificates don't renew themselves: renew before expiry and restart hopd.

---

### 🔀 Running Alongside Existing Websites (Nginx / Caddy / Reverse Proxy)

If your VPS already runs other websites and owns ports `:80` and `:443`, **do not let `hopd` bind them**. Instead, let your existing web server terminate wildcard TLS and reverse-proxy tunnel traffic to `hopd` on an internal port:

```
[ Internet ] ── HTTPS (:443) ──> [ Your Nginx / Caddy ] ──> :8080 (HTTP) ──> [ hopd ]
[ Internet ] ── TLS (:7443)   ───────────────────────────────────────────> [ hopd ]
```

#### 1. Enable Proxy Mode in `.env`

In `.env`, simply set:
```env
COMPOSE_PROFILES=proxy
```
That's it! Docker Compose automatically frees ports `:80` and `:443`, publishes plain-HTTP port `:8080`, and configures `hopd` for reverse proxy operation. You never need to touch `docker-compose.yml`.

> [!WARNING]
> Port `8080` is published on all interfaces so that a reverse proxy running in its own container can reach it. **Keep `8080` closed in your cloud firewall / security list** — only `7443` (and your proxy's `80`/`443`) should be reachable from the internet.

> [!TIP]
> **Symptom of a port clash:** if you forget `COMPOSE_PROFILES=proxy` while another container owns `:80`/`:443`, `docker compose up -d` may still report `Started`, but `docker compose ps` shows no published ports and the logs show DNS errors like `lookup acme-v02.api.letsencrypt.org on 127.0.0.53:53: ... connection refused` — the container came up without a network. Set the profile, then `docker compose down && docker compose up -d`.

*(If running bare-metal Systemd without Docker, pass `-reverse-proxy` — it listens on `:8080` on all interfaces, so either firewall it or set `-ingress 127.0.0.1:8080` explicitly.)*

#### 2. Find the Address Your Web Server Should Forward To

This depends on **where your web server runs**, not on hop:

| Your Nginx / Caddy runs… | Forward to | Notes |
|---|---|---|
| Directly on the host (apt, systemd) | `127.0.0.1:8080` | Simplest case. |
| In Docker — **config file only** | `<gateway-ip>:8080` | No change to the proxy's `docker-compose.yml`. See below to find the IP. |
| In Docker — with `host.docker.internal` | `host.docker.internal:8080` | Same on every machine, but on Linux you must add one line to the proxy's compose service (see below). |

> [!IMPORTANT]
> If your web server runs in Docker, **`127.0.0.1` is the web server's own container, not the host** — forwarding there gives `502 Bad Gateway`.

**Finding `<gateway-ip>`** (web server in Docker, config-file-only): the host is reachable at the gateway of the Docker network your web server container is on. Print it with (replace `caddy-container` with your container's name from `docker ps`):
```sh
docker inspect caddy-container \
  --format '{{range $net, $cfg := .NetworkSettings.Networks}}{{$net}} {{$cfg.Gateway}}{{"\n"}}{{end}}'
# myproject_default 172.18.0.1   ← use 172.18.0.1:8080
```
This IP stays the same while that Docker network exists. If you ever `docker compose down` the web server's project and tunnels start returning `502`, re-run the command and update the address.

**Using `host.docker.internal` instead:** add this to your web server's service in *its* `docker-compose.yml`, then recreate it with `docker compose up -d <service>` (a restart or reload does not apply compose changes):
```yaml
    extra_hosts:
      - "host.docker.internal:host-gateway"
```

#### 3. Configure Your Web Server

In the examples below, replace:
- `hop.yourdomain.com` → your `DOMAIN` from `.env` (e.g. `hop.example.com`, so the site name becomes `*.hop.example.com`)
- `<address>` → the address from step 2 (e.g. `127.0.0.1:8080` or `172.18.0.1:8080`)

##### Caddy

**a) Check that your Caddy can issue a wildcard certificate.** A wildcard certificate can only be issued via the **DNS-01** challenge, and the stock `caddy` binary/image ships **without** DNS provider plugins. Check:
```sh
caddy list-modules | grep dns.providers
# Caddy in Docker:
docker compose exec caddy caddy list-modules | grep dns.providers
```
If `dns.providers.cloudflare` is listed, skip to b). Otherwise build Caddy with the plugin, e.g. with this `Dockerfile` next to your Caddy compose file:
```dockerfile
FROM caddy:2-builder-alpine AS builder
RUN xcaddy build --with github.com/caddy-dns/cloudflare

FROM caddy:2-alpine
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
```
Point your Caddy service at it (`build: .` instead of `image: caddy:2-alpine`), pass the Cloudflare token as an environment variable (`CF_API_TOKEN: ${CF_API_TOKEN}` under `environment:`), and run `docker compose up -d --build caddy`. For a non-Docker Caddy, download a build that includes `caddy-dns/cloudflare` from [caddyserver.com/download](https://caddyserver.com/download). Other DNS providers have their own `caddy-dns/<provider>` plugin.

**b) Add the site block** to your `Caddyfile` (Caddyfiles use tabs for indentation):
```caddyfile
*.hop.yourdomain.com {
	tls {
		dns cloudflare {env.CF_API_TOKEN}
	}
	reverse_proxy <address>
}
```
Deliberately no `encode` directive: compressing tunnel traffic would break streaming responses (SSE, chunked) passing through.

**c) Reload Caddy** — applies the Caddyfile with no downtime; if the file has an error, Caddy keeps the old config and prints why:
```sh
caddy reload --config /etc/caddy/Caddyfile
# Caddy in Docker:
docker compose exec caddy caddy reload --config /etc/caddy/Caddyfile
```
Watch the logs for `certificate obtained successfully` for `*.hop.yourdomain.com` (10–30 seconds on first run).

##### Nginx

**a) Get a wildcard certificate.** Nginx does not issue certificates itself; use Certbot with the DNS plugin for your provider, e.g. Cloudflare:
```sh
sudo apt install certbot python3-certbot-dns-cloudflare
sudo install -m 600 /dev/null /etc/letsencrypt/cloudflare.ini
echo "dns_cloudflare_api_token = <your-cloudflare-api-token>" | sudo tee /etc/letsencrypt/cloudflare.ini >/dev/null
sudo certbot certonly --dns-cloudflare \
  --dns-cloudflare-credentials /etc/letsencrypt/cloudflare.ini \
  -d "*.hop.yourdomain.com" \
  --deploy-hook "systemctl reload nginx"
```
Certbot renews automatically; the deploy hook reloads Nginx after each renewal so it picks up the new certificate. (Nginx in Docker: mount `/etc/letsencrypt` into the container and use `docker exec <nginx-container> nginx -s reload` as the hook.)

**b) Add a server block** (e.g. `/etc/nginx/sites-available/hop`, then `sudo ln -s /etc/nginx/sites-available/hop /etc/nginx/sites-enabled/`):
```nginx
server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;   # Nginx older than 1.25.1: remove this line and use "listen 443 ssl http2;"
    server_name *.hop.yourdomain.com;

    ssl_certificate     /etc/letsencrypt/live/hop.yourdomain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/hop.yourdomain.com/privkey.pem;

    # Tunnels carry arbitrary apps and webhooks: no upload limit, and pass
    # streaming responses (SSE, chunked) through instead of buffering them.
    client_max_body_size 0;
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;

    location / {
        proxy_pass http://<address>;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
    }
}
```

**c) Test and reload:**
```sh
sudo nginx -t && sudo systemctl reload nginx
# Nginx in Docker:
docker exec <nginx-container> nginx -t && docker exec <nginx-container> nginx -s reload
```

#### 4. Verify

From your laptop, with no agent connected yet:
```sh
curl https://test.hop.yourdomain.com
# hop: No agent is serving "test" right now.   ← web server → hopd path works
```

| Symptom | Cause | Fix |
|---|---|---|
| `curl: (35) ... tlsv1 alert internal error` / `SSL_ERROR_INTERNAL_ERROR_ALERT` | Web server has no certificate for this name | Site name still `yourdomain.com`, config not reloaded, or the certificate request failed — check the web server's logs |
| `502 Bad Gateway` | Web server can't reach hopd | Wrong address in step 2 (usually `127.0.0.1` from inside Docker), or hopd isn't running (`docker compose ps` in the hop directory) |
| Caddy log: `module not registered: dns.providers.cloudflare` | Stock Caddy without the DNS plugin | Step 3 Caddy a) |
| Caddy/Certbot: `API token ... invalid` or `403` | Token wrong or lacking `Zone:DNS:Edit` for your zone | Recreate the token as in [DNS Provider Credentials](#1-dns-provider-credentials) |
| Caddy: `Caddyfile input is not formatted` warning | Spaces instead of tabs | Harmless; re-indent with tabs to silence it |
| Response comes from your main website, not hop | DNS for `*.hop` missing, or another site block matches first | Check `dig +short test.hop.yourdomain.com` and your other server blocks |

> [!NOTE]
> Port `:7443` connects directly to `hopd` from your laptop and bypasses Nginx/Caddy because it is a raw TCP/yamux TLS control connection, not HTTP. `hopd` still obtains its own certificate for `hop.yourdomain.com` to secure this connection, so remember to set `STAGING=false` in proxy mode too — agents will not trust a staging certificate.

---

## 💻 Part 2: Client Setup & Usage (`hop`)

Once your `hopd` server is running (or your team administrator has given you your credentials), install and use the `hop` client on your laptop.

### 📦 Installation

There are no prebuilt packages yet (no Homebrew tap, and `go install` by module path does not resolve), so build the client from source. You need **Go 1.26+** (`go version`; any Go ≥ 1.21 will download the required toolchain automatically).

```sh
git clone https://github.com/tabaak/hop.git
cd hop
go build -o hop ./cmd/hop
sudo install -m 755 hop /usr/local/bin/hop
```

Verify your installation:
```sh
hop version
# hop v1.1.0 (protocol 1)
# hopd v1.1.0 (protocol 1) at hop.yourdomain.com:7443   ← only when HOP_TOKEN is set
```

#### Client and server versions

`hop` and `hopd` are released together under one version number, but you don't have to upgrade them together:

- **Any `hop` works with any `hopd` that speaks the same protocol.** The protocol number (shown by `hop version` and `hopd version`) changes only when a change could no longer work with older binaries, which is rare. New features are added so that older binaries simply ignore them.
- **A newer `hop` against an older `hopd` still connects.** Features that need server support won't work until the server is upgraded, and `hop` prints a one-line note saying so.
- **If the protocols don't match**, the connection is refused with a message saying which side to upgrade.

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
- **DNS is checked before anything else:** `hopd` asks your zone's authoritative nameservers (not a cache that may still remember "no such name") whether `<domain>` and `*.<domain>` resolve, creates missing records through the DNS provider, and otherwise waits for them. Existing records are never modified. Disable with `-dns-check=false`; skip only the record creation with `-manage-dns=false`.
- **Staging is the default:** `-staging` defaults to `true` to protect your domain from Let's Encrypt production rate limits while setting up DNS tokens.
- **One agent per subdomain:** Tunnels are scoped to token labels. If a connection drops, a new agent under the *same token label* reclaims the name immediately. An agent with a *different* token label is refused.
- **45-Second Grace Hold:** If an agent temporarily drops connection, the server preserves its subdomain for 45 seconds. Requests arriving in the interim receive **HTTP 503 `Retry-After: 5`** instead of 404, preventing webhook providers from dropping or deactivating endpoints.
- **Instant Intentional Teardown:** Explicit exits (`Ctrl-C` or `hop stop`) send a protocol `Bye` frame, releasing subdomains immediately so they can be reused without waiting out the grace window.
- **Versions are checked at the handshake:** The agent announces its protocol and release; `hopd` refuses protocols it can't speak with a message naming the side to upgrade, reports its own release and protocol to authenticated agents (never to a rejected token), and logs which `hop` release each device runs.
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
| `-dns-provider` | `$DNS_PROVIDER` (`cloudflare` if `CLOUDFLARE_API_TOKEN` is set) | DNS host for certificates and records; credentials come from its env vars |
| `-dns-check` | `true` | Wait until `<domain>` and `*.<domain>` resolve before starting |
| `-manage-dns` | `true` | Create missing A records through the DNS provider |
| `-public-ip` | *(detected)* | Address the DNS records should point at |
| `-tls-cert` | *(empty)* | Path to custom TLS certificate fullchain.pem (skips ACME) |
| `-tls-key` | *(empty)* | Path to custom TLS private key.pem |

---

## 🧪 Tests

Run the complete test suite with race detection:

```sh
go test -race ./...
```

---

## 📄 License

Hop is open-source software licensed under the [MIT License](LICENSE).
