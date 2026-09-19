# mcp-switchboard specification

Behavioural spec for the whole system. The wire format is specified separately in
[docs/PROTOCOL.md](docs/PROTOCOL.md); this document covers everything around it.

## 1. Purpose and components

Expose stdio MCP servers running on machines behind NAT to MCP consumers, with no
inbound port on those machines.

| Component | Package | Role |
|---|---|---|
| Client | `mcp-switchboard-client` | Spawns local stdio MCP servers from `mcp.json` (plus the built-in harness); multiplexes them over one outbound WebSocket. Speaks no MCP itself. |
| Hub | `mcp-switchboard-hub` | Terminates tunnels, runs one MCP client session per tunnelled server, aggregates tools, serves them over Streamable HTTP, console, API and metrics. |
| Harness server | `mcp-switchboard-server-harness` | First-party stdio MCP server (files, search, git, shell). Added by the client by default; tunnelled like any other server. |

## 2. Client

- **C1. Config.** Reads `mcp.json` (default: current directory). Two entry shapes:
  `{command, args?, env?, cwd?}`, or, if the entry has a top-level `source` key, a
  FastMCP config launched via `uvx fastmcp run`. A bare FastMCP file (top-level `source`) is a
  single server named after its `name` or file stem.
- **C2.** `mcpServers` must be a non-empty object; otherwise the client exits 1 with a message.
  If the *default* `./mcp.json` does not exist and the built-in harness is enabled (C9), the client
  runs with the harness alone and logs that it did. A config named with `--config` /
  `MCP_SWITCHBOARD_CONFIG` must exist, and with `--no-harness` a config is always required
  (otherwise there would be nothing to tunnel).
- **C3. Projects.** An entry may set `project` (string). A top-level `project` is the default for
  entries without one. Blank strings count as absent. A non-string is an error. `project` is not
  forwarded to FastMCP configs.
- **C4. Names.** Server and project names must satisfy `protocol.validate_name` (in particular, no
  `__`); violations exit 1 before connecting.
- **C5. Settings.** From CLI flags, `MCP_SWITCHBOARD_*` env vars, or `.env`. A value starting with
  `/` naming an existing regular file is replaced by the file's contents. `LABEL` defaults to the hostname.
- **C6. Connection.** One outbound `wss://<hub>/tunnel/v1` with `Authorization: Bearer <token>`.
  For `wss` the TLS context trusts certifi's CA bundle.
- **C7. Lifecycle.** Sends `hello` (including each server's `project` when set), then supervises
  each server, forwarding stdout lines as `mcp` frames and `hub → client` frames to stdin, and sending
  `server_state` on every transition. `restart` stops and respawns one server.
- **C8. Reconnect.** Exponential backoff 1s → 60s. After a reconnect every local server is restarted.
  A fatal refusal (bad token, protocol mismatch) is not retried.
- **C9. Built-in harness.** Unless disabled, the client adds a server named `harness` running
  `python -m mcp_switchboard_server_harness` with its own interpreter, in the client's working directory (which becomes the harness root, see S2) (the harness is a dependency of the
  client, so no install or fetch happens at startup). Disable with `--no-harness` or
  `MCP_SWITCHBOARD_HARNESS=false`. An `mcp.json` entry named `harness` replaces the built-in one.
  The harness needs a valid `mcp.json` like any other run; it is added to, not instead of, its servers.
- **C10. Shutdown.** SIGINT/SIGTERM stops the connection and terminates children; exit code 0.
  A tunnel error exits 1.

## 3. Hub

- **H1. Listeners.** Tunnel listener (default `127.0.0.1:8097`), always token-protected. Private
  listener (default `127.0.0.1:8099`) serves `/mcp`, `/`, `/api/*`, `/metrics`; unauthenticated
  unless `MCP_SWITCHBOARD_PRIVATE_TOKEN` is set.
- **H2. Hello validation.** Rejects with an `error` frame and closes on: unsupported protocol
  version, server or project names containing `__`, or two servers with the same name in one hello.
- **H3. Sessions.** Per tunnelled server, one MCP client session (`initialize`, `tools/list`).
  Tools appear in the catalog when the session is up and disappear when the server stops.
