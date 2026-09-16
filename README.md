# mcp-switchboard

Run MCP servers on machines behind NAT, and use their tools from anywhere — n8n,
an agent, or the built-in web console — without opening a single inbound port.

A **client** on each machine spawns your local stdio MCP servers and opens one
*outbound* WebSocket to a **hub**. The hub speaks MCP to each tunnelled server,
aggregates every tool into one endpoint, and serves it over Streamable HTTP. It
also gives you a console to browse what is connected, call any tool by hand, and
read back every call that has ever been made.

```
┌── your laptop, a NATed box, anywhere ─────────────────┐
│ mcp-switchboard-client                                │
│   mcp.json → spawns local stdio MCP servers           │
│   one outbound WSS connection, all servers multiplexed│
└───────────────────────┬───────────────────────────────┘
                        │  wss://…/tunnel/v1   (outbound only, token)
┌───────────────────────▼───────────────────────────────┐
│ mcp-switchboard-hub                                   │
│                                                       │
│  :8097  tunnel endpoint  ← the only public listener   │
│  ──────────────── loopback only ───────────────────   │
│  :8099  /mcp      Streamable HTTP  ← n8n, agents      │
│         /         web console                         │
│         /api/*    JSON API                            │
│         /metrics  Prometheus                          │
│                                                       │
│  → Loki (batched)        → SQLite call log            │
└───────────────────────────────────────────────────────┘
```

## Why this exists

