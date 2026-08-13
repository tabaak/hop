# Deploying hopd on Ubuntu

Assumes the domain is `hop.vokh.dev` and DNS is hosted at Cloudflare. Substitute
your own values throughout.

## 1. DNS

Two records, both pointing at the VPS IP:

| Type | Name          | Content     | Proxy status |
|------|---------------|-------------|--------------|
| A    | `hop`         | `<VPS IP>`  | **DNS only** |
| A    | `*.hop`       | `<VPS IP>`  | **DNS only** |

The orange cloud must be **off**. Cloudflare's proxy terminates TLS itself,
which both defeats the certificate hopd obtains and puts an intermediary in
front of a connection that is supposed to be end-to-end.

Check they resolve before going further:

```sh
dig +short hop.vokh.dev
dig +short anything.hop.vokh.dev
```

## 2. Cloudflare API token

A wildcard certificate can only be validated by the DNS-01 challenge, so hopd
needs to write a TXT record into the zone.

Cloudflare dashboard → My Profile → API Tokens → Create Token → Custom token:

- **Permissions:** `Zone` → `DNS` → `Edit`, **and** `Zone` → `Zone` → `Read`
- **Zone Resources:** Include → Specific zone → `vokh.dev`

Both permissions are required. `DNS:Edit` writes the challenge record;
`Zone:Read` is what lets the library find the zone ID in the first place. A
token with only `DNS:Edit` fails with a confusing zone-lookup error.

Use a scoped **token**, never the Global API Key — that key can do anything to
every zone on the account.

## 3. Build

The VPS almost certainly doesn't need a Go toolchain; cross-compile locally.

```sh
# Intel/AMD VPS
GOOS=linux GOARCH=amd64 go build -o hopd ./cmd/hopd

# ARM VPS (Ampere, Graviton, some Hetzner instances)
GOOS=linux GOARCH=arm64 go build -o hopd ./cmd/hopd

scp hopd root@<VPS>:/usr/local/bin/hopd
```

Check which you need with `uname -m` on the VPS: `x86_64` → amd64, `aarch64` → arm64.

## 4. Service user, directories, secrets

On the VPS, as root:

```sh
useradd --system --no-create-home --shell /usr/sbin/nologin hop
chmod 755 /usr/local/bin/hopd

mkdir -p /etc/hop
cat > /etc/hop/hopd.env <<'EOF'
CLOUDFLARE_API_TOKEN=<the scoped token from step 2>
HOP_TOKENS=<a long random string>
EOF
chmod 600 /etc/hop/hopd.env
```

Generate the agent token with something like `openssl rand -hex 32`. Anyone
holding it can open tunnels on your domain, so treat it as a password.
`HOP_TOKENS` is comma-separated if you want more than one.

## 4b. Behind an existing Caddy

Skip this section if nothing else is on :80/:443.

If a Caddy container already owns those ports, it fronts hop instead: it holds
the wildcard certificate and reverse-proxies tunnel traffic to hopd on the
Docker bridge address. hopd keeps its own TLS on :7443, because the control
connection speaks hop's protocol rather than HTTP and cannot be proxied.

```
:443 Caddy ── existing site   → app container
            └─ *.hop.vokh.dev → 172.17.0.1:8080 (hopd, plaintext)
:7443 hopd control, own certificate for the bare domain
```

**The stock `caddy` image cannot do this.** It ships no DNS provider plugins, so
it can only solve HTTP-01 and TLS-ALPN-01 — neither of which can validate a
wildcard. The binary needs rebuilding with the Cloudflare module; see
`deploy/caddy/Dockerfile`.

1. Copy `deploy/caddy/Dockerfile` next to your compose file, e.g. `./caddy/Dockerfile`.

2. Confirm the bridge address hopd should bind, and correct the unit if it differs:

   ```sh
   ip -4 addr show docker0 | grep inet
   ```

   `172.17.0.1` is the default. This address is reachable from containers but
   not from the internet, which is why hopd's plaintext ingress is safe there.

3. In `docker-compose.yml`, for the caddy service:

   ```yaml
   caddy:
     build: ./caddy          # replaces: image: caddy:2-alpine
     extra_hosts:
       - "host.docker.internal:host-gateway"
     environment:
       - CF_API_TOKEN=${CF_API_TOKEN}
   ```

4. Put the same Cloudflare token in a `.env` beside the compose file, so Caddy
   can solve DNS-01 too:

   ```sh
   echo 'CF_API_TOKEN=<the scoped token>' >> .env
   chmod 600 .env
   ```

5. Append to the Caddyfile:

   ```
   *.hop.vokh.dev {
       tls {
           dns cloudflare {env.CF_API_TOKEN}
       }
       reverse_proxy host.docker.internal:8080
   }
   ```

