# Caddy with the Cloudflare DNS provider module.
#
# The official caddy image ships no DNS provider plugins, so it can only solve
# HTTP-01 and TLS-ALPN-01 challenges. A wildcard certificate (*.hop.vokh.dev)
# can only be validated by DNS-01, so the binary has to be rebuilt with the
# provider compiled in.
#
# Keep the version tags on both stages in step with each other.
FROM caddy:2-builder-alpine AS builder
RUN xcaddy build --with github.com/caddy-dns/cloudflare

FROM caddy:2-alpine
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