- **H4. Restarts.** A restart (from the console, or `server_state` cycling) retires the old session
  and waits (bounded by `STOP_WAIT_TIMEOUT`, 5 s) for it to finish before starting the next, so rapid
  restarts never lose tools or race over a channel. A stop aborts an in-flight `initialize`.
- **H5. Tool naming.** Exposed name is `{label}__[{project}__]{server}__{tool}`. Each tool is also
  tagged in `title` (`tool · server @ label`), at the front of `description`
  (`[label · server] …`), and in `_meta` (`host`, `project`, `server`, `connectionId`, `upstreamName`).
- **H6. Scopes.** The same tools are served at:

  | Path | Scope | Names |
  |---|---|---|
  | `/mcp` | all | `label__[project__]server__tool` |
  | `/mcp/host/{label}` | host | `[project__]server__tool` |
  | `/mcp/host/{label}/project/{project}` | project | `server__tool` |
  | `/mcp/host/{label}/server/{server}` | server | `tool` |
  | `/mcp/host/{label}/project/{project}/server/{server}` | project + server | `tool` |

  Calls resolve against the same scope they were listed in.
- **H7. Endpoints panel.** `GET /api/endpoints` returns, for the console: the local base URL
  (`MCP_SWITCHBOARD_LOCAL_BASE_URL`, default `http://127.0.0.1:<private port>`), one row per
  reachable scope for the currently connected hosts/projects/servers with an example tool name, and
  the client-install command when `MCP_SWITCHBOARD_PUBLIC_URL` is set (else none). With nothing
  connected, the rows are `/mcp` and a `/mcp/host/<host>` placeholder.
- **H8. Call log.** Every tool call, from the console or an MCP consumer, is written to SQLite with
  arguments and result, pruned by `RETENTION_DAYS` and `MAX_ROWS`.
- **H9. Observability.** Prometheus `/metrics` (`mcpsb_*`); optional batched Loki push whose bounded
  queue drops (and counts drops) rather than blocking a call.

## 4. Harness server

### 4.1 General behaviour

- **S1. Identity.** Stdio MCP server; name from `--name` (default `harness`). Through the hub its tools
  are named `{label}__harness__{tool}` at `/mcp`, and plain `{tool}` at `/mcp/host/{label}/server/harness`.
- **S2. Root and confinement.** Every file path (absolute or relative) is resolved with `realpath` and
  must lie inside the *root*: `--root` / `MCP_SWITCHBOARD_HARNESS_ROOT`, default the working directory the
  client was started in (`--root /` lifts the restriction). Relative paths resolve against the root;
  `..` escapes and symlinks pointing outside are refused (`PermissionError`). A symlink *itself* can be
  deleted or moved; only following it is checked. The root itself cannot be deleted. Paths returned to
  the caller are relative to the root (`.` for the root).
- **S3. Confinement is a guard, not a sandbox.** `run_command`, `run_python`, `process_start` (and
  `git_*`, through hooks and configuration) run arbitrary code as the launching user and are not
  confined; only their `cwd` is. See section 5.
- **S4. Errors.** Expected failures are reported one of two ways, by tool family:
  - The *typed file tools* (4.2) report them **in the result** (`success`, `deleted`, `moved` = `false`
    plus a `message`) for outcomes like a missing path or refused overwrite, and raise for faults such
    as a path outside the root or reading a missing file.
  - Everything else raises. Errors a tool raises on purpose (`OSError`, `ValueError`, `RuntimeError`)
    are delivered to the caller as a tool error (`isError: true`) **with their message**, e.g.
    `PermissionError: 'x' is outside the allowed root /work`; anything unexpected yields only a generic
    "Error executing tool" and is logged server-side.
- **S5. Output limit.** Text returned per stream (`run_command`, `run_python`, git output, background
  process reads) or per `file_read` is capped at `--max-output` /
  `MCP_SWITCHBOARD_HARNESS_MAX_OUTPUT` characters (default 100000). Truncation is always flagged
  (`truncated`, or a `[output truncated]` suffix on git output), never silent.
- **S6. Timeouts.** Commands are killed after 120 s (`timeout` argument on `run_command` and
  `run_python`, clamped to 3600 s) and reported as a tool error. The whole process group is killed, so
  children a command backgrounded (`sleep 99 &`) do not outlive it. A missing binary (`rg`, `git`, `bash`)
  is a tool error naming it.
