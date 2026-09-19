# First-party MCP servers

MCP servers written for this project, as opposed to third-party ones
(`uvx mcp-server-git`, `npx @modelcontextprotocol/server-filesystem`) which you just
reference from an `mcp.json`.

| Directory | Package | What it is |
|---|---|---|
| [`harness/`](harness) | `mcp-switchboard-server-harness` | filesystem, search, git and shell tools for coding agents |

## The convention

One subdirectory per server, each its own Python package:

```
servers/
  my-server/
    pyproject.toml          # name: mcp-switchboard-server-my-server
    src/mcp_switchboard_server_my_server/
    tests/
```

When you add one:

1. Add it to the uv workspace members in the root `pyproject.toml`, and re-run `uv lock`.
2. Add a `packages.<name>` output in `flake.nix` (the uv2nix package set already
   sees it once it is a workspace member).
3. Add it to the test matrix in `.github/workflows/ci.yml`.

## You do not have to put servers here

Nothing about the hub cares where a server comes from. A server in this
directory gets tunnelled exactly like any other: it is just a command in an
`mcp.json` that the client spawns over stdio. This directory exists so that
servers written specifically for this setup have an obvious home and get built,
tested and released along with everything else.

For reference, `tests/fake_mcp_server.py` is a minimal, complete stdio MCP
server (two tools, one of which deliberately fails) used by the end-to-end test.
