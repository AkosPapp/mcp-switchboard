# Tunnel protocol v1

The wire format between `mcp-switchboard-client` (on a machine with local MCP servers) and
`mcp-switchboard-hub`. This file is the single source of truth; `protocol.py` in each package is a
literal transcription of it and the two copies are kept byte-identical by a test.

## Shape

One **outbound** WebSocket per client process, carrying every MCP server that client runs. All frames
are JSON text frames, each an object with a `type` field.

The client is a **dumb pipe**: it speaks no MCP. It spawns local stdio MCP servers and shuttles their
stdin/stdout across the tunnel tagged with a server name. Every MCP concern — `initialize`,
`tools/list`, id correlation — lives in the hub, which runs one MCP client session per server channel.

## Connecting

The client opens `wss://<hub>/tunnel/v1` with:

```
Authorization: Bearer <tunnel token>
```

An invalid or missing token is rejected during the handshake with HTTP 401. The token is always
required: this listener is the one intended to face the public internet.

## Frames

### `hello` — client → hub, first frame

```json
{
  "type": "hello",
  "protocol": 1,
  "client": {
    "name": "mcp-switchboard-client",
    "version": "0.3.0",
    "instance": "3f8a…",
    "label": "legion5",
    "environment": {
      "kinds": ["devcontainer", "direnv"],
      "project": "myproject",
      "workspace": "/workspaces/myproject",
      "details": {"devcontainerName": "myproject", "direnvDir": "/workspaces/myproject"}
    }
  },
  "servers": [
    {"name": "git", "command": "uvx mcp-server-git --repository ."},
    {"name": "lsp", "command": "nixd", "project": "nix"}
  ]
}
```

`instance` is a per-process UUID. `label` identifies the machine, defaults to its hostname, and is what
the hub uses to tag tools by host. `command` is descriptive only — the hub never executes it.

`client.instructions` is optional and additive (the protocol version stays 1; older clients omit it): a
list of `{"path", "content"}` objects carrying the repository's own agent instruction files
(`AGENTS.md`, `CLAUDE.md`, `.cursorrules`, `.github/copilot-instructions.md`), read from the client's
working directory tree at connect time, root-first. The hub injects them verbatim into the system
prompt of agents that can see that client's tools, each labelled `label:path`, and re-reads them via
`context_update` below. It is untrusted input bounded hub-side — at most 8 files, 32 KiB per file and
64 KiB total, paths collapsed to one line, a malformed list dropped without refusing the hello (same
tolerance as `environment`) — and it is a mirror: the hub never opens the paths it is shown.

`client.environment_brief` is optional and additive (the protocol version stays 1; older clients omit
it): a short (~30 lines) plain-text description of the client host — user/uid/groups, hostname and OS,
sudo status (verified only by a non-interactive check; otherwise `unknown`), cwd, git root/branch/
dirty+untracked counts and the `origin` remote with credentials redacted, the scratch and temp paths the
file tools may write, DNS resolvers and local addresses (**reachability is never probed**), and
present/absent for a curated tool list. The hub injects it, verbatim and stamped with its age, into the
system prompt of agents that can see that client's tools; a brief older than 5 minutes is marked stale
rather than silently trusted, alongside a hub section (effective `CALL_TIMEOUT`, push-notification
state) that only the hub knows. Untrusted input capped at 8 KiB hub-side; a malformed value is dropped
without refusing the hello. Clients refresh it via `context_update` below.

`client.environment` is optional and additive (the protocol version stays 1; older clients omit it). It
tells the hub where the client runs so a console can show it: `kinds` is a list of open strings (today
`devcontainer`, `container`, `direnv`, `nix-shell`, `venv`; may be empty), `project` the project name
(devcontainer.json `name`, else the git root's directory name, else the cwd basename, or an explicit
`MCP_SWITCHBOARD_PROJECT_NAME`), `workspace` the client's absolute cwd, and `details` a small map of
non-secret strings (paths and names only, never environment variable values). The hub decodes it
tolerantly: unknown kinds are kept, a malformed `environment` is dropped without rejecting the hello,
and sizes are capped (kinds at 8, details at 16 entries, every string at 256 characters).
`docs/protocol.json` lists it under `hello.clientOptional`.

`project` (per server) is optional. It groups a server under a named project on that machine, which the hub uses in
tool names (`{label}__{project}__{server}__{tool}`) and in the project-scoped `/mcp` endpoints. Omit it
for a server that belongs to no project.

Server names and project names must not contain `__`, since the hub joins names with that separator and
splits on it. The hub rejects the connection with an `error` frame if they do.