- **S7. Concurrency.** Subprocess tools are `async` and file tools run in worker threads, so a slow call
  never blocks other calls on the same server.
- **S8. Atomic writes.** `file_write` (overwrite) and `edit_file` write a temp file in the target's
  directory and `os.replace` it, preserving an existing file's mode; a crash never leaves a truncated
  file. `append` is a plain append.
- **S9. Annotations.** Every tool advertises MCP annotations so clients can auto-approve reads and
  confirm risky calls (4.5).

### 4.2 Typed file and shell tools

All outputs are advertised as JSON output schemas (`structuredContent`).

**`run_command`**: run a shell command via `bash -c`.

| Field | Type | Notes |
|---|---|---|
| in `command` | string | required |
| in `cwd` | string, optional | working directory, confined to the root; default: the root |
| in `env` | object of string to string, optional | overrides on top of the inherited environment |
| in `timeout` | number, optional | seconds; default 120, max 3600 |
| out `stdout`, `stderr` | string | each capped at the output limit |
| out `exit_code` | integer | a non-zero exit is a normal result, not an error |
| out `return_value` | object or null | reserved; currently always `null` |
| out `truncated` | boolean | stdout or stderr was cut |
| out `stdout_total`, `stderr_total` | integer | full lengths before truncation |

**`file_read`**: read one file, in pages if large.

| Field | Type | Notes |
|---|---|---|
| in `path` | string | required |
| in `binary` | boolean, default `false` | |
| in `offset` | integer, default 0 | start position: bytes when `binary`, else characters |
| in `limit` | integer, optional | chunk size in the same unit; never exceeds the output limit |
| out `content` | string (base64), optional | set when `binary` is true |
| out `raw_text` | string, optional | UTF-8 text; set when `binary` is false. A file that is not valid UTF-8 needs `binary: true` |
| out `size` | integer | total file size in bytes |
| out `offset` | integer | where this chunk started |
| out `truncated` | boolean | more remains; call again with a larger `offset` |

**`file_write`**: write one file, creating missing parent directories.

| Field | Type | Notes |
|---|---|---|
| in `path` | string | required |
| in `content` | string | required. With `binary: true` it is base64 and is decoded to bytes; otherwise written as UTF-8 text |
| in `append` | boolean, default `false` | append instead of overwrite |
| in `binary` | boolean, default `false` | |
| out `success` | boolean | `false` for invalid base64 or an OS error |
| out `message` | string | e.g. `wrote 5 bytes to x` |

**`file_delete`**: delete a file, symlink or directory.

| Field | Type | Notes |
|---|---|---|
| in `path` | string | required |
| in `recursive` | boolean, default `true` | when `false`, a directory is removed only if empty |
| out `deleted` | boolean | `false` if the path does not exist, a non-recursive delete hit a non-empty directory, or the path is the root |
| out `message` | string | |

**`dir_list`**: list a directory.

| Field | Type | Notes |
|---|---|---|
| in `path` | string | required |
| in `recursive` | boolean, default `false` | symlinks are not followed |
| out `entries[]` | object | `name`, `path` (root-relative), `size` (bytes), `is_dir`, `mtime` (ISO 8601 UTC) |

Order is stable: sorted by name; when recursive, each directory's subdirectories then its files.
Raises if `path` is not a directory.

**`file_move`**: move or rename.

| Field | Type | Notes |
|---|---|---|
| in `src`, `dst` | string | required |
| out `moved` | boolean | `false` if `src` is missing or `dst` already exists (never overwrites) |
| out `message` | string | missing parent directories of `dst` are created |

### 4.3 Search and edit tools

| Tool | Arguments | Returns |
|---|---|---|
| `tree_of_files` | `root="."` | nested dict: subdirectories by name, files under `"__files__"`; skips `.git`, `node_modules`, `__pycache__`, `.venv` |
| `find_files` | `pattern`, `root="."`, `limit=1000` | sorted root-relative paths of files whose path or name matches a glob such as `**/*.py`. Inside a git repository `.gitignore` is honoured (tracked plus untracked-not-ignored files); otherwise the directories above are skipped |
| `ripgrep` | `query`, `path="."`, `glob=None`, `ignore_case=false`, `context=0`, `max_results=200` | `{matches: [{file, line_no, text, is_match}], truncated}`; `context` adds up to 20 surrounding lines per match (`is_match: false`); `truncated` when `max_results` was hit; requires `rg` |
| `read_lines` | `path`, `start=1`, `end=None` | lines `start..end`, 1-based inclusive |
| `edit_file` | `path`, then either `new_content`, or `old_str` + `new_str` (+ `replace_all=false`) | confirmation. `old_str` must occur **exactly once** unless `replace_all`; zero matches, several matches without `replace_all`, an empty `old_str`, or a missing file is an error, so the wrong spot is never edited silently |
| `run_python` | `code`, `timeout=None` | `{returncode, stdout, stderr}` from a fresh interpreter run in the root; output capped |

