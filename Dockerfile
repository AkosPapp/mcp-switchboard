# mcp-switchboard-hub container image.
#
# IMPORTANT - the hub binds 127.0.0.1 by default, which inside a container means
# "reachable from nothing". Nothing is baked in here on purpose: the service is
# configured entirely through MCP_SWITCHBOARD_* environment variables, so you
# must set at least
#
#   -e MCP_SWITCHBOARD_TUNNEL_HOST=0.0.0.0
#   -e MCP_SWITCHBOARD_PRIVATE_HOST=0.0.0.0        # or leave private on loopback
#   -e MCP_SWITCHBOARD_TUNNEL_TOKEN=/run/secrets/tunnel-token
#
# (or run with --network=host) or the listeners will be unreachable.
#
# Secrets: any MCP_SWITCHBOARD_* value that starts with "/" and resolves to an
# existing regular file is replaced by that file's contents at startup, so mount
# a token file and pass its path rather than passing the token itself. Directory
# values such as MCP_SWITCHBOARD_DATA_DIR are never substituted.
#
#   docker run --rm \
#     -p 8097:8097 -p 8099:8099 \
#     -e MCP_SWITCHBOARD_TUNNEL_HOST=0.0.0.0 \
#     -e MCP_SWITCHBOARD_PRIVATE_HOST=0.0.0.0 \
#     -e MCP_SWITCHBOARD_TUNNEL_TOKEN=/run/secrets/tunnel-token \
#     -v /srv/mcp-switchboard:/var/lib/mcp-switchboard \
#     -v /etc/secrets/tunnel-token:/run/secrets/tunnel-token:ro \
#     ghcr.io/akospapp/mcp-switchboard-hub:latest

# ---------------------------------------------------------------------------
# console stage: the web console, built by npm from its own lockfile
# ---------------------------------------------------------------------------
# --platform=$BUILDPLATFORM: the console is static JS/CSS, architecture-
# independent, so it is built once for the machine doing the building rather
# than once per $TARGETARCH.
FROM --platform=$BUILDPLATFORM node:22-alpine AS console

WORKDIR /src/hub/web

# Dependencies first, so editing the console's source does not reinstall
# node_modules on every build.
COPY hub/web/package.json hub/web/package-lock.json ./
RUN npm ci

COPY hub/web/ ./
RUN npm run build

# ---------------------------------------------------------------------------
# build stage: one static binary, console included
# ---------------------------------------------------------------------------
# --platform=$BUILDPLATFORM pins this stage to the machine doing the building,
# and the go build below targets $TARGETARCH. Go cross-compiles, so an arm64
# image needs no QEMU: emulating a compiler was the expensive half of the image
# this replaces.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src/hub

# Dependencies first, so editing the hub's own source does not re-download the
# module cache on every build.
COPY hub/go.mod hub/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY hub/ ./
# hub/web/embed.go's `go:embed all:dist` needs this before the build below.
COPY --from=console /src/hub/web/dist ./web/dist

# Supplied by buildx per target platform; VERSION comes from the workflow's
# image tag, and defaults so a plain `docker build` still works.
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.0.0+docker

# CGO_ENABLED=0 with a pure-Go SQLite driver is what makes the runtime stage
# able to be this small - no libc to match, nothing to link against - and it is
# also what makes the cross-build above a plain environment variable rather than
# a cross toolchain.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/mcp-switchboard-hub ./cmd/mcp-switchboard-hub

# ---------------------------------------------------------------------------
# runtime stage: the binary, a user to run it as, and nothing else
# ---------------------------------------------------------------------------
#
# alpine rather than scratch: a non-root user needs /etc/passwd, the volume
# needs an owner, and TLS to an LLM provider or a Loki endpoint needs a CA
# bundle. That is three reasons for ~8MB.
FROM alpine:3.21 AS runtime

RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 10001 switchboard \
 && adduser -S -u 10001 -G switchboard -h /var/lib/mcp-switchboard switchboard \
 && install -d -o switchboard -g switchboard -m 0750 /var/lib/mcp-switchboard

COPY --from=build /out/mcp-switchboard-hub /usr/local/bin/mcp-switchboard-hub

WORKDIR /var/lib/mcp-switchboard
USER switchboard

VOLUME ["/var/lib/mcp-switchboard"]

# Both listeners. The tunnel one is the only that is meant to face a network.
EXPOSE 8097 8099

# The hub answers /health on both listeners, without a token, precisely so this
# works. curl is not installed, so the check uses the binary's own listener via
# wget from busybox.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:${MCP_SWITCHBOARD_PRIVATE_PORT:-8099}/health" || exit 1

LABEL org.opencontainers.image.title="mcp-switchboard-hub" \
      org.opencontainers.image.description="Aggregating MCP gateway for outbound tunnels" \
      org.opencontainers.image.source="https://github.com/AkosPapp/mcp-switchboard" \
      org.opencontainers.image.licenses="MIT"

ENTRYPOINT ["mcp-switchboard-hub"]
