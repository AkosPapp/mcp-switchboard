# Build: MCP reverse-proxy client (installer script + PyPI package)

Two deliverables, meant to be used together but built/published separately:

1. A tiny shell **bootstrap installer**, hosted at a static URL, that
   ensures `npx` and `uvx` are available, then runs the real tool via `uvx`.
2. A **PyPI package** that is the actual reverse proxy: it reads a
   project's MCP server config, and for each configured server opens an
   authenticated outbound tunnel to an MCP gateway (Context Forge), so
   local servers are reachable through the gateway without needing any
   inbound network access themselves.

None of the existing mcp-proxy tools fit this shape (they run a local
HTTP server that something else has to reach) — this is deliberately the
opposite: every connection this tool makes is outbound.

---

## 1. Shell installer

**Distribution**: hosted at one static, stable URL — a GitHub raw file on
a pinned release tag, or GitHub Pages, is fine. Should support the usual
bootstrap pattern:

```sh
curl -fsSL https://<static-url>/install.sh | sh -s -- --gateway-url ... --token ...
```

**Responsibilities**, in order:

1. Check whether `npx` and `uvx` are already on `PATH`. If both are
   present, skip straight to step 4.
2. If either is missing, check whether `nix` is available on `PATH`. If
   so, use `nix shell` (e.g. `nixpkgs#nodejs nixpkgs#uv`) to provide
   whatever's missing for the duration of this script's execution — don't
   permanently install anything in this case.
3. If `nix` isn't available either, install natively:
   - **uv/uvx**: official installer, `curl -LsSf https://astral.sh/uv/install.sh | sh`
     (or the wget equivalent — see redundancy below).
   - **npx** (via Node.js): prefer a self-contained approach that doesn't
     assume a system package manager — e.g. fetching the official Node.js
     LTS release tarball for the detected OS/arch directly from
     nodejs.org and extracting it into a cache directory, then using that
     bundled `npx` for this run. Don't require sudo or modify system
     package state.
4. **Redundancy**: every download in this script must work with either
   `curl` or `wget` — write one small fetch helper that tries `curl`
   first, falls back to `wget`, and fails with a clear error if neither
   exists, rather than assuming `curl` unconditionally.
5. Once `npx` and `uvx` are confirmed available (however they got there),
   hand off entirely: `exec uvx <pypi-package-name> "$@"`, forwarding
   every argument the installer script itself received, unmodified.

**Constraints**:
- POSIX `sh` compatible — don't rely on bashisms, since this runs via
  `curl | sh` on arbitrary environments.
- No permanent system modification when the `nix` path is used.
- Idempotent and safe to re-run.
- Fails loudly and early if it truly can't get `npx`/`uvx` working, rather
  than silently falling through to a broken `uvx` invocation.

---

## 2. PyPI package — the actual reverse proxy

**Distribution**: published to PyPI with a console-script entry point, so
`uvx <package-name> <args>` runs it directly without a separate install
step (this is what the shell installer ultimately execs into).

**CLI**:
- `--gateway-url` (required) — the Context Forge / MCP gateway address to
  tunnel to. Should also be settable via an env var for cases where
  passing it as a plain CLI arg isn't desirable (shell history, process
  list visibility) — mirror the env var convention Context Forge's own
  reverse-proxy client already uses if there's a natural match, so
  behavior is consistent for anyone who's used that.
- `--token` (required) — bearer token for authenticating the outbound
  connection. Same env var consideration as above.
- `--config` (optional) — path to the MCP server config file. Defaults to
  `./mcp.json` in the current working directory if not given.
- All other args: reject unknowns with a clear error rather than
  silently ignoring them, since the installer forwards arguments blindly.

**Config file handling — two shapes to support**:
1. **Default / primary**: the widely-used `mcp.json` shape —
   `{"mcpServers": {"<name>": {"command": "...", "args": [...], "env": {...}}}}`
   — the same convention Claude Desktop, Cursor, VS Code etc. use. Each
   entry describes one local stdio MCP server to launch and tunnel.
2. **FastMCP-compatible**: also recognize configs that match FastMCP's
   own schema (https://gofastmcp.com/public/schemas/fastmcp.json/v1.json)
   — a single-server shape with `source` (path to a Python file + entry
   point), `environment` (uv-based dependency/venv setup), and
   `deployment` (transport/host/port/etc.) blocks. Decide and document
   clearly: how an individual `mcpServers` entry signals "this one is
   FastMCP-style, run it via FastMCP's own launch mechanism using its
   `environment`/`source` config" vs. "this one is a plain command to
   spawn directly" — the presence of a `source` key is a reasonable
   discriminator, but make the actual rule explicit in the README rather
   than leaving it implicit in the code.

**Core behavior**:
- For every server in the resolved config, run it as a local subprocess
  (stdio, or per its own declared transport for FastMCP-style entries)
  and open one outbound, authenticated, persistent connection to
  `--gateway-url` per server, tunneling MCP protocol traffic through it —
  same conceptual model as Context Forge's own `mcpgateway.reverse_proxy`
  (outbound-only, no inbound port ever opened).
- Run all configured servers concurrently from a single invocation.
- Auto-reconnect on connection loss with backoff; one server's connection
  dying or reconnecting must not affect the others.
- Clean shutdown on SIGINT/SIGTERM — terminate child processes and close
  tunnels gracefully, don't leave orphaned subprocesses behind.
- Log per-server, clearly enough to tell which tunnel is up/down at a
  glance.

**Packaging**:
- Standard `pyproject.toml`-based package (build backend of your choice),
  versioned, with a console-script entry point matching what the shell
  installer invokes.
- Choose and document the actual PyPI package name and GitHub repo as
  part of this work — not specified here.

---

## Open decisions left to you

- PyPI package name / GitHub repo name / install-script hosting URL.
- Exact discriminator logic between plain `mcpServers` entries and
  FastMCP-style entries in the same config file.
- Reconnect/backoff parameters and their defaults.
- Whether `--config` does any upward directory search for `mcp.json` or
  strictly resolves relative to the current working directory (current
  working directory only is the simpler default — deviate only with a
  clear reason).