`run_bash` and `list_dir` were removed in favour of `run_command` and `dir_list` (fewer, sharper tools
choose better). `tree_of_files` stays because its nested shape is different.

### 4.4 Git tools

All run in the root. Ref and revision arguments starting with `-` are rejected (no option injection),
and path arguments are confined to the root. Output is capped (S5).

| Tool | Arguments | Returns |
|---|---|---|
| `git_status` | none | short status |
| `git_diff` | `staged=false`, `paths=None`, `rev=None` | unified diff: unstaged by default, `staged` for what would be committed, `rev` for working tree vs a revision |
| `git_show` | `rev="HEAD"`, `stat_only=false` | a commit's message and diff, or just its file summary |
| `git_log` | `limit=10` | `hash subject` lines |
| `git_branch` | none | local branches, current starred |
| `git_add` | `files: [str]` | git output |
| `git_checkout` | `ref`, `create=false` | switch (or create and switch); git refuses if uncommitted changes would be lost |
| `git_commit` | `message` | git output |
| `git_push` | none | git output |

### 4.5 Background processes

For work that outlives one call (dev servers, watchers, slow builds).

| Tool | Arguments | Returns |
|---|---|---|
| `process_start` | `command`, `cwd=None`, `env=None` | `{id, pid}`; at most 16 at once, all killed when the harness exits |
| `process_read` | `id`, `wait=0` | `{id, stdout, stderr, running, exit_code, dropped}`: only output produced since the previous read; `wait` (max 30 s) pauses until exit or timeout first; `dropped` means older output overflowed the 1,000,000-character buffer |
| `process_kill` | `id` | `{id, killed, message}`; kills the whole process group; unread output stays readable |

Finished, fully-read processes are forgotten when a new one starts. An unknown `id` is an error.

### 4.6 Tool annotations

| Tools | `readOnlyHint` | `destructiveHint` | `idempotentHint` | `openWorldHint` |
|---|---|---|---|---|
| `file_read`, `dir_list`, `tree_of_files`, `find_files`, `ripgrep`, `read_lines`, `git_status`, `git_log`, `git_diff`, `git_show`, `git_branch` | true | | true | false |
| `process_read` (consumes buffered output) | true | | false | false |
| `file_write`, `edit_file`, `git_checkout` | false | true | false | false |
| `file_delete`, `process_kill` | false | true | true | false |
| `file_move`, `git_commit` | false | false | false | false |
| `git_add` | false | false | true | false |
| `run_command`, `run_python`, `process_start` | false | true | false | true |
| `git_push` | false | true | false | true |

## 5. Security model

The tunnel token is the only credential on the public listener. The private listener relies on
not being reachable, optionally hardened with a private token. The install command shown in the
console contains the tunnel token, which is why it renders only on the private listener.

Anyone who can call a hub's `/mcp` endpoints can use the harness on every connected machine. The
harness's root confinement stops file tools escaping by mistake or by path trick, and annotations let
well-behaved clients ask before destructive calls, but neither is a security boundary:
`run_command` can do whatever the client's user can. Treat access to the private listener as shell
access to every machine running a default client, and use `--no-harness` where that is not acceptable.

## 6. Non-goals

Non-stdio upstream transports; inbound connections to clients; sandboxing tool execution;
application-level heartbeats (WebSocket ping/pong only — see PROTOCOL.md).

## 7. Testing

Unit tests per package (`client/tests`, `hub/tests`, `servers/harness/tests`); an end-to-end test in
`tests/` runs a real client process, real MCP servers and a real hub, and calls tools through the
console API and as an MCP consumer; a NixOS VM test in `nix/`.
