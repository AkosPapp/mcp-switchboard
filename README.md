# mcp-reverse-proxy-client

Outbound-only reverse proxy for [MCP](https://modelcontextprotocol.io) servers.
It reads a project's `mcp.json`, launches each configured server as a local
subprocess, and opens one authenticated, persistent **outbound** WebSocket
tunnel per server to an MCP gateway ([Context Forge](https://github.com/IBM/mcp-context-forge)).
No inbound port is ever opened — this is the mirror image of the usual
mcp-proxy tools, which run a local HTTP server that something else has to
reach.

Two pieces, published separately:

1. **`install.sh`** — a POSIX-shell bootstrap you `curl | sh`. It makes sure
   `npx` and `uvx` exist, then hands off to the real tool via `uvx`.
2. **This PyPI package** — the actual reverse-proxy client, run via
   `uvx mcp-reverse-proxy-client ...`.

## Quick start

```sh
curl -fsSL https://akospapp.github.io/agent-reverse-proxy/install.sh | sh -s -- \
  --gateway-url https://gateway.example.com \
  --token "$MY_GATEWAY_TOKEN"
```

or, if you already have `uv` installed:

```sh
uvx mcp-reverse-proxy-client --gateway-url https://gateway.example.com --token "$TOKEN"
```

## CLI

| Flag | Env var | Required | Notes |
|---|---|---|---|
| `--gateway-url` | `REVERSE_PROXY_GATEWAY` | yes | Same env var name Context Forge's own `mcpgateway.reverse_proxy` uses, so a value exported for that tool works unchanged here. |
| `--token` | `REVERSE_PROXY_TOKEN` | yes | Same convention as above. |
| `--config` | — | no | Path to the MCP server config. Defaults to `./mcp.json`, resolved **strictly relative to the current working directory** — no upward directory search. |
| `--reconnect-delay` | — | no | Initial reconnect backoff in seconds (default `1.0`). Doubles on each attempt, capped at 60s — same shape Context Forge's client uses. |
| `--max-retries` | — | no | Per-server reconnect attempts before giving up, `0` = infinite (default). |
| `--keepalive` | — | no | Heartbeat interval in seconds (default `30`). |
| `--log-level` | — | no | `DEBUG`/`INFO`/`WARNING`/`ERROR`/`CRITICAL` (default `INFO`). |

Any other argument is rejected with an error rather than silently ignored,
since `install.sh` forwards its own arguments to this tool blindly.

## Config file

Two shapes are recognized in the same `mcp.json`:

### 1. Plain command entries (default)

The widely-used shape from Claude Desktop / Cursor / VS Code:

```json
{
  "mcpServers": {
    "git": {
      "command": "uvx",
      "args": ["mcp-server-git"],
      "env": { "SOME_VAR": "value" }
    }
  }
}
```

Each entry is spawned directly as `command args...` with `env` merged on top
of the current environment.

### 2. FastMCP-style entries

**Discriminator rule (exact): if an entry contains a top-level `source` key,
it is treated as a [FastMCP config](https://gofastmcp.com/public/schemas/fastmcp.json/v1.json)
instead of a plain command.** A config file can also *be* a bare FastMCP
config itself (no `mcpServers` wrapper) if its top level has a `source` key —
in that case it's treated as a single server named after its `name` field or
the file's stem.

```json
{
  "mcpServers": {
    "my-fastmcp-server": {
      "source": { "path": "server.py", "entrypoint": "mcp" },
      "environment": { "dependencies": ["httpx"] },
      "deployment": { "transport": "stdio" }
    }
  }
}
```

FastMCP-style entries are launched with `fastmcp run <generated-config>`
(via `uvx`), letting FastMCP itself resolve the `environment`/`source`
blocks with `uv`. **Only `stdio` transport is tunneled** — the wire protocol
only bridges stdin/stdout, so `deployment.transport` is forced to `stdio`
regardless of what the entry declares (a warning is logged if it declared
something else).

## Protocol

Each server's tunnel is wire-compatible with Context Forge's own
`mcpgateway.reverse_proxy`: it connects to `<gateway-url>/reverse-proxy/ws`,
authenticates with `Authorization: Bearer <token>` and `X-Session-ID`, and
exchanges the same JSON envelope shape
(`{"type": "register" | "request" | "response" | "notification" | "heartbeat" | "unregister" | "error", "sessionId": ..., ...}`).
All servers in a config run concurrently from a single invocation; one
tunnel reconnecting or dying does not affect the others. `SIGINT`/`SIGTERM`
trigger a clean shutdown: each tunnel unregisters, closes its WebSocket, and
terminates (then kills, if needed) its subprocess — no orphaned processes.

## Shell installer (`install.sh`)

Responsibilities, in order:

1. If `npx` and `uvx` are both already on `PATH`, skip straight to step 4.
2. Else, if `nix` is on `PATH`, use `nix shell nixpkgs#nodejs nixpkgs#uv` to
   provide whatever's missing for this invocation only — nothing is
   permanently installed.
3. Else, install natively without `sudo` or touching system package state:
   - `uv`/`uvx` via the official installer (`astral.sh/uv/install.sh`).
   - Node/`npx` by downloading the official Node.js LTS tarball for the
     detected OS/arch straight from `nodejs.org` into a cache directory
     (`~/.cache/mcp-reverse-proxy-installer`) and using its bundled `npx`
     for this run. The pinned version can be overridden with the
     `NODE_VERSION` env var.
4. Every download works with either `curl` or `wget` (whichever is present);
   the script fails loudly if neither exists.
5. Once both tools are available, it execs
   `uvx mcp-reverse-proxy-client "$@"`, forwarding all of the installer's
   own arguments unmodified.

The script is POSIX `sh`, idempotent, and safe to re-run.

## Package / repo naming

- PyPI package: `mcp-reverse-proxy-client`
- Console script: `mcp-reverse-proxy-client`
- GitHub repo: [`AkosPapp/agent-reverse-proxy`](https://github.com/AkosPapp/agent-reverse-proxy)
- Installer hosting: GitHub Pages, deployed by CI —
  `https://akospapp.github.io/agent-reverse-proxy/install.sh`

## CI / releasing

Three workflows under `.github/workflows/`:

- **`ci.yml`** — runs the test suite (Python 3.9 and 3.12) and shellchecks
  `install.sh` on every push and pull request.
- **`pages.yml`** — on every push to `main` that touches `install.sh`,
  publishes it to GitHub Pages so the install URL above always serves the
  latest committed script. Also runnable manually (`workflow_dispatch`).
- **`publish-pypi.yml`** — on every GitHub Release publish, builds the sdist
  + wheel and uploads them to PyPI with `twine`, authenticating via the
  `PYPI_API_TOKEN` repo secret. It first checks that the release tag
  (`vX.Y.Z`) matches the version in `pyproject.toml` and fails loudly on a
  mismatch, so you can't accidentally publish the wrong version.

**One-time setup before these run:**

1. **GitHub Pages**: repo Settings → Pages → Source → "GitHub Actions".
2. **PyPI token**: create an API token scoped to this project on PyPI
   (or an account-wide token for the very first publish, since the project
   won't exist yet), then add it as a repo secret named `PYPI_API_TOKEN`
   (Settings → Secrets and variables → Actions).

**To cut a release:** bump `version` in `pyproject.toml`, commit, tag as
`vX.Y.Z` (matching that version exactly), and publish a GitHub Release from
that tag — `publish-pypi.yml` does the rest.