This replaces the reverse-proxy feature of [mcp-context-forge](https://github.com/IBM/mcp-context-forge),
which cannot work as advertised. In every released version (checked 0.9.0 through
1.0.10), its tunnel endpoint accepts connections and then drops everything:

```python
elif msg_type in ("response", "notification"):
    # TODO: Route to appropriate MCP client      ← responses are logged and discarded
```
```python
# TODO: Implement message queue for SSE delivery ← the SSE endpoint only sends keepalives
```

Nothing registers tunnelled servers into its tool catalog, so the tools never
appear to a consumer. Its protocol also has both peers answer `heartbeat` with
`heartbeat`, an infinite loop we measured at ~80 messages/second per session.

## Quick start

### The hub

NixOS, via the flake:

```nix
{
  inputs.mcp-switchboard.url = "github:AkosPapp/mcp-switchboard";

  # in your host config
  imports = [ inputs.mcp-switchboard.nixosModules.default ];

  services.mcp-switchboard = {
    enable = true;
    # Any setting takes a literal or a path to a file holding the value, so
    # sops-nix secrets need no special handling.
    tunnelToken = "/run/secrets/mcp-switchboard/tunnel-token";
    tunnel.host = "0.0.0.0";        # reachable by your clients
    private.host = "127.0.0.1";     # console + /mcp stay local
    loki.enable = true;
    prometheus.register = true;
  };
}
```

Or with Docker (note it binds loopback by default, so override the hosts):

```sh
docker run -p 8097:8097 -p 8099:8099 \
  -e MCP_SWITCHBOARD_TUNNEL_TOKEN=... \
  -e MCP_SWITCHBOARD_TUNNEL_HOST=0.0.0.0 \
  -e MCP_SWITCHBOARD_PRIVATE_HOST=0.0.0.0 \
  -v mcp-switchboard:/var/lib/mcp-switchboard \
  ghcr.io/akospapp/mcp-switchboard-hub
```

### The client

On any machine with MCP servers, next to an `mcp.json`:

```sh
curl -fsSL https://akospapp.github.io/mcp-switchboard/install.sh | sh -s -- \
  --hub-url wss://switchboard.example.com --token "$TOKEN"
```

or, if you already have `uv`:

```sh
uvx mcp-switchboard-client --hub-url wss://switchboard.example.com --token "$TOKEN"
```

The installer only makes sure `npx` and `uvx` exist (via `nix shell` if you have
Nix, otherwise per-user installs with no sudo) and then hands off to `uvx`.

### `mcp.json`

The usual shape, as used by Claude Desktop, Cursor and VS Code:

```json
{
  "mcpServers": {
    "git":  { "command": "uvx", "args": ["mcp-server-git", "--repository", "."] },
    "fetch": { "command": "uvx", "args": ["mcp-server-fetch"] }
  }
}
```

FastMCP-style entries are also recognised — any entry containing a top-level
`source` key is launched with `fastmcp run` instead. Only stdio transport is
tunnelled, since the wire protocol bridges stdin/stdout.

## Connecting n8n

Point n8n's **MCP Client Tool** node (transport: HTTP Streamable) at the hub. The
same tools are served at three scopes, so you can narrow the list instead of
scrolling one enormous flat one:

| URL | What it lists | Tool names |
|---|---|---|
| `/mcp` | everything, every machine | `legion5__git__git_status` |
| `/mcp/host/legion5` | one machine | `git__git_status` |
| `/mcp/host/legion5/server/git` | one server | `git_status` |

Every tool is tagged with its origin three ways, because different clients
surface different fields: in the **name** (above), in the **title**
(`git_status · git @ legion5`), and at the front of the **description**
(`[legion5 · git] …`) — that last one matters because it is what an LLM reads
when choosing a tool. The machine-readable `_meta` carries `host`, `server`,
`connectionId` and `upstreamName` too.

The console shows each tool's fully-resolved exposed name, so there is never a
guess about what n8n will see.

## Two listeners, on purpose

The tunnel endpoint is the only thing that needs to face the internet, so it is
the only thing on the public listener, and it always requires a bearer token.
The console, the API, `/mcp` and `/metrics` live on a separate listener that
binds loopback and is **unauthenticated by default** — not because auth was
skipped, but because not being reachable is a stronger guarantee than a shared
secret. If you do expose it, set `MCP_SWITCHBOARD_PRIVATE_TOKEN`.

## Configuration

Everything is configured with `MCP_SWITCHBOARD_*` environment variables, read
from the process environment and optionally from a `.env` file. See
[`.env.example`](.env.example) for the full list.

Any value that starts with `/` and points at an existing regular file is replaced
by that file's contents. So secrets are passed as paths, with no separate
`*_FILE` variables:

```sh
MCP_SWITCHBOARD_TUNNEL_TOKEN=literal-value
MCP_SWITCHBOARD_TUNNEL_TOKEN=/run/secrets/mcp-switchboard/tunnel-token
```

Directories are never substituted, so path-valued settings like
`MCP_SWITCHBOARD_DATA_DIR` behave normally. A setting marked secret that points
at a missing file fails at startup rather than silently authenticating with the
literal string `/run/secrets/...`.

## Observability

`/metrics` exposes `mcpsb_connections_active`, `mcpsb_servers{state}`,
`mcpsb_tools_total{label,server}`, `mcpsb_tool_calls_total{label,server,tool,status,source}`,
`mcpsb_tool_call_duration_seconds`, `mcpsb_tunnel_frames_total{direction}` and
`mcpsb_loki_dropped_total`.

With `MCP_SWITCHBOARD_LOKI_ENABLED=true`, each tool call and lifecycle event is
batched and pushed to Loki. The queue is bounded and drops (counting the drops)
rather than ever blocking or failing a tool call if Loki is down.

Every call — from the console and from MCP consumers alike — is also written to a
SQLite log with its full arguments and result, browsable in the console and
subject to `RETENTION_DAYS` / `MAX_ROWS`.

## Layout

```
hub/          the service: tunnel, MCP endpoint, console, API, exporters
client/       the tunnel client, deliberately dependency-light (no MCP dep)
servers/      first-party MCP servers (see its README)
nix/          NixOS module, uv2nix package set, VM test
tests/        end-to-end test: real client, real MCP server, real consumer
docs/         PROTOCOL.md, the tunnel wire format
```

## Development

```sh
nix develop                      # or: uv sync
pytest hub/tests client/tests -q # unit
pytest tests -q                  # end to end (spawns a real client process)
nix build .#checks.x86_64-linux.vm -L   # NixOS VM test, needs KVM
```

The client and hub each carry byte-identical copies of `protocol.py` and
`envconf.py` so the client needs no dependency on the hub; a test fails if they
drift.

## Licence

MIT.
