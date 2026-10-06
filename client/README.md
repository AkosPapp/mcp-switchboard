# mcp-switchboard-client

The client half of [mcp-switchboard](https://github.com/AkosPapp/mcp-switchboard).

It reads an `mcp.json`, spawns the local stdio MCP servers it describes, and
opens a single **outbound** WebSocket to an `mcp-switchboard-hub`, multiplexing
every server over that one connection. No inbound port is ever opened, so it
works from behind NAT with nothing forwarded.

The client speaks no MCP itself — it is a pipe, shuttling each server's
stdin/stdout across the tunnel. All MCP logic lives in the hub. That is why its
only own dependencies are `websockets` and `certifi` (plus the harness package,
which brings the `mcp` SDK) (a bundled CA bundle, because
`uvx`'s portable Pythons often cannot find a system certificate store).

## Running

```sh
uvx mcp-switchboard-client --hub-url wss://switchboard.example.com --token "$TOKEN"
```

or bootstrap `uvx`/`npx` first with the installer:

```sh
curl -fsSL https://akospapp.github.io/mcp-switchboard/install.sh | sh -s -- \
  --hub-url wss://switchboard.example.com --token "$TOKEN"
```

`mcp.json` uses the familiar shape, resolved from the current directory. It is optional:
without one, the client tunnels just the built-in coding harness (see below):

```json
{
  "mcpServers": {
    "git": { "command": "uvx", "args": ["mcp-server-git", "--repository", "."] }
  }
}
```

An entry may set `"project"` to group it under a project name (a top-level
`"project"` is the default for all entries). The client sends it to the hub, which
uses it in tool names and project-scoped endpoints. Names must not contain `__`.

A first-party [coding harness](../servers/harness) (file, search, git and shell
tools) is added as a server named `harness` by default, confined to the directory you start
the client in. Turn it off with
`--no-harness` or `MCP_SWITCHBOARD_HARNESS=false`; an `mcp.json` entry named
`harness` replaces it.

The client also collects the instruction files your repository carries for coding
agents (`AGENTS.md`, `CLAUDE.md`, `.cursorrules`, `.github/copilot-instructions.md`,
also at any depth, capped at 8 files / 64 KiB) from the start directory and ships them
to the hub, which puts them into the agent's system prompt; it re-sends them whenever
they change, so an edit to `AGENTS.md` reaches a running session. Turn it off with
`--no-instructions` or `MCP_SWITCHBOARD_INSTRUCTIONS=false`.

A push is treated as what it is: the harness marks `git_push` irreversible in its tool `_meta`, so
the hub asks for approval before any agent can run it — even one set to `approval=never`. If that
friction is wrong for your deployment, that is a decision to make deliberately, not by silence.

Alongside them the client ships a short environment brief of the host your tools run
on — user, hostname, git branch/dirtiness (remote with credentials redacted), what the
file tools may write, tool presence; sudo is reported only if provable without a
prompt, and network reachability is never probed — refreshed like the instruction
files and marked stale by the hub if a client vanishes. Turn it off with
`--no-env-brief` or `MCP_SWITCHBOARD_ENV_BRIEF=false`.

Settings can also come from `MCP_SWITCHBOARD_*` environment variables or a
`.env` file (`HUB_URL`, `TUNNEL_TOKEN`, `LABEL`, `CONFIG`, …). Any value that
starts with `/` and points at an existing regular file is read from that file, so
a token can be passed as a path.

`LABEL` defaults to the machine's hostname and is how the hub tags this
machine's tools, so consumers can tell which host a tool lives on.

See the [main README](../README.md) for the full picture.
