# mcp-switchboard-hub

The server half of [mcp-switchboard](https://github.com/AkosPapp/mcp-switchboard).

It accepts outbound WebSocket tunnels from machines running local stdio MCP
servers, speaks MCP to each tunnelled server, and re-serves every tool from one
place:

- **`/mcp`** — Streamable HTTP MCP endpoint for consumers (n8n, agents), also
  available scoped per machine (`/mcp/host/{label}`), per project
  (`/mcp/host/{label}/project/{project}`) or per server
  (`/mcp/host/{label}/server/{server}`, optionally within a project).
- **`/`** — a web console to browse connections, servers and tools, call any tool
  by hand, read back every call, and copy the URLs and client-install command
  from the Endpoints panel.
- **`/api/*`** — the JSON API behind the console.
- **`/metrics`** — Prometheus.
- Loki export and a SQLite call log.

## Running

```sh
pip install mcp-switchboard-hub
MCP_SWITCHBOARD_TUNNEL_TOKEN=... mcp-switchboard-hub
```

It listens on two ports on purpose: the tunnel endpoint (default
`127.0.0.1:8097`) is the only thing meant to face the internet and always
requires a bearer token, while the console, API, `/mcp` and `/metrics` live on a
second listener (default `127.0.0.1:8099`) that binds loopback and is
unauthenticated by default.

Everything is configured through `MCP_SWITCHBOARD_*` environment variables, read
from the environment or a `.env` file. Any value that starts with `/` and points
at an existing regular file is replaced by that file's contents, so secrets can
be passed as paths without separate `*_FILE` variables.

See the [main README](../README.md) for the full picture, and
[docs/PROTOCOL.md](../docs/PROTOCOL.md) for the tunnel wire format.