6. Build and roll out, checking the existing site still works **before**
   touching hop:

   ```sh
   docker compose build caddy
   docker compose up -d caddy
   docker compose logs -f caddy
   curl -I https://<your existing site>
   ```

   Rollback is putting `image: caddy:2-alpine` back and re-running `up -d`.

## 5. Firewall

```sh
ufw allow 22/tcp     # don't lock yourself out
ufw allow 80/tcp     # redirect to https
ufw allow 443/tcp    # public ingress
ufw allow 7443/tcp   # agent control connections
ufw enable
```

`ufw enable` applies a default-deny policy immediately, so the rule for 22 has
to exist *before* it runs or it drops your current SSH session.

Note that **Docker's published ports bypass ufw entirely** — it writes its own
rules in the `DOCKER-USER` chain, evaluated first. On a Docker host, ufw only
governs ports held by non-container processes.

### Oracle Cloud

Oracle's Ubuntu images ship a restrictive `iptables` INPUT chain ending in
`REJECT ... icmp-host-prohibited`, and instances additionally sit behind a VCN
security list. **Both** layers must permit a port.

In the OCI console: Networking → Virtual Cloud Networks → your VCN → Security
Lists → default → Add Ingress Rule, source `0.0.0.0/0`, TCP, port `7443`.

On the host, insert before the REJECT rule and persist:

```sh
REJECT_LINE=$(sudo iptables -L INPUT -n --line-numbers | awk '/REJECT/ {print $1; exit}')
sudo iptables -I INPUT "$REJECT_LINE" -p tcp --dport 7443 -m state --state NEW -j ACCEPT
sudo netfilter-persistent save
```

**If hopd runs behind a containerised proxy, the same chain blocks the proxy
from reaching it.** Traffic from a container to a host bridge address goes
through INPUT, so the plaintext ingress port needs a rule too:

```sh
REJECT_LINE=$(sudo iptables -L INPUT -n --line-numbers | awk '/REJECT/ {print $1; exit}')
sudo iptables -I INPUT "$REJECT_LINE" -s 172.16.0.0/12 -d 172.17.0.1 -p tcp --dport 8080 -m state --state NEW -j ACCEPT
sudo netfilter-persistent save
```

Scoped to Docker's private ranges and the bridge address, so it isn't publicly
reachable — and hopd binds only that address anyway. The symptom without it is
a **502 from the proxy**, and `nc` from inside the container reporting
`Host is unreachable` (which is the ICMP that `reject-with icmp-host-prohibited`
produces). When testing reachability, probe the *actual* port — a probe against
port 22 succeeds because 22 is in the allowlist, and proves only that routing
works.

## 6. Install the unit

```sh
cp deploy/hopd.service /etc/systemd/system/hopd.service
# edit -domain and -email to match your setup
systemctl daemon-reload
```

## 7. Staging first

Let's Encrypt's production endpoint allows 5 duplicate certificates per week.
A misconfigured DNS token can burn that in one afternoon, and then you wait.
So prove the whole flow against staging, which is far more forgiving.

Edit the unit's `ExecStart` to `-staging=true`, then:

```sh
systemctl start hopd
journalctl -u hopd -f
```

You want to see `obtaining certificate ...` followed by `certificate ready`.
That means the DNS challenge round-tripped, which is the only genuinely
failure-prone step. The certificate itself will be untrusted — that's expected
from staging, and browsers will warn.

Common failures:

| Symptom | Cause |
|---|---|
| zone lookup / "could not find zone" | token is missing `Zone:Read` |
| 403 writing the TXT record | token is missing `DNS:Edit`, or scoped to the wrong zone |
| propagation timeout | record isn't visible yet, or the zone is proxied (orange cloud on) |
| `permission denied` on /var/lib/hop | `StateDirectory=` removed from the unit |

## 8. Switch to production

Once staging succeeds, set `-staging=false` in the unit, remove the staging
artifacts so it doesn't serve a cached untrusted cert, and restart:

```sh
rm -rf /var/lib/hop/certs
systemctl daemon-reload
systemctl restart hopd
journalctl -u hopd -f
systemctl enable hopd
```

Verify from your laptop:

```sh
curl -I https://hop.vokh.dev
```

A 404 from hop is the healthy answer — no tunnel is claiming that name, but TLS
terminated correctly.

## 9. Connect an agent

```sh
export HOP_SERVER=hop.vokh.dev:7443
export HOP_TOKEN=<the token from HOP_TOKENS>

hop http 3000 --sub myapp
```

Then `https://myapp.hop.vokh.dev` reaches `localhost:3000`.

## Renewal

certmagic renews in the background, roughly 30 days before expiry, using the
same DNS-01 flow. Nothing to schedule. Keep `/var/lib/hop` intact and keep the
Cloudflare token valid — if you rotate it, update `/etc/hop/hopd.env` and
restart.