### `hello_ack` — hub → client

```json
{"type": "hello_ack", "connectionId": "c1a2…", "hub": {"name": "mcp-switchboard-hub", "version": "0.1.0"}}
```

### `mcp` — both directions

The data plane. `payload` is a raw MCP JSON-RPC message, passed through untouched.

```json
{"type": "mcp", "server": "git", "payload": {"jsonrpc": "2.0", "id": 1, "method": "tools/list"}}
```

Hub → client frames are written to that server's stdin; client → hub frames are one line read from its
stdout. Because each server has its own channel with its own MCP session, JSON-RPC ids never collide
and the hub needs no id rewriting.

### `server_state` — client → hub

```json
{"type": "server_state", "server": "git", "state": "running", "exitCode": null, "error": null}
```

States: `starting`, `running`, `exited`, `failed`. Sent on every transition so the console can show a
crashed server with its error instead of silently listing no tools.

### `context_update` — client → hub

```json
{"type": "context_update", "instructions": [{"path": "AGENTS.md", "content": "# new rules\n"}], "environment_brief": "uid=1000 user=me\n..."}
```

Carries an updated copy of either context item; both fields are independently optional and each
replaces its hub-side copy **wholesale** (an empty list or string clears it, an absent field is left
alone). Clients send it whenever the collected instruction files change (edits to `AGENTS.md` reach the
agent without a reconnect) and whenever the environment brief changes (each push also resets that
carry's staleness clock). Same caps and same tolerance rules as the corresponding `hello` fields; a hub
that does not know this frame ignores it, so the older snapshot simply stays in use.

### `skills_update` — client → hub

```json
{"type": "skills_update", "skills": [{"name": "pdf", "path": "pdf/SKILL.md",
  "source": "global:.claude/skills", "description": "PDF work", "content": "---\nname: pdf\n---\nDo the thing.\n"}]}
```

The live replacement for `hello`'s `client.skills`: every SKILL.md skill the client can see on its
host (opencode discovery locations — `~/.claude/skills`, `~/.config/opencode/skills`, `~/.agents/skills`
and the project-level `.claude/.opencode/.agents/skills` directories up to the git root). The list is
complete — an empty list means the host has none — and the client sends it whenever the scan changes.
`name` is the directory name and must match the skill grammar (`^[a-z0-9]+(-[a-z0-9]+)*$`);
`path` and `source` are display labels; `content` is the whole SKILL.md text. The hub caps the set
(200 skills, 64 KiB each, 256 KiB total), stores valid entries under
`skills/hosts/<label>/<name>/SKILL.md` in its data directory as read-only mirrors, and a hub that
does not know this frame ignores it. Untrusted input: counts and sizes are bounded hub-side, junk is
dropped rather than refused (same discipline as `instructions`).

## Tool `_meta` extensions

Servers may carry namespaced hints in a tool's `_meta` (an MCP standard field). Switchboard honours
exactly one: `switchboard.irreversible: true` — the server asserts that this tool's effect cannot be
undone from that machine (a `git_push`; the harness also sends `switchboard.irreversibleReason`, shown
to the approver). The hub gates such tools even under `approval: "never"` (spec W1a), passes the fact
to downstream `/mcp` consumers as tool meta `irreversible`, and serves it in the API as
`annotations.irreversibleHint`. Foreign namespaces in `_meta` are never interpreted, and a claim from
a server is a claim — like all annotations it is advisory (spec W4), never a security control.

### `restart` — hub → client

```json
{"type": "restart", "server": "git"}
```

Stop and respawn that server, then report the resulting `server_state`. Triggered from the console.

### `error` — hub → client

```json
{"type": "error", "message": "server names must not contain '__'", "server": null}
```

Sent before closing when the connection is unusable (bad protocol version, illegal server names).

## Liveness

WebSocket ping/pong only, native to the `websockets` library, with a 20s interval and 10s timeout.

**There is deliberately no application-level heartbeat frame.** The protocol this replaces had both
sides answer `heartbeat` with `heartbeat`, which is an infinite loop — measured at ~80 messages/second
per session against a live gateway. Any future heartbeat frame must be asymmetric (`ping` answered by
`pong`, never by another `ping`).

## Reconnection

The client reconnects with exponential backoff (1s doubling to a 60s cap). On a successful reconnect it
**restarts every local MCP server**, because the hub establishes a fresh MCP session and a server
process that has already completed `initialize` would reject a second one.

## Versioning

`protocol` in `hello` is an integer. The hub rejects a version it does not implement with an `error`
frame and closes; it does not attempt to negotiate down.
