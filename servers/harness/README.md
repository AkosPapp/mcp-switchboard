# mcp-switchboard-server-harness

A first-party stdio MCP server that gives a coding agent the basics on a
remote machine: filesystem, search, git and shell tools. It is a normal MCP
server, so [mcp-switchboard-client](../../client) tunnels it like any other.

Tools (26; full schemas and behaviour in [spec.md](../../spec.md#4-harness-server)). The core
set deliberately mirrors the tool suite a Claude Code / opencode-style agent already knows:
`bash`, `read`, `write`, `edit`, `glob`, `grep` — plus the extras listed below.

- **Files:** `read` (line-numbered `N: ` output, paged with `offset`/`limit`, lists directories),
  `write`, `edit` (exact-string replace, returns a diff; `dry_run`, `replace_all`),
  `apply_patch` (unified diff, multi-file, all-or-nothing, `dry_run`),
  `file_delete` (directories need `recursive=true` unless empty), `file_move`
- **Search:** `glob` (honours `.gitignore`, newest first), `grep` (regex over contents)
- **Run:** `bash` (`mode` = `full`/`head`/`tail`/`grep`), `run_tests` (auto-detects
  pytest / go / npm / cargo, returns structured failures), `output_read` / `output_grep` (page through output that was
  cut; commands return an `output_id`), and `process_start` / `process_read` / `process_kill` for
  long-running work. Long *waits* are never blocking calls: `wait_for_start` + `wait_for_poll`
  watch a shell predicate harness-side (satisfied when it exits non-zero) so no hub per-call
  deadline can kill them; a blocked command that hits a timeout returns its partial output
  (`timed_out`, `exit_code` 124), never an error
- When driving cluster or fleet CLIs through `bash`, prefer their machine-readable flags
  (`--json` / `--parsable` / `-h`) over default table output — Slurm table columns are
  misalignment-prone and mis-read silently
- **Git:** `git_status`, `git_diff`, `git_show`, `git_log`, `git_branch`, `git_add`, `git_checkout`,
  `git_commit`, `git_push` — `git_push` declares itself **irreversible** (tool `_meta`), so the hub
  gates it even under `approval=never`; it takes no arguments, so there is no force-flag path, and
  a direct push to `main`/`master` is flagged in the output

Every tool carries read-only / destructive annotations, output is capped and always flagged when
truncated, and commands run under a timeout that kills their whole process group.

File tools are confined to a **root** (default: the directory the client started in; change it with
`--root DIR` or `MCP_SWITCHBOARD_HARNESS_ROOT`; `--root /` lifts it), plus two always-writable
neighbours: the **scratch** directory (`<root>/.harness/scratch` by default, `--scratch DIR` or
`MCP_SWITCHBOARD_HARNESS_SCRATCH`; created on first use, disposable) and the **system temp**
directory (`$TMPDIR`, else `/tmp`). Cap output with `--max-output N` or
`MCP_SWITCHBOARD_HARNESS_MAX_OUTPUT`.

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

The root/scratch/temp set only confines the *file* tools; `run_command`, `run_python` and
`process_start` still run arbitrary commands as the user that started the client — anything that
user can read (`~/.git-credentials`, ssh keys) is reachable, and the file-tool confines
deliberately let through exactly what those can write, so the two paths never disagree. It is a
guard against mistakes and path surprises, not a sandbox; a real one (mount namespace / separate
uid) is a hub/OS feature, see [docs/SANDBOX_PROPOSAL.md](../../docs/SANDBOX_PROPOSAL.md). Keep the
hub's private listener unexposed, or set `MCP_SWITCHBOARD_PRIVATE_TOKEN`.
