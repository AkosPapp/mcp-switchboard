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
# build stage: resolve the locked workspace into a self-contained venv
# ---------------------------------------------------------------------------
FROM python:3.12-slim AS build

# Pinned so an image rebuild cannot silently change the resolver.
COPY --from=ghcr.io/astral-sh/uv:0.11.21 /uv /usr/local/bin/uv

ENV UV_LINK_MODE=copy \
    UV_COMPILE_BYTECODE=1 \
    UV_PYTHON_DOWNLOADS=never \
    UV_PROJECT_ENVIRONMENT=/opt/venv

WORKDIR /src

# The repo is a uv workspace: the root pyproject.toml is a bare marker and the
# single uv.lock covers both members. Copy the manifests first so dependency
# resolution caches independently of the source.
COPY pyproject.toml uv.lock ./
COPY hub/pyproject.toml ./hub/
COPY client/pyproject.toml ./client/

RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --locked --no-dev --no-install-workspace --package mcp-switchboard-hub

# Now the code. --no-editable makes the venv hold a real installed copy, so the
# runtime stage does not need /src at all.
COPY hub ./hub

RUN --mount=type=cache,target=/root/.cache/uv \
    uv sync --locked --no-dev --no-editable --package mcp-switchboard-hub

# ---------------------------------------------------------------------------
# runtime stage: interpreter + venv, no uv, no build tooling, no source tree
# ---------------------------------------------------------------------------
FROM python:3.12-slim AS runtime

RUN groupadd --system --gid 10001 switchboard \
 && useradd --system --uid 10001 --gid switchboard --home /var/lib/mcp-switchboard switchboard \
 && install -d -o switchboard -g switchboard -m 0750 /var/lib/mcp-switchboard

COPY --from=build /opt/venv /opt/venv

ENV PATH="/opt/venv/bin:$PATH" \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1

WORKDIR /var/lib/mcp-switchboard
USER switchboard

VOLUME ["/var/lib/mcp-switchboard"]

LABEL org.opencontainers.image.title="mcp-switchboard-hub" \
      org.opencontainers.image.description="Aggregating MCP gateway for outbound tunnels" \
      org.opencontainers.image.source="https://github.com/AkosPapp/mcp-switchboard" \
      org.opencontainers.image.licenses="MIT"

ENTRYPOINT ["mcp-switchboard-hub"]
