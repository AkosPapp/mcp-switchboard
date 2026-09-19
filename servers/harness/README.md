# mcp-switchboard-server-harness

A first-party stdio MCP server that gives a coding agent the basics on a
remote machine: filesystem, search, git and shell tools. It is a normal MCP
server, so [mcp-switchboard-client](../../client) tunnels it like any other.

Tools (24; full schemas and behaviour in [spec.md](../../spec.md#4-harness-server)):

- **Files:** `file_read` (paged), `file_write`, `file_delete`, `file_move`, `dir_list`, `tree_of_files`,
  `find_files` (honours `.gitignore`), `ripgrep`, `read_lines`, `edit_file`
- **Run:** `run_command`, `run_python`, and `process_start` / `process_read` / `process_kill` for
  long-running work
- **Git:** `git_status`, `git_diff`, `git_show`, `git_log`, `git_branch`, `git_add`, `git_checkout`,
  `git_commit`, `git_push`

Every tool carries read-only / destructive annotations, output is capped and always flagged when
truncated, and commands run under a timeout that kills their whole process group.

File tools are confined to a **root** (default: the directory the client started in). Change it with
`--root DIR` or `MCP_SWITCHBOARD_HARNESS_ROOT`; `--root /` lifts it. Cap output with `--max-output N`
or `MCP_SWITCHBOARD_HARNESS_MAX_OUTPUT`.

## Use

`mcp-switchboard-client` runs it by default as a server named `harness`; nothing to
configure (`--no-harness` turns it off). To run it standalone or pin your own
entry, add it to the `mcp.json` next to the client:

```json
{
  "mcpServers": {
    "harness": { "command": "uvx", "args": ["mcp-switchboard-server-harness"] }
  }
}
```

or run it directly: `mcp-switchboard-server-harness --name harness`.

## Security

The root only confines the *file* tools; `run_command`, `run_python` and `process_start`
still run arbitrary commands as the user that started the client. It is a guard against mistakes
and path tricks, not a sandbox. Keep the hub's private listener unexposed, or
set `MCP_SWITCHBOARD_PRIVATE_TOKEN`.
