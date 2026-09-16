# mcp-switchboard-client

The client half of [mcp-switchboard](https://github.com/AkosPapp/mcp-switchboard).

It reads an `mcp.json`, spawns the local stdio MCP servers it describes, and
opens a single **outbound** WebSocket to an `mcp-switchboard-hub`, multiplexing
every server over that one connection. No inbound port is ever opened, so it
works from behind NAT with nothing forwarded.

The client speaks no MCP itself — it is a pipe, shuttling each server's
stdin/stdout across the tunnel. All MCP logic lives in the hub. That is why its
only dependency is `websockets`.

## Running

```sh
uvx mcp-switchboard-client --hub-url wss://switchboard.example.com --token "$TOKEN"
```

or bootstrap `uvx`/`npx` first with the installer:

```sh
curl -fsSL https://akospapp.github.io/mcp-switchboard/install.sh | sh -s -- \
  --hub-url wss://switchboard.example.com --token "$TOKEN"
```

`mcp.json` uses the familiar shape, resolved from the current directory:

```json
{
  "mcpServers": {
    "git": { "command": "uvx", "args": ["mcp-server-git", "--repository", "."] }
  }
}
```

Settings can also come from `MCP_SWITCHBOARD_*` environment variables or a
`.env` file (`HUB_URL`, `TUNNEL_TOKEN`, `LABEL`, `CONFIG`, …). Any value that
starts with `/` and points at an existing regular file is read from that file, so
a token can be passed as a path.

`LABEL` defaults to the machine's hostname and is how the hub tags this
machine's tools, so consumers can tell which host a tool lives on.

See the [main README](../README.md) for the full picture.
