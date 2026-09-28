# Build stage
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS builder

ARG TARGETOS=linux
ARG TARGETARCH

WORKDIR /src

RUN apk add --no-cache ca-certificates tzdata

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build statically linked binary
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w -X main.version=v1.0.0" -o /hopd ./cmd/hopd

# Final runtime image
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

# Create data and config directories
RUN mkdir -p /etc/hop /var/lib/hop/certs

COPY --from=builder /hopd /usr/local/bin/hopd

# Ports:
# 80: HTTP redirect
# 443: HTTPS public tunnel ingress
# 7443: Agent control listener (TLS)
EXPOSE 80 443 7443

VOLUME ["/etc/hop", "/var/lib/hop/certs"]

ENTRYPOINT ["/usr/local/bin/hopd"]
