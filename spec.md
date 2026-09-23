# mcp-switchboard specification

Behavioural and technical spec for the whole system. The tunnel wire format is specified
separately in [docs/PROTOCOL.md](docs/PROTOCOL.md); this document covers everything around it.

## 0. Status of this revision

Four changes land together, because each one forces the others:

1. **The hub is rewritten in Go.** The Python hub (FastAPI + `mcp` + aiosqlite) is replaced by a
   single static binary using the official [`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk).
   Motivation is per-connection cost: the hub holds one live MCP client session per tunnelled
   server *and* one per consumer session, and is about to hold one long-running agent loop per
   active run. Goroutines and a static binary suit that better than an asyncio process whose
   deployment drags a resolved virtualenv behind it.
2. **An agent orchestrator moves into the hub.** The hub gains the ability to *run* agents, not
   just serve tools to them: an LLM loop, per-agent tool permissions, parent→child spawning, and
   inter-agent messaging, all exposed as MCP tools so agents can drive it themselves.
3. **The console is rebuilt** as a React + Vite + TypeScript + Tailwind SPA, embedded into the Go
   binary with `embed.FS`, adding a **Chat** view (branching conversations, OpenWebUI-style) and a
   **Graph** view (live agent tree, communication edges, MCP permissions), and designed
   mobile-first because both views are things you want to check from a phone.
4. **Agent profiles and per-chat clients** (§5.2a). A chat is no longer opened against a
   hand-assembled agent: it is created from a reusable **profile** (system prompt, default model,
   hub-tool capabilities, approval mode) plus **exactly one MCP client** (a machine that dials the
   hub), and the hub builds the chat's working agent with grants for that client alone. The console
   gains an **Agents** panel for profiles, a hub-tools section, and per-chat tool inspection
   (§7.2, §8). This is the first migration after the schema froze (`002_profiles`, §6.2), and its
   REST contract is [docs/PROFILES_API.md](docs/PROFILES_API.md).

5. **Chats are the unit** (§5.1, [docs/CHAT_MODEL_API.md](docs/CHAT_MODEL_API.md)). The user no
   longer sees agents: a chat has a title, a system prompt and one client (or none), and chats talk
   to each other by *injecting messages into each other's own chats* instead of holding separate
   peer conversations. The `agents` row stays underneath as the chat's internal execution record,
   1:1 with it for everything created from now on. Migration `005_chats_as_unit` (D13) adds
   `chats.parent_chat_id` and `messages.sender`; the LLM-facing tools are renamed
   `switchboard.chat.*` (§5.5).

**Unchanged:** the client (`mcp-switchboard-client`), the harness server
(`mcp-switchboard-server-harness`), and the tunnel protocol v1 on the wire. Both remain Python.
The tunnel carries JSON over a WebSocket and is language-agnostic; a Python client talks to a Go
hub with no protocol change.

### 0.1 Naming

"Harness" in this document means **only** the first-party MCP server of §9 (files, search, git,
shell). The agent machinery is the **orchestrator**, and a running conversation is a **run**.
Earlier drafts called the agent runtime an "agent harness"; that name is retired to avoid the
collision.

### 0.2 Protocol source of truth after the rewrite

Today `protocol.py` and `envconf.py` are kept byte-identical between hub and client by
`hub/tests/test_protocol_sync.py`. A Go hub cannot participate in that check, so:

- **P1.** `docs/PROTOCOL.md` becomes the sole normative source. Frame names, states and the
  separator move into `docs/protocol.json` — a small machine-readable manifest listing the
  constants and each frame's required fields.
- **P2.** `client/src/mcp_switchboard_client/protocol.py` and `hub/internal/protocol/protocol.go`
  are both generated-or-checked against `docs/protocol.json` by a test in each language. Drift in
  either direction fails CI.
- **P3.** `envconf.py` stays Python-only (the client keeps it); the Go hub reimplements the same
  two rules — `MCP_SWITCHBOARD_*` prefix, and *any value beginning with `/` that resolves to an
  existing **regular file** is replaced by that file's contents*. A shared table-driven fixture
  (`docs/envconf-cases.json`) is executed by both test suites so the substitution semantics
  cannot diverge.
- **P4.** A **cross-language conformance test** replaces byte-identity: the end-to-end suite runs
  the real Python client against the real Go hub binary and asserts on both the observed frames
  and the resulting MCP tool catalog.

## 1. Purpose and components

Expose stdio MCP servers running on machines behind NAT to MCP consumers with no inbound port on
those machines — and, on top of that same catalog, run agents that use those tools.

| Component | Package / module | Language | Role |
|---|---|---|---|
| Client | `mcp-switchboard-client` | Python | Spawns local stdio MCP servers from `mcp.json` (plus the built-in harness); multiplexes them over one outbound WebSocket. Speaks no MCP itself. |
| Hub | `mcp-switchboard-hub` | Go | Terminates tunnels, runs one MCP client session per tunnelled server, aggregates tools, serves them over Streamable HTTP, and hosts the orchestrator, console, API and metrics. |
| Harness server | `mcp-switchboard-server-harness` | Python | First-party stdio MCP server (files, search, git, shell, background processes). Added by the client by default; tunnelled like any other server. |
| Console | `hub/web/` | TypeScript | Single-page app embedded in the hub binary. Connections (with the hub's own tools), Calls, Endpoints, **Chat**, **Graph**, **Agents** (profiles). |

## 2. Identity and naming

This section is normative for everything that follows, because most of the orchestrator's
correctness rests on getting identity right.

- **I1. A server's stable identity is the triple `(label, project, server)`**, where `project` may
  be empty. It is stable across client restarts and hub restarts, because all three come from the
  client's configuration. Written `label/project/server` in prose and carried as a `ServerRef`
  struct in code.
- **I2. `connectionId` is not an identity.** It is a per-tunnel-session UUID, regenerated on every
  reconnect. It may appear in live views, logs and `_meta`, and it is fine as a *cache key* for an
  open session, but **nothing durable may be keyed on it** — not permissions, not chat history,
  not metrics.

  > This is the single most important correction to the earlier agent draft, which keyed
  > `mcp_server_permissions` on a `server_id`. Every client reconnect (§C8, and every hub restart)
  > would have silently revoked every agent's permissions.

- **I3. Live state vs. durable config.** The registry (what is connected *right now*) is in-memory
  and rebuilt from scratch on every hub start. Permissions, agents, chats and edges are durable
  rows in the store. A `ServerRef` in the store that currently matches no connection is **not an
  error**: it is a permission for a machine that is offline. The Graph view renders it greyed out.
- **I4. Exposed tool names** are composed as `{label}__[{project}__]{server}__{tool}`. Neither a
  label, a project nor a server name may contain `__` (`protocol.validate_name`). An upstream tool
  name must match `^[A-Za-z0-9._-]+$` and the composed name must be ≤128 characters; a tool that
  fails either is dropped from the catalog with a warning rather than breaking the whole
  `tools/list`.
- **I5. Built-in orchestrator tools are named `switchboard.<group>.<verb>`** — a dot namespace,
  which is legal under I4's charset and cannot collide with a tunnelled tool, since those always
  contain `__` and never a `.` in the composed prefix.
- **I6. Agents, chats, messages and runs are UUIDv7** (time-ordered, so they sort by creation and
  index well in SQLite). Stored as 32-char lowercase hex without dashes, matching the existing
  call-id convention.
- **I7. An MCP client is only a process that dials the hub**, identified by its `label` (the
  `label` of a connection, I1). It is neither an MCP *server* (a stdio process the client spawns
  and tunnels, the second and third components of I1's triple) nor a *hub tool* (the built-in
  `switchboard.*` tools of §5.5). The word "client" in the console and in `clientLabel` always
  means this. A client offers zero or more servers; "this chat may use client `laptop`" is the
  grant `(laptop, *, *)`.
- **I8. A profile is a template, not an agent.** It is the reusable configuration a chat's working
  agent is instantiated from (§5.2a). It has no parent, no tree position, no runs, no chats and —
  deliberately — **no grants**: which client a conversation may use is chosen when the chat is
  created, not when the profile is written.

## 3. Client

Unchanged by this revision. Retained verbatim for completeness.

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
- **C7. Lifecycle.** Sends `hello` (including each server's `project` when set, and the optional `client.environment`), then supervises
  each server, forwarding stdout lines as `mcp` frames and `hub → client` frames to stdin, and sending
  `server_state` on every transition. `restart` stops and respawns one server.
- **C8. Reconnect.** Exponential backoff 1s → 60s. After a reconnect every local server is restarted.
  A fatal refusal (bad token, protocol mismatch) is not retried.
- **C9. Built-in harness.** Unless disabled, the client adds a server named `harness` running
  `python -m mcp_switchboard_server_harness` with its own interpreter, in the client's working
  directory (which becomes the harness root, see S2) — the harness is a dependency of the client,
  so no install or fetch happens at startup. Disable with `--no-harness` or
  `MCP_SWITCHBOARD_HARNESS=false`. An `mcp.json` entry named `harness` replaces the built-in one.
- **C10. Shutdown.** SIGINT/SIGTERM stops the connection and terminates children; exit code 0.
  A tunnel error exits 1.
- **C11. Environment.** Detects, once at startup and without raising, where it runs (`kinds`:
  devcontainer, container, direnv, nix-shell, venv; plus `project`, `workspace`, small non-secret
  `details`) and sends it as the optional `hello.client.environment`. `MCP_SWITCHBOARD_PROJECT_NAME` /
  `--project-name` overrides the project name. One INFO line summarises it.

## 4. Hub

### 4.1 Listeners

- **H1.** Two listeners, for the same reason as before. The **tunnel listener** (default
  `127.0.0.1:8097`) serves only `/tunnel/v1` and `/health` and is always token-protected; it is the
  only one meant to be publicly reachable. The **private listener** (default `127.0.0.1:8099`)
  serves `/mcp*`, `/`, `/api/*`, `/metrics`, and is unauthenticated unless
  `MCP_SWITCHBOARD_PRIVATE_TOKEN` is set.
- **H1a.** With the orchestrator enabled the private listener also holds **LLM provider
  credentials** and the **entire chat history**. Its threat model changes accordingly; see §10.
- **H2. Hello validation.** Rejects with an `error` frame and closes on: unsupported protocol
  version, server or project names containing `__`, or two servers with the same name in one hello.
  An optional `client.environment` is decoded tolerantly (unknown kinds kept, malformed dropped, sizes capped) and never rejects a hello.
- **H3. Sessions.** Per tunnelled server, one MCP client session (`initialize`, `tools/list`).
  Tools appear in the catalog when the session is up and disappear when the server stops.
- **H4. Restarts are reconciled, not reacted to.** Each server channel has one supervisor goroutine
  holding the last state the client reported, latest-wins. It retires the session on any new state
  and opens a new one only once the server has held `running` for
  `MCP_SWITCHBOARD_SERVER_SETTLE_DELAY` seconds (default 0.15).
  A stop aborts an in-flight `initialize`, and nothing about a session is done on the tunnel's
  receive loop, which carries every server on that client.

  > Both properties are load-bearing, and the first one is not obvious. Opening a session per
  > `running` frame and cancelling it on the next `starting` looks equivalent and is not: cancelling
  > does not un-send an `initialize` already on the wire. During a burst of restarts those stale
  > handshakes arrive at the process the client has *just respawned*, which rejects the second one
  > ("the initialize handshake is not accepted") and leaves the channel holding a session that
  > cannot list tools. Settling collapses a burst into the one session its final state calls for.
  > The second property is why the receive loop never blocks on a session: waiting there stalls
  > every other server on the tunnel, including the MCP replies the handshake being waited on needs.
  > `STOP_WAIT_TIMEOUT` (5 s) survives only as the bound on how long shutdown waits for a
  > supervisor to unwind.
- **H5. Tool naming.** As I4. Each tool is also tagged in `title` (`tool · server @ label`), at the
  front of `description` (`[label · server] …`), and in `_meta` (`host`, `project`, `server`,
  `connectionId`, `upstreamName`). The description tag matters because it is the field an LLM
  actually reads when choosing a tool — including *our own* agents.
- **H5a. The agent-facing catalog omits `connectionId`.** At `/mcp/agent/{id}` the `_meta` block
  carries only the stable fields (`host`, `project`, `server`, `upstreamName`). `connectionId` is
  regenerated on every reconnect (I2), so leaving it in a tool definition that is serialised into a
  provider request would invalidate the prompt cache on every client reconnect — exactly what
  §5.4's stable naming exists to prevent.
- **H6. Scopes.**

  | Path | Scope | Names |
  |---|---|---|
  | `/mcp` | all | `label__[project__]server__tool` |
  | `/mcp/host/{label}` | host | `[project__]server__tool` |
  | `/mcp/host/{label}/project/{project}` | project | `server__tool` |
  | `/mcp/host/{label}/server/{server}` | server | `tool` |
  | `/mcp/host/{label}/project/{project}/server/{server}` | project + server | `tool` |
  | `/mcp/agent/{agent_id}` | chat's execution record | `label__[project__]server__tool`, filtered to that record's grants, plus `switchboard.*`. **Internal to the run loop** — not offered to external consumers (§5.5, Q2) |

  Calls resolve against the same scope they were listed in. All scopes are backed by one server
  instance and one session manager; the scope is derived per request from path parameters.
- **H7. Endpoints panel.** `GET /api/endpoints` returns the local base URL
  (`MCP_SWITCHBOARD_LOCAL_BASE_URL`, default `http://127.0.0.1:<private port>`), one row per
  reachable scope with an example tool name, and the client-install command when
  `MCP_SWITCHBOARD_PUBLIC_URL` is set. With nothing connected, the rows are `/mcp` and a
  `/mcp/host/<host>` placeholder.
- **H8. Call log.** Every tool call — console, MCP consumer, REST, or agent — is written to the
  store with arguments and result. See §6.3 for retention, which the orchestrator changes.
- **H9. Observability.** Prometheus `/metrics` (`mcpsb_*`); optional batched Loki push whose
  bounded queue drops (and counts drops) rather than blocking a call.

### 4.2 Go package layout

Paths below are final. The Python hub was moved to `legacy/hub/` at the start of step 3 of §15
and deleted at step 10, so `hub/` means the Go tree.

```
hub/
  cmd/mcp-switchboard-hub/main.go     flags, env, signal handling, serve
  internal/
    protocol/      tunnel frames, generated from docs/protocol.json
    config/        Settings, env loading, file-path substitution (P3)
    registry/      live Connection/ServerChannel tree, ServerRef, scope composition
    tunnel/        WebSocket handler, hello validation, frame pump
    mcpsession/    one go-sdk ClientSession per tunnelled server, restart discipline
    mcpserver/     the consumer-facing MCP server, all six scopes
    store/         Store interface + sqlite implementation + migrations
    calls/         CallRecord, dispatch instrumentation
    agents/        orchestrator: runs, mailbox, permissions, budgets
    llm/           provider interface + anthropic/openai/openai-compatible impls
    api/           REST handlers, SSE, per-run streams
    metrics/       prometheus collectors
    events/        the change bus (fan-out, lossy by design — see §7.3)
  web/             React SPA source; dist/ embedded via embed.FS
```

- **G1. Concurrency model.** One goroutine per tunnel read loop, one per server channel writer,
  one per active run. A single `registry` guarded by an `RWMutex`; reads (which dominate: every
  `tools/list`) take the read lock. No global lock is ever held across an I/O call.
- **G2. Context propagation.** Every call path carries a `context.Context` from the originating
  request or run. Cancelling a run cancels its in-flight tool call, which cancels the MCP request
  on the channel. This is a capability the Python hub lacks and the orchestrator needs.
- **G3. Graceful shutdown.** SIGINT/SIGTERM cancels the root context; the hub stops accepting,
  drains in-flight tool calls up to `SHUTDOWN_GRACE` (default 10 s), marks every `running` run as
  `interrupted` in the store, then exits 0.
- **G4. No global registries.** Prometheus collectors live on a hub-owned registry, never
  `prometheus.DefaultRegisterer`, so two hubs in one test process do not collide.

## 5. Agent orchestrator

### 5.1 Model

A **chat** is what the user sees and works with: a title, a system prompt (a profile reference, its
own text, or none), at most one MCP client, and a **DAG of messages**. A chat may have **child
chats** (`parent_chat_id`, A29), which the console shows nested under it.

Underneath, every chat has one **execution record** — the `agents` row of §5.2. It holds what a run
needs (model, prompt, budget, grants, approval, status, the parent→child tree, cumulative cost) and
is **1:1 with its chat** for everything created from now on: creating a chat creates its record,
deleting the chat deletes it (A26, A28). Data from before that, where one agent has several chats,
keeps working — a run is already per chat — and such chats simply appear as independent chats. The
record is an implementation detail: the console shows it only in the Graph view (as a chat node)
and the `/api/agents*` routes remain for that view and API callers.

An **agent record** is therefore a durable, named configuration: a model, a system prompt, a set of
tool grants, and a place in a parent→child tree. It is not a process.

A **run** is one execution of a record against one chat: the loop *prompt → model → tool calls →
model → …* until the model stops calling tools, a budget is exhausted, or it is cancelled. Runs
are the unit of concurrency, cancellation and metering.

A **profile** (§5.2a) is a reusable system-prompt template a chat may reference; it is not itself an
agent and has no place in the tree.

Chats communicate by **injecting a user-role message with `sender` metadata into the recipient's own
chat** and waking it as a human message would (B7); there are no separate peer or "conversation"
chats.

- **A1.** Records outlive runs. Killing a run does not delete the record; deleting a chat (A28) or
  an agent (`DELETE /api/agents/{id}`) cancels its runs and its descendants' runs.
- **A2.** Inference happens **in the hub process**. The hub holds provider API keys. This is the
  decision that makes the hub a stateful application rather than a gateway, and it is why §10 is
  substantially longer than it used to be.
- **A3.** Runs do not survive a hub restart. On startup, any run still marked `running` is marked
  `interrupted` with a message explaining why, and its chat remains readable and branchable.
  Durable resumption is explicitly **out of scope** for this revision (§13).

### 5.2 Entities

`agents` — the execution records of chats (§5.1). The console never presents one as a thing of its
own; the Graph view draws each as its chat.

| Field | Type | Notes |
|---|---|---|
| `id` | uuidv7 hex | |
| `parent_id` | uuid, null | null for a root agent; establishes the tree |
| `name` | string | display name; unique within a parent |
| `description` | string | shown in the Graph view and given to the parent when it spawns |
| `project` | string, null | scoping + a metrics label (bounded cardinality, unlike `agent_id`) |
| `model` | json | see §5.7 |
| `system_prompt` | text | |
| `depth` | int | derived and stored; `parent.depth + 1`, enforced against `AGENT_MAX_DEPTH` |
| `status` | enum | `idle` \| `running` \| `waiting` \| `blocked` \| `done` \| `error` |
| `budget` | json | §5.6 |
| `capabilities` | json | `{can_spawn: bool, can_message: bool}`, default both `false`; drives M1 |
| `approval` | enum | `never` \| `destructive` \| `always`; §5.8. Set at creation, never re-derived |
| `auto_wake` | bool | default `true`; when `false` mail accumulates instead of enqueuing a run (B2) |
| `token_total`, `cost_total_micros` | int | **cumulative, self plus all descendants** (B5) |
| `created_at`, `updated_at`, `last_activity_at` | timestamptz | RFC 3339 UTC |
| `deleted_at` | timestamptz, null | soft delete; history stays readable |
| `profile_id` | uuid, null | the profile this agent references *live* (A23), or inherited from its parent (M3); `ON DELETE SET NULL`. Null when the agent uses its own prompt |
| `origin` | enum | `manual` (a root made through `POST /api/agents` with a `clientLabel` key, A20) \| `spawn` (a child: made by `switchboard.chat.spawn`, by `POST /api/chats` with a `parentChatId`, or with a `parentId`) \| `chat` (the record of a top-level chat made by `POST /api/chats`, A26; also a root made through `POST /api/agents` without `clientLabel`). Default `chat` (D12) |
| `client_label` | string, null | the one MCP client the agent is bound to (A19), null for none; kept in step with the grants, and cleared on a descendant when narrowing removes its access (A14) |

`name` uniqueness needs a partial index, not a plain one: `parent_id` is `NULL` for roots and
SQLite treats NULLs as distinct, and soft-deleted siblings must not hold a name hostage. The index
is therefore on `(COALESCE(parent_id, ''), name) WHERE deleted_at IS NULL`.

`status` is derived state, written on every transition and used for the Graph badges:
`idle` (no run), `running` (a run is executing), `waiting` (run is blocked on a tool call),
`blocked` (waiting on a human approval or on a child), `done`, `error`.

`chats`

| Field | Type | Notes |
|---|---|---|
| `id` | uuidv7 hex | |
| `agent_id` | uuid | the chat's execution record (§5.1): 1:1 with the chat when created after D13, shared by several chats only in older data |
| `parent_chat_id` | uuid, null | the chat that spawned this one (A29); a plain column (no foreign key) cleared by a trigger when the parent is deleted (D13). Null for top-level chats |
| `peer_agent_id` | uuid, null | **legacy, no longer written**: set on chats of the retired agent↔agent threads (`kind` `agent`/`spawn`), which stay readable as ordinary chats |
| `title` | string | `"New chat"` by default, replaced by the first user message while untouched; editable |
| `kind` | enum | `human` for every chat written now; `agent` \| `spawn` only on legacy peer chats |
| `active_leaf_id` | uuid, null | which leaf of the message DAG is the "current" conversation |
| `tags` | json array | |
| `token_total`, `cost_total_micros` | int | denormalised running totals, for list rendering without a join |
| `created_at`, `updated_at` | timestamptz | |
| `archived_at` | timestamptz, null | |
| `profile_id` | uuid, null | the chat's profile reference, mirrored onto its record; `ON DELETE SET NULL` (A17) |
| `client_label` | string, null | the chat's one client (A19), mirrored onto its record; a child chat records its parent chat's value when spawned (M3) |

> A chat is created with its execution record in one step (`POST /api/chats`, A26). Rebinding its
> profile, own prompt or client (`PATCH /api/chats/{id}`, A27) changes the record and the mirrored
> fields together. Each chat keeps its own context window: another chat's words reach it only as
> messages injected into it (B7), never through a shared history, which avoids the
> shared-mutable-history problem the moment two sides branch independently.

### 5.2a Profiles

`profiles`

| Field | Type | Notes |
|---|---|---|
| `id` | uuidv7 hex | |
| `name` | string | unique among profiles (409 on a clash) |
| `description` | string | |
| `system_prompt` | text | required |
| `model` | json, null | `{provider, model, ...}` as §5.7; null means the first configured model |
| `capabilities` | json | `{can_spawn, can_message}`: which hub tools (§5.5) an agent made from it is offered (M1). Default both `false` |
| `approval` | enum | `never` \| `destructive` \| `always`, default `destructive`; becomes the agent's stored mode (W1) |
| `budget` | json | same keys as `agents.budget` (§5.6); `{}` means the hub defaults |
| `is_default` | bool | exactly one profile is the default |
| `created_at`, `updated_at` | timestamptz | |

The REST shape is camelCase (`systemPrompt`, `isDefault`, `capabilities: {canSpawn, canMessage}`)
and is specified in [docs/PROFILES_API.md](docs/PROFILES_API.md); the agent side of the model
(manual creation, live reference, optional client) in [docs/AGENT_MODEL_API.md](docs/AGENT_MODEL_API.md),
which wins where the two differ.

- **A16. A profile is not an agent and carries no grants.** It has no place in the tree, no runs and
  no MCP permissions of any kind; the Graph view never shows one (I8).
- **A17. Exactly one default.** A partial unique index on `is_default = 1` makes "at most one" a
  schema fact, and making a profile the default clears the previous one in the same transaction.
  The default cannot be un-set except by making another profile the default, and the default
  profile — like the last remaining one — cannot be deleted. Deleting any other profile leaves
  every chat and agent using it working: their `profile_id` becomes null and the agent keeps the
  prompt, model, capabilities, approval and budget it last had (A23). On first start with
  the orchestrator enabled and no profiles, the hub seeds one default profile, `Assistant` (prompt
  `You are a helpful assistant.`, no hub tools, approval `destructive`).
- **A18. A chat is created whole.** `POST /api/chats` creates the chat and its execution record
  together (A26); the console never creates an agent. Its prompt is a profile (`profileId`), or the
  chat's own `systemPrompt` (empty meaning no system prompt at all), and its client is exactly one
  client label or none (A19). The record is named `<title> · <short id>` (unique among siblings
  whatever the titles), gets `origin = chat` (`spawn` under a parent chat) and no automatic
  `AGENT_DEFAULT_GRANTS`. The Chat panel groups chats by `client_label`, never by prompt or profile
  name, and nests child chats under their parent (A29).
- **A19. Grants are exactly `(clientLabel, *, *)`, or none.** A chat with a client holds
  that one allowed grant, `source = 'explicit'`, and nothing else: it reaches every server of that
  one client and no other client's. A client-less chat holds no grant at all: its catalog is the
  hub tools its capabilities allow (M1) and nothing from any client. A record with no grants is
  simply not pinned (§5.4), which is harmless as it has no client tools to name.
- **A20. `AGENT_DEFAULT_GRANTS` and `POST /api/agents` are the agent-level paths.** The default
  grants apply only to a root record created through `POST /api/agents` **without** a
  `clientLabel` key (the Graph view's spawn dialog, API callers), which gets `origin = chat` and
  a `client_label` only if its grants happen to pin one client, and a first chat (nested under its
  parent's chat when it has a parent). A `POST /api/agents` **with** a `clientLabel` key creates a
  `manual` record with exactly the A19 grants and no chat; `POST /api/chats {agentId, title?}`
  then attaches a chat to it, copying the record's profile and client. **`agentId` present means
  this legacy attach form and takes precedence** over every other field of the body. The older
  `{profileId?, clientLabel?, title?, model?}` body is the primary form (A26) with fewer fields
  (`*` and the empty string are a 400 for `clientLabel`; 404 on an unknown profile).
- **A21. Tool inspection uses the run loop's own function.** `GET /api/chats/{id}/tools` is
  computed by the same catalog builder a run calls each turn (§5.4), so it cannot disagree with
  what the model is offered; `GET /api/hub-tools` lists the `switchboard.*` definitions with the
  capability each requires (M1). Both show real dotted names (I5); the provider-safe name mapping
  (§5.7) is internal.
- **A22. A pinned agent's tools are named without its client (§5.4 step 3).** Because such an agent has
  exactly one client, the tool list shown by A21 and offered to the model drops the client label,
  and drops the server name for the `harness` server; other servers keep theirs. The names an
  operator sees in the chat's tools panel are exactly the names in the model's requests.
- **A23. The profile reference is live.** While an agent's `profile_id` is set and the profile
  exists, the agent's effective `system_prompt`, `model` (else the first configured model),
  `capabilities`, `approval` and `budget` are the profile's *current* values. They are resolved at
  the start of each run (which fixes the budget snapshot), again at the start of every turn (prompt,
  model, catalog, the approval gate of W1), and for each `/mcp/agent/{id}` call, and they are
  written back onto the agent row every time they differ (and when the profile is edited), so
  deleting the profile, which only nulls the reference, never blanks a prompt. Without a reference
  the agent's own fields apply. **Sub-agents** (`origin = spawn`) keep the profile reference for
  display only: they run on the values they were spawned with, because a spawn call may narrow
  capabilities and approval (M3) and a live profile value would silently undo that.
- **A24. Rebinding the client.** `PATCH /api/agents/{id} {clientLabel}` (and A27's chat form)
  rewrites the record's grants to exactly `(newClient, *, *)` with `source = 'explicit'` (or to none
  for `null`) in one transaction, updates `client_label`, and, as a human edit (A13), propagates the
  narrowing by A14: every descendant's inherited allow row for any other label is deleted, and a
  descendant whose `client_label` is no longer served is cleared. Nothing is ever added to a
  descendant.
- **A25. The system prompt is inspectable and computed once.** `GET /api/chats/{id}/system-prompt`
  returns `{systemPrompt, source: "profile" | "agent" | "none", profileId, profileName, model:
  {provider, model} | null, toolCount}` for the chat's agent as the next turn will send it. The
  text comes from `systemPromptFor`, the single function the run loop uses to build a turn's system
  prompt, over the same resolved agent (A23), and `toolCount` is the size of the same turn plan's
  catalog (§5.4). Today the hub adds nothing of its own: the prompt is exactly the effective
  `system_prompt`, verbatim, with no sub-agent or tool notes prepended or appended (tools travel as
  the request's tool list). Anything added later goes into that function and is reported here.

- **A26. Chat creation.** `POST /api/chats {title?, profileId?, systemPrompt?, clientLabel?, model?,
  parentChatId?}` creates the chat and its record together and returns the Chat (201). The prompt
  source is decided as follows: `profileId` naming a profile is a live reference (A23; 404 if it does
  not exist); an explicit `profileId: null` means no profile, and the chat's own `systemPrompt`
  (default empty) is used; **`profileId` omitted means the default profile only if `systemPrompt`
  is also omitted** — a request that sends its own `systemPrompt` and no `profileId` gets that text,
  not the default profile. `clientLabel` omitted or `null` is no client; `*` or the empty string is
  a 400. `title` defaults to `"New chat"` and is replaced by the first line of the first user
  message while the chat has no messages. `model` overrides the profile's initial model copy
  (`{provider, model}`). `parentChatId` makes a child chat (A29); the child's grants are bounded by
  the parent's (A12), so a `clientLabel` outside the parent's is simply not granted.
- **A27. Rebinding a chat.** `PATCH /api/chats/{id}` additionally accepts `profileId` (string, or
  `null` to detach: the record keeps the values it last had), `systemPrompt` (the chat's own text,
  used only while no profile is referenced, else ignored) and `clientLabel` (string or `null`,
  A24). They change the chat's record and the mirrored `profile_id`/`client_label` of the record's
  chats (and clear the client of descendants' chats that A14 narrowed away). A child chat (A23:
  `origin = spawn`) that is bound to a profile this way takes the profile's values once, not live.
- **A28. Deleting a chat is a cascade.** `DELETE /api/chats/{id}` permanently deletes the chat, its
  messages, runs and draft, **every child chat** (`parent_chat_id`, transitively) and each
  execution record no remaining chat uses — and that has no live child record outside the deleted
  set, so older data whose child chats were never linked is not swept away — with the record's
  descendants, grants, edges, inbox and call rows. Runs on the deleted chats are cancelled first.
  All database changes happen in one transaction. The response is `200 {deletedChats: n}`.
- **A29. Sub-chats.** `switchboard.chat.spawn` (and `POST /api/chats` with a `parentChatId`)
  creates a *child chat* of the calling chat: a new chat with `parent_chat_id` set, whose record is
  a child of the caller's record (depth, child-count limits, the A8 edge and A12 grant bounds apply).
  It inherits client, capabilities, approval, model and profile reference (M3). An optional first
  `message` is injected into it as `sender.kind = "spawn"` (B7) and its final answer comes back into
  the spawning chat as a reply (B8). Nothing else ever creates a chat as a side effect of messaging.

`messages` — a DAG, not a list (the decision taken in §0):

| Field | Type | Notes |
|---|---|---|
| `id` | uuidv7 hex | |
| `chat_id` | uuid | |
| `parent_id` | uuid, null | null for the root message; **siblings are branches** |
| `role` | enum | `system` \| `user` \| `assistant` \| `tool` |
| `content` | json | array of content blocks (text, image, tool_use, tool_result) — provider-neutral |
| `tool_calls` | json, null | `[{id, name, arguments}]` as issued by the model |
| `tool_results` | json, null | `[{tool_call_id, call_id, result, error}]`; `call_id` joins `calls` |
| `token_input`, `token_output`, `cost_micros`, `latency_ms` | int | per-message metering |
| `model` | json, null | the full turn options that produced an assistant message: `{provider, model, max_tokens, temperature, thinking}` (may differ from the agent's current config) |
| `system_prompt` | text, null | the exact system prompt text that turn was generated against, set alongside `model` |
| `tools` | json, null | the exact tool definitions (`llm.Tool[]`: name, description, input schema) that turn was generated against, set alongside `model` |
| `finish_reason` | string, null | `stop` \| `tool_use` \| `max_tokens` \| `budget` \| `cancelled` \| `error` |
| `run_id` | uuid, null | which run produced it |
| `sender` | json, null | on a `user` message injected by another chat (B7): `{chatId, chatTitle, senderName, kind}`, `kind` = `message` (a `switchboard.chat.send`) \| `reply` (a returned answer, B8) \| `spawn` (the first task from the parent chat); `chatTitle` is the sender's title at that moment, `senderName` its execution record's name at that moment (M4a) — what a reply's `chat.send` should pass as `to`. Null on everything typed by a human or written by a model. The raw text is stored; the model-facing preamble is added when a request is built (B9) |
| `last_active_child_id` | uuid, null | which child was last active below this message; remembers a branch's leaf for A6 |
| `created_at` | timestamptz | |

- **A4. Branching is free.** "Branch here", "regenerate" and "edit and resend" are all the same
  operation: create a new message whose `parent_id` is the same as an existing message's, then set
  `chats.active_leaf_id` to the new leaf. No history is ever copied.
- **A5. Rendering a conversation** is a walk from `active_leaf_id` up `parent_id` to the root,
  reversed. The store provides this as one recursive CTE, not N queries.
- **A6. Sibling navigation.** The UI shows `‹ 2/3 ›` on any message with siblings, exactly as
  OpenWebUI does. Selecting a sibling re-points `active_leaf_id` at that subtree's deepest
  previously-active leaf (remembered per branch in `messages.last_active_child_id`).
- **A7. Full payloads are stored.** Tool arguments and results are kept verbatim in
  `tool_calls`/`tool_results`, *not* only by reference into `calls`, because the call log is
  pruned and chats are not (§6.3). The `call_id` cross-reference is kept anyway so the Calls view
  can jump to the message that caused a call while the call row still exists.
- **A8m. `model`/`system_prompt`/`tools` are a per-turn snapshot, not a live reference.** They are
  set once, when the assistant message is persisted, from exactly what that turn was sent (spec.md
  §5.3's `plan.system`/`cat.llmTools()`/the resolved model options) — never recomputed later. This
  is deliberate: the profile or grants a chat resolves through can change after the fact (A23), so
  without this a JSON export (§7.2) could only reproduce what a chat resolves to *now*, not what a
  past turn actually ran against — which breaks both offline prompt/tool iteration against a real
  transcript and provider-side prompt caching, which needs a byte-identical request to hit.

`edges` — communication permissions between chats' execution records (the tools name chats, M4).
**Edges are symmetric (D14):** one row stands for the pair, and `allowed` governs both directions
— there is no such thing as "A may message B but not B may message A". This is what makes the
connection *the* thing the agent reasons about: it does not have to know or care which side dialed
first.

| Field | Type | Notes |
|---|---|---|
| `agent_a_id`, `agent_b_id` | uuid | composite primary key, stored with the lexicographically smaller id first so `(a,b)` and `(b,a)` are the same row |
| `allowed` | bool | |
| `created_at`, `updated_at` | timestamptz | |

- **A8.** A parent↔child edge is created `allowed = true` on spawn (both directions, being the
  same row, D14). Any other edge defaults to denied; absence of a row means denied.
- **A9.** Cycles are permitted structurally — two chats may be allowed to message each other —
  but message loops are bounded by the budget rules of §5.6, not by the graph shape. This is
  deliberate: the failure this project exists to replace shipped a symmetric `heartbeat` that
  answered `heartbeat`, an infinite loop measured at ~80 msg/s (see PROTOCOL.md). A graph that
  permits cycles **must** have a termination argument that does not depend on the graph.
- **D14. Connections are independent of the tree.** The parent/child structure (`chats.parent_chat_id`
  / `agents.parent_id`) and the edge graph are two different relations that happen to agree at
  spawn time (A8). Once created, a **parent/child link is immutable**: nothing — no tool, no API
  route, no console action — ever changes or clears `parent_id`/`parent_chat_id` on an existing
  chat, and deleting the parent cascades (D13) rather than re-parenting. Edges, in contrast, are
  freely mutable and are how *any two chats*, related or not, get to talk: the console's Graph view
  (human operator, already trusted) may connect any two chats at all, through `PUT
  /api/graph/edges/{a}/{b}` (§7.2). There is no switchboard.* tool that opens an edge (§5.5) — a
  chat cannot wire itself up to an arbitrary other chat on its own — so this is what lets two
  unrelated leaf chats collaborate directly instead of relaying through a shared ancestor, but only
  a human sets that up.

`grants` — MCP permissions, server-level only:

| Field | Type | Notes |
|---|---|---|
| `agent_id` | uuid | |
| `label`, `project`, `server` | string | the `ServerRef` triple of I1; `project` is `''` for none |
| `allowed` | bool | |
| `source` | enum | `inherited` \| `explicit` \| `human` |
| `created_at`, `updated_at` | timestamptz | |

Primary key `(agent_id, label, project, server)`.

- **A10. Server-level only.** No per-tool granularity in this revision. A grant is "this agent may
  use everything `label/project/server` exposes", which maps exactly onto the existing scope
  mechanism and needs no new resolution path.
- **A11. Wildcards.** `label`, `project` and `server` may each be the literal `*`, matching any
  value. `(*, *, *)` is "everything", which is what a root agent created through the legacy
  `POST /api/agents` path gets by default unless `MCP_SWITCHBOARD_AGENT_DEFAULT_GRANTS` says
  otherwise (A20). A chat made by `POST /api/chats` never gets it: it holds `(client, *, *)`, or nothing, (A19).
- **A12. Inheritance and the escalation rule.** On spawn, a child's grant set is the parent's,
  intersected with whatever the parent asked for. **An agent can never grant a child more than it
  holds itself.** This is enforced at the one point where an agent *writes* a grant — spawn —
  against the granting agent's set at that moment; a chat cannot grant anything after the fact
  (§5.5), only a human, from the Graph view (A13).
  At **call time the agent's own stored grant set is authoritative and is not re-derived from its
  ancestors.** Narrowing reaches descendants through A14's propagating transaction, which is the
  single mechanism for it; re-deriving at call time would additionally revoke the `human` grants
  that A13 and A14 exist to preserve, and the two rules cannot both hold.
  A consequence worth naming: because a chat holds only `(client, *, *)`
  (A19), every descendant it spawns holds a subset of that, so **the one-client restriction of an
  agent propagates to its whole subtree by construction**, and asking for another client's grants
  yields nothing.
- **A13. Humans are not bound by A12.** A grant edited from the Graph view is stored with
  `source = 'human'` and may exceed the parent's set. It is rendered with a distinct badge, since
  it is the one way the tree's invariant is legitimately broken.
- **A14. Narrowing propagates; widening does not.** Revoking a grant on an agent revokes it on
  every descendant whose grant is `inherited`, transitively, in one transaction. Adding a grant
  affects only that agent. Descendant grants marked `human` survive a parent's revocation and are
  flagged in the UI as orphaned-by-policy.
- **A15. Offline servers.** A grant whose `ServerRef` matches nothing currently connected is held,
  not deleted (I3). Calling through it fails with `server not connected`, which is a normal tool
  error, not a permission error — the distinction is visible in the message.

`runs`

| Field | Type | Notes |
|---|---|---|
| `id` | uuidv7 hex | |
| `agent_id`, `chat_id` | uuid | |
| `trigger` | enum | `human` \| `agent_message` \| `spawn`. A user message posted over REST is `human` — there is one principal (X5), so console and API are not distinguishable and pretending otherwise would put a meaningless value in the metrics |
| `triggered_by_agent_id` | uuid, null | |
| `status` | enum | `queued` \| `running` \| `waiting` \| `done` \| `cancelled` \| `interrupted` \| `error`. Only `running` counts against `AGENT_MAX_CONCURRENT_RUNS` (R7) |
| `budget_snapshot`, `usage` | json | what it was allowed, what it actually spent |
| `error` | text, null | |
| `started_at`, `finished_at` | timestamptz | |

### 5.2b Skills

A **skill** (`skills` table, migration 009) is a named block of instructions: `name` (a slug,
`[a-z0-9][a-z0-9_-]*`, unique), `description`, `body` and `auto` (default true). Skills are global,
not per chat or profile. They have two entrances:

- **S1. `/name` runs a skill.** A user message whose text is `/name` or `/name arguments`, with
  `name` a known skill, is sent to the model as `<skill name="…">body</skill>` followed by the
  arguments. Only the model's copy changes: the stored message, and so the thread, keep exactly
  what was typed, and editing a skill later changes how old `/name` messages read to the model on
  later turns. An unknown `/word` (a path, say) is left alone. The console's composer offers a
  menu of skills while the message is a bare `/prefix`.
- **S2. The model may load `auto` skills itself.** While at least one skill is `auto`, the system
  prompt gets a listing appended (`name: description`, one line each, and how to load one) and the
  chat's tool list gains `switchboard.skill.load {name}`, which returns the body. Only that short
  listing costs tokens on every turn; a body is read on demand and, like any tool result, then
  stays in the transcript. `switchboard.skill.load` needs no capability, is read-only, and is not
  in `GET /api/hub-tools`. With no `auto` skill neither the listing nor the tool exists, and the
  system prompt is exactly the chat's own (A25). A skill with `auto` off is reachable by `/name` only.

### 5.3 The run loop

```
   enqueue(run)
       │
       ▼
 ┌─ load conversation: walk active_leaf_id → root, reverse
 │  resolve tool catalog: grants ∩ live registry (+ switchboard.* tools)
 │  ▼
 │  provider.Complete(ctx, messages, tools)        ← streamed
 │  ├─ token deltas ────────────────────────► per-run SSE stream (§7.4)
 │  ▼
 │  persist assistant message (with tool_calls)
 │  ▼
 │  finish_reason == tool_use ?
 │     no  → status=done, agent.status=idle, exit
 │     yes ↓
 │  for each tool call (concurrently, bounded by AGENT_MAX_PARALLEL_TOOL_CALLS):
 │     ├─ authorise against grants               → deny ⇒ synthetic error result
 │     ├─ approval required (§5.8)?              → block, wait, or auto-deny on timeout
 │     └─ dispatch through the *same* code path as every other call (§4, H8)
 │  ▼
 │  persist one tool message per result (R6)
 │  ▼
 └─ budget check (§5.6) → over ⇒ finish_reason=budget, status=done
```

- **R1. One dispatch path.** Agent tool calls go through the identical `dispatch` used by the
  console and by MCP consumers, with `source = "agent"` and `agent_id`/`chat_id`/`run_id` recorded
  on the call row. Every existing guarantee — call log, metrics, Loki, timeouts — applies to agent
  calls for free. This is the whole reason the orchestrator lives in the hub rather than beside it.
- **R2. Denied calls are not errors to the caller.** A call the agent is not permitted to make
  returns a *tool result* with `error: "not permitted: <ref>"`, so the model can adapt, and is
  recorded with `status = "denied"`. It is not raised to the API client.
- **R3. Cancellation** cancels the run's context, which aborts the in-flight provider request and
  every in-flight tool call (G2). The partial assistant message is persisted with
  `finish_reason = "cancelled"` so the transcript is never a lie about what happened.
- **R4. At most one run per chat** at a time. A second trigger for a busy chat is queued, not run
  in parallel, because two runs appending to one `active_leaf_id` is a lost-update race. This
  holds for a message injected by another chat exactly as for a human one (B7): its append is
  deferred until its run reaches the head of the chat's queue, so it never lands between a tool
  call and its results.
- **R5. Idempotency.** `POST /api/chats/{id}/messages` accepts an `Idempotency-Key`; a repeat
  within 10 minutes returns the original run rather than starting a second one. Mobile networks
  retry, and a retried POST that spends money twice is not acceptable.
- **R6. One `role: "tool"` message per tool result.** `tool_results` stays an array for
  provider-neutrality but holds exactly one element, and the DAG stays a chain: an assistant
  message with *n* tool calls is followed by *n* tool messages. A result is persisted and pushed to
  the stream the moment it lands rather than waiting for the slowest sibling call, and every result
  has its own id to link to, cite and branch from. The provider layer (L6) regroups them into
  whatever shape the provider wants on the next request.
- **R7. A run that is waiting does not occupy a concurrency slot.** `AGENT_MAX_CONCURRENT_RUNS` counts
  runs in `running` only. A run blocked on another chat's reply (`switchboard.chat.send` with
  `wait: true`) or on a human approval (§5.8) is `waiting`: it holds no slot, and it re-acquires
  one to resume. Without this, a parent waiting on a child holds the slot the child needs and a
  tree deeper or wider than the hub-wide cap deadlocks until `AGENT_REPLY_TIMEOUT` fires. A
  `waiting` run still counts against its own `max_wall_seconds`.

### 5.4 Tool catalog resolution

For an agent, the catalog is computed **once per turn** and held for that turn (T1); grant and
topology changes take effect at the next turn boundary:

1. Iterate live `(connection, channel)` pairs.
2. Keep those matching an `allowed` grant. Precedence is evaluated in two stages, in this order:
   **(a) any matching row with `allowed = false` denies**, whatever its specificity — a wildcard
   deny beats an exact allow, deliberately, so a broad revocation cannot be re-opened by a narrow
   row left behind; **(b)** otherwise the most specific matching `allowed = true` row applies,
   `exact` beating `*` field by field (`label`, then `project`, then `server`). Specificity
   therefore orders allows only, and never rescues a denied ref. V1 requires table-driven tests
   for exactly this function.
3. Compose names by the agent's **naming mode**, which is a function of its *stored grants* only
   (never of what is connected), so names stay stable regardless of what else dials in — an
   agent's prompt cache must not be invalidated because an unrelated machine connected (T1):
   - **Pinned** — every allowed grant names the same concrete client label. This is always the
     case for a chat that has a client (A19). The client prefix is redundant
     and is dropped: names are `[project__]server__tool`, and the first-party `harness` server
     (§9), which every client carries, drops its server prefix as well, leaving bare tool names
     such as `run_command`. Every other server keeps its prefix, so tools of different servers
     cannot collide. A tool call resolves only against that one client's servers.
   - **Unpinned** — wildcard or multi-client grants (agents made through the API, or with no grants): the fully
     qualified `label__[project__]server__tool` of `Scope.ALL`.
   A duplicate name within one agent's catalog is dropped with a warning, as before (I4).
4. Append the `switchboard.*` tools the agent's capabilities allow (§5.5, M1). While the agent
   references a profile those capabilities are the profile's current ones (A23). An agent without
   grants (A19) gets these and nothing else.
5. Drop names failing I4, and drop duplicates loudly.

- **T1.** Tool-set churn is expensive: it invalidates provider prompt caches. The hub therefore
  recomputes a running agent's catalog **only between turns**, never mid-turn, and a server that
  disappears mid-turn yields a `server not connected` tool error rather than a vanishing tool.
  Between turns it is recomputed only if a grant or the live registry actually changed, so a stable
  system re-sends a byte-identical tool block. Ordering is stable too, by composed name — see Q5.

### 5.5 Built-in `switchboard.*` tools

Exposed **only at `/mcp/agent/{id}`** (the id is the calling chat's execution record), where the hub
knows which chat is calling (X8). They are never listed at `/mcp` or at the host/project/server
scopes: those scopes have no principal, so "the caller's child chats", "the caller's grants" and
"the caller's mailbox" have no referent there.
Whether external consumers should be able to attach to `/mcp/agent/{id}` at all is Q2, and until it
is answered the scope is internal to the run loop. Every one of these is itself logged as a call.

The tools speak of **chats**: arguments and results carry chat ids or names, which the hub
translates to execution records internally (M4). The tool names of earlier revisions, which spoke
of agents, are not offered. Editing the graph itself — opening an edge between two unrelated
chats, or granting/revoking an MCP server — is a trusted-human operation now, done from the
console's Graph view or its REST API (§7.2), not a switchboard.* self-service tool; a spawned
child still gets an edge to its parent for free (A8), which is all `chat.send` needs day to day.
Neither `chat.spawn` nor `chat.send` pauses for approval (M2): a chat with either capability is
trusted to use it without a human in the loop for every call.

| Tool | Input | Output | Behaviour |
|---|---|---|---|
| `switchboard.chat.spawn` | `{title, system_prompt, model?, grants?, budget?, capabilities?, approval?, message?}` | `{chat_id}` | Creates a child chat of the calling chat (A29). `model`, `capabilities` and `approval` default to the caller's (M3). `grants` are intersected with the caller's (A12). Fails if the child's depth (`parent.depth + 1`) would exceed `AGENT_MAX_DEPTH`, or if the caller already has `AGENT_MAX_CHILDREN` live direct children. `message`, when given, is injected into the child as `sender.kind = "spawn"` and wakes it; its final answer returns to the caller's chat as a reply (B8). |
| `switchboard.chat.send` | `{to, message}` | `{message_id}` | `to` is a **name**, as returned by `chat.list`'s `name` field — not a chat id (M4a). Resolved against the same set `chat.list` shows: the caller's parent, a child, or a chat joined to it by an allowed edge (D14; never itself). Inserts `message` into the recipient's **own** (most recently active) chat exactly as if the human had typed it there — a plain `user`-role turn, distinguished only by `sender` metadata (chat id, title and kind, B7) — and wakes it (B2). Always fire-and-forget: there is no `wait`, no synchronous reply, and no special routing back. If the recipient wants to answer, it calls `switchboard.chat.send` back to the sender, exactly like any other message — the edge is symmetric (D14), so it always can. The one exception is `switchboard.chat.spawn`'s own `message` (B8): a spawned child's *final* answer is still returned to the spawning chat automatically, because that is a property of spawning a task, not of `chat.send`. |
| `switchboard.chat.list` | `{}` | `[{name, chat_id, title, status, relation, depth?}]` | Every chat the caller can currently message: its parent and children (`relation: "parent"` / `"child"`, with `depth`) plus every chat joined by an edge (`relation: "connected"`). One flat list, no scope parameter — this is deliberately the *only* way a chat discovers who it can talk to, so there is nothing to get wrong. Never reveals a chat the caller cannot message. `name` is what `chat.send`'s `to` takes. |
| `switchboard.chat.stop` | `{chat_id}` | `{cancelled}` | Cancels a descendant chat's runs. Only on descendants. |
| `switchboard.user.ask` | `{questions: [{question, header?, options?: [{label, description?}], multi_select?}]}` (1–4 questions, at most 6 options each) | `{answers: [{question, answers: [string]}]}` | Asks the user and waits (see M6). Each answer is the chosen option labels and/or the user's own words. Errors if the user skips, if nobody answers within `APPROVAL_TIMEOUT`, or when there is no run to pause (through `/mcp/agent/{id}`). |
| `switchboard.mcp.list_tools` | `{}` | `[{label, project, server, connected, tools: [{name, description}]}]` | The caller's own grants joined against the live registry: which servers it may use, whether each is connected right now, and — for a connected one — every tool on it, named exactly as the caller would call it (naming.go's client-pinning rule applies here too, so a single-client chat sees the same short names it would actually use). |

- **M6. Asking the user.** `switchboard.user.ask` runs on the approval machinery (W1–W3): the run
  leaves its slot and reads as `blocked`, an `approval_required` frame carrying `questions` is
  streamed, the question is listed in `GET /api/approvals` and `GET /api/runs/{id}` (a
  `PendingApproval` with `questions` set), and a push notification (kind `question`, body
  `asks: <first question>`) goes to a top-level human chat's subscribers. It is answered by
  `POST /api/runs/{id}/questions/{callId}` with `{answers: [{answers: [string]}]}`, one entry per
  question, none empty (400 otherwise; 409 once answered or gone). `POST …/approvals/{callId}` with
  `approved: false` skips it. Unlike the other switchboard tools it needs no capability
  (an exception to M1): any chat may ask its user.
- **M1.** The `switchboard.chat.*` and `mcp.list_tools` tools are only listed for a chat whose
  `capabilities.can_spawn` / `can_message` flags are set. A leaf worker chat gets none of them,
  which is both cheaper (fewer tokens) and safer. While a chat references a profile the flags are
  the profile's current ones (A23): editing the profile affects every chat using it, from its next
  turn. Each tool's requirement (`canSpawn`, `canMessage`, `canSpawn or
  canMessage`, or `always`) is derived from the same visibility predicate the catalog uses and
  served by `GET /api/hub-tools` (A21). The capability flags keep their names; the console words
  them in terms of chats.
- **M2. Annotations.** `chat.list` and `mcp.list_tools` are `readOnlyHint: true`. `chat.spawn` and
  `chat.send` are annotated `destructiveHint: false, openWorldHint: false`: their effects reach
  outside the caller, same as before, but they are the two actions §5.5 exists to let a chat take on
  its own, so §5.8's approval gate never pauses them regardless of approval mode. `chat.stop` is
  `destructiveHint: true, openWorldHint: false` (it kills work).

- **M3. Sub-chats inherit.** A child made by `switchboard.chat.spawn` inherits from its parent:
  **the client restriction** (through A12: its grants are a subset of the parent's, so it can never
  reach another client, whatever `grants` it is asked for), **`capabilities`**, **`approval`**,
  **`model`** and **`profile_id`** (its record's `origin` is `spawn`, and it keeps the values it was
  spawned with, A23). `capabilities` and `approval` may be given explicitly in the
  call but only to *narrow*: a capability the parent lacks, or an approval mode laxer than the
  parent's (`always` > `destructive` > `never`), is refused (A12 applied to M1 and W1). Depth and
  child-count limits still apply. The child chat records the spawning chat's
  `client_label` and `profile_id`, so the console can show which client a sub-chat belongs to.
- **M4. Chat ids in, records inside.** Tool arguments and results name chats; the hub resolves a
  chat id (or, for `chat.send`, a name, M4a) to its execution record and authorises by the
  **caller's own identity** (X8): an edge is checked between the two chats' records, and
  "descendant" means the record is below the caller's in the tree. An unknown chat, or one whose
  record is deleted, is not found; a chat outside the caller's reach is `denied`. A `/mcp/agent/{id}`
  call with no run behind it acts in the record's most recently active human chat (created on demand
  if it has none). Which chat stands for a record that has several (older data) is that same rule:
  its most recently active human chat.
- **M4a. Names, not ids, address a send.** `chat.send`'s `to` is the reachable chat's `name` (its
  execution record's `agents.name`, §5.2) rather than a uuid: a model composes and remembers a
  short human-legible name far more reliably than a chat id, and the two are equally safe here,
  since both are resolved only against the caller's own reachable set (M4), never trusted from the
  argument otherwise (X8). `name` is unique enough in practice to address by (it is built as
  `title · <id suffix>`, chats.go's `agentNameFor`), and a reachable set is small and already
  edge-gated, so a collision within it is not a realistic concern.

### 5.6 Mailbox, wake-up and termination

The draft this replaces had no answer for *how a stopped chat receives a message*. MCP is
request/response; a chat that is not running cannot be called. So:

- **B1. Every record has a durable mailbox** (`inbox` table: `id, agent_id, from_agent_id,
  chat_id, message_id, delivered_at`); `chat_id` is the recipient chat and `message_id` the
  injected message (B7), which already sits in that chat.
- **B2. Delivery wakes the recipient.** `switchboard.chat.send` injects the message into the
  recipient's own chat and enqueues a run for that chat, queued behind any run already on it (R4).
  A record with `auto_wake = false` accumulates the message instead (it is in its chat and in the
  mailbox, but no run starts) and shows a badge in the Graph view; there is no switchboard.* tool to
  drain the mailbox any more (§5.5) — the message is read from the chat itself, same as anything
  else in it.
- **B3. Delivery latency** is measured from the `send` call returning to the recipient's run
  starting, and is the `mcpsb_agent_message_delivery_seconds` histogram.
- **B4. Budgets are the termination argument.** Two different things were conflated here in an
  earlier draft, and they are now separate. The **per-run budget** is `agents.budget` merged with
  the hub-wide defaults, whichever is lower, and is snapshotted onto the run
  (`runs.budget_snapshot`):

  | Key | Default | Meaning |
  |---|---|---|
  | `max_turns` | 32 | model round-trips in one run |
  | `max_tool_calls` | 200 | per run |
  | `max_tokens` | 1,000,000 | input + output per run |
  | `max_cost_micros` | 5,000,000 ($5) | per run |
  | `max_wall_seconds` | 1800 | per run |

  The **lifetime budget** is a single key on the same object, and it is the one that bounds a
  cycle (B5):

  | Key | Default | Meaning |
  |---|---|---|
  | `max_lifetime_cost_micros` | 50,000,000 ($50) | cumulative spend of this agent **and all its descendants**, across every run it ever makes |

  The remaining limits are **structural hub-wide caps**, not budget keys, and are configured as
  `AGENT_MAX_*` (E6) rather than per agent:

  | Cap | Default | Meaning |
  |---|---|---|
  | `AGENT_MAX_DEPTH` | 4 | agent tree depth |
  | `AGENT_MAX_CHILDREN` | 8 | live direct children per agent |
  | `AGENT_MAX_CONCURRENT_RUNS` | 16 | runs in `running` at once, hub-wide (R7) |
  | `AGENT_MAX_PARALLEL_TOOL_CALLS` | 8 | concurrent tool calls within one turn |

- **B5. Cost accumulates up the tree, durably, and that is what terminates a cycle.** When a turn
  finishes, its cost and tokens are added to `agents.cost_total_micros` / `token_total` for the
  running agent **and every ancestor**, in the same transaction that persists the message — one
  recursive-CTE update, not a walk. Enqueuing a run is refused if the agent or **any** ancestor has
  `cost_total_micros ≥ max_lifetime_cost_micros`; the refusal is recorded as a run with
  `status = "done"`, `finish_reason = "budget"` so it is visible rather than silent.

  > This is the whole termination argument for the cyclic graphs A9 permits, so it is worth being
  > explicit about why the per-run budget is *not* sufficient: a message cycle starts a **new run**
  > per hop, each with a fresh `max_cost_micros`, so a per-run ceiling is never reached and never
  > terminates anything. Only a monotonically increasing lifetime total does. It is deliberately
  > never reset by anything other than a human editing the agent — which, per B6, is exactly how a
  > stalled tree is resumed.
- **B6. Exhausting a budget is a normal ending**, not an error: `finish_reason = "budget"`,
  `run.status = "done"`, with the specific limit named in the run's `usage`. The chat stays
  resumable — a human can raise the budget (for a lifetime cap, raise
  `max_lifetime_cost_micros`; the spent total itself is never rewound) and continue from the same
  leaf.

- **B7. Messages are injected into the recipient's own chat.** A `switchboard.chat.send` appends
  a `user`-role message to the recipient named by `to` (M4a) — its most recently active chat, under
  its active leaf, deferred by R4 — with `sender = {chatId, chatTitle, senderName, kind: "message"}`
  (§5.2), then wakes it exactly like a human
  message (trigger `agent_message`, `triggered_by_agent_id` = the sender's record). No chat is
  created for a message: there are no peer or "conversation" chats, and the ones older data holds
  are only read, never written. The console shows the message as "from <sender title>", linked to the
  sender chat, and never as something the human typed.
- **B8. Replies.** The recipient's final answer for a run woken by a message goes back to the
  sender: inline as the tool result for `wait: true`, otherwise **injected into the sender's own
  chat** with `sender.kind = "reply"`, waking it. A reply-triggered wake carries no reply route of
  its own, so answering a reply does not send another reply: without that rule two chats would
  ping-pong without either model choosing to (A9's cycle is otherwise still bounded only by B5).
  A `spawn` message's answer returns the same way.
- **B9. The model reads a preamble, the store keeps the raw text.** When a request is built, a
  user message carrying `sender` is prefixed by one fixed line naming the sender chat — for a
  `message`: `[Message from chat "<title>". Reply with switchboard.chat.send {to: "<sender
  name>"}.]`, for a `reply`: `[Reply from chat "<title>".]`, for a `spawn`: `[Task from your
  parent chat "<title>". Your final answer is delivered back to it.]` — then a blank
  line and the text. `<sender name>` is `sender.senderName` (M4a): the name a reply's `chat.send`
  should use, so the recipient never has to call `chat.list` just to answer. The line is produced
  by one function and only in the provider-facing conversion; `messages.content`, the search index
  and the console see the raw text. Being a deterministic function of stored data it does not
  disturb the catalog (T1) or the cached prefix, and token accounting is the provider's own usage
  figures, so it is unaffected.

### 5.7 Models and providers

```json
{
  "provider": "anthropic",
  "model": "claude-opus-5",
  "temperature": null,
  "max_tokens": 8192,
  "thinking": {"type": "enabled", "budget_tokens": 4000}
}
```

- **L1.** The `llm` package defines one interface:
  `Complete(ctx, messages, tools, opts) (<-chan Delta, error)` — always streaming, because the
  console streams and a non-streaming path would be a second code path to keep correct.
- **L2. Providers in this revision:** `anthropic`, `openai`, and `openai-compatible` (a base URL
  plus an optional key — covers Ollama, vLLM, LiteLLM, OpenRouter, LM Studio; Ollama is spoken to
  natively, L7). Each is a file in `internal/llm/`.
- **L3. Credentials** come from `MCP_SWITCHBOARD_LLM_<PROVIDER>_API_KEY`, subject to the
  file-path substitution rule (P3), so sops/systemd credentials work with no new mechanism.
  A key is never returned by any API endpoint and never logged.
- **L4. Model registry.** `GET /api/models` lists configured providers and their usable models,
  for the model picker. Models are declared in config (`MCP_SWITCHBOARD_LLM_MODELS`, a JSON file
  path) because pricing must be attached to them; `openai` and `anthropic` list declared models
  only. `openai-compatible` additionally discovers its models (L8). A model object is
  `{provider, model, prices, contextWindow?, supportsTools?, discovered?}`. A models-file entry may
  carry `context_window`, the per-model context override (L7).
- **L5. Cost.** Prices are `{input_micros_per_mtok, output_micros_per_mtok, cache_read, cache_write}`
  per model in that same file. A model with no price entry records `cost_micros = 0` and is
  flagged `cost_unknown` in the UI rather than guessing. All money is integer micros; no floats.
- **L6. Tool-call translation.** The provider layer converts between our provider-neutral content
  blocks and each provider's schema. Tool JSON Schemas pass through from `tools/list` untouched;
  a schema a provider rejects causes that *tool* to be dropped for that provider with a warning,
  never the whole request to fail.

### 5.8 Human-in-the-loop

- **L7. Native Ollama and the per-request context window.** Ollama's `/v1/chat/completions`
  always uses the server's default context (4096) because it cannot take a per-request `num_ctx`,
  so a prompt full of tool definitions would be silently truncated. The `openai-compatible`
  provider therefore detects Ollama lazily (strip a trailing `/v1` from the base URL to get the
  root; `GET {root}/api/version` returning JSON with `version` means Ollama; cached per provider;
  a network failure is not cached) or is forced with `MCP_SWITCHBOARD_LLM_OPENAI_COMPATIBLE_KIND`
  = `auto` (default) | `ollama` | `generic`. Ollama is spoken to via streaming `POST {root}/api/chat`
  (NDJSON) with `options: {num_ctx, temperature?, num_predict?}`: tool calls carry object
  arguments, tool results are `role: tool` with `tool_name`, images go in `images`, `message.thinking`
  becomes thinking deltas (and `think` is sent when the model reports the `thinking` capability),
  `message.tool_calls` become complete tool-call deltas with generated ids when Ollama gives none,
  and the final `done` chunk supplies the finish reason and `prompt_eval_count`/`eval_count`.
  `num_ctx` is, in order: the models-file `context_window` of that model; `MCP_SWITCHBOARD_LLM_OLLAMA_NUM_CTX`
  (int, default 0 = auto); else `min(model maximum from /api/show, 32768)` (`/api/show` cached per
  model with a TTL). If the final `prompt_eval_count` is at least the window, the prompt filled it
  and Ollama truncated: the hub logs a warning and sets `Usage.Truncated` (with `Usage.ContextWindow`)
  so the run can show it. Non-Ollama endpoints stay on the `/v1` SSE path.
- **L8. Model discovery.** For `openai-compatible` (never `openai` or `anthropic`) the hub
  discovers models: Ollama via `GET {root}/api/tags` then `/api/show` per model (`contextWindow`
  from `model_info`, `supportsTools` from the `tools` capability; embedding-only models are
  skipped); other servers (LiteLLM, vLLM, LM Studio) via `GET {base}/models`. It is skipped when
  the base URL host is `openrouter.ai` or `MCP_SWITCHBOARD_LLM_OPENAI_COMPATIBLE_DISCOVER=false`
  (default `true`). The list is refreshed once at startup in the background (a down provider never
  blocks or fails startup; the last good list is kept and the failure is logged once) and lazily
  every 60 s; `GET /api/models` triggers the refresh but waits at most about 2 s, serving the cache
  otherwise. Discovered models merge with declared ones: a declared entry wins for prices and the
  context override (it still gains the discovered context window and tool support for display);
  discovered-only models are usable immediately. Cost rule: models discovered on Ollama are local
  and free, so they are priced at zero (not `cost_unknown`), as are declared Ollama models with no
  price entry; discovered models on other servers stay unpriced (`cost_unknown`) unless declared.
- **W1.** Every agent carries `approval: "never" | "destructive" | "always"` as a stored column
  (§5.2). While an agent references a profile the mode is **the profile's** (A23), and a spawned
  child inherits its parent's (M3). Otherwise it defaults to `destructive` when the agent is *created* holding a grant that matches a
  `harness` server, and to `never` otherwise; it is never re-derived afterwards, because a mode
  that silently changed when a grant was edited would be a surprising way to lose a safety prompt.
  Editing grants later leaves the mode alone, and the Graph view shows it next to the grant list.
  With `destructive`, any tool whose MCP annotations
  say `destructiveHint: true` or `openWorldHint: true` — which is exactly `run_command`,
  `run_python`, `process_start`, `git_push`, `file_delete`, `file_write`, `edit_file`,
  `git_checkout`, `process_kill` (§9.6) — pauses the run.
- **W2.** A paused run sets `agent.status = "blocked"` and `run.status = "waiting"` — releasing
  its concurrency slot (R7) — emits an event, and shows in the Chat view as an approve/deny card
  with the full arguments. It is answered by `POST /api/runs/{id}/approvals/{callId}`.
- **W3. Timeout.** Unanswered after `APPROVAL_TIMEOUT` (default 1 h), the call is auto-denied and
  the run continues with a tool error saying so. It never hangs forever holding a goroutine.
- **W4.** The annotations that drive this are advisory data from an upstream server, so W1 is a
  usability feature, not a security control. §10 says what actually is one.

## 6. Storage

### 6.1 The Store interface

- **D1.** All persistence goes through one interface in `internal/store`, split into focused
  sub-interfaces (`CallStore`, `AgentStore`, `ChatStore`, `GrantStore`, `RunStore`, `ProfileStore`) so a caller
  depends only on what it uses. SQLite is the only implementation in this revision; the interface
  exists so a Postgres implementation can be added without touching call sites.
- **D2.** Every method takes a `context.Context`. Every multi-row mutation runs in one
  transaction; grant propagation (A14) and message-plus-leaf-pointer updates (A4) are
  *specifically* required to be atomic.
- **D3.** SQLite is opened with `journal_mode=WAL`, `synchronous=NORMAL`, `foreign_keys=ON`,
  `busy_timeout=5000`. A single write connection (serialised) and a pool of read connections:
  WAL allows concurrent readers, and one writer removes `SQLITE_BUSY` from the write path
  entirely.
- **D4. No ORM.** Hand-written SQL in one package. The query set is small and the indexes matter
  more than the ergonomics.

### 6.2 Migrations

The existing Python hub has **no migration mechanism** — `calls.py` runs `CREATE TABLE IF NOT
EXISTS` and nothing else, so adding a column to a deployed database silently does nothing and
then fails at insert time. The Go hub therefore starts with one, for its *own* future changes.
Existing `calls.db` files are **not** migrated: there is no deployment whose call log is worth
keeping, so the cutover deletes the file rather than carrying a compatibility path that would be
written once, run once and then maintained forever.

- **D5.** A `schema_migrations` table records applied versions. Migrations are numbered, embedded
  `.sql` files, applied in a transaction at startup, forward-only. They are owned by
  `hub/internal/store/migrations/` and are the single source of the schema.
- **D6. Migration 001 is the original schema, and is frozen.** It was one file while no deployment
  existed; once a development database at version 1 held real data it became immutable, and every
  later change is a new numbered file (D12). 001 holds: `calls` (including `agent_id`, `chat_id`,
  `run_id`, and `denied` among the `status` values), the orchestrator tables (`agents`, `chats`,
  `messages`, `edges`, `grants`, `runs`, `inbox`) with their indexes including the partial unique
  index on agent names (§5.2), and the FTS5 virtual table and triggers backing the chat search of
  U6.
- **D12. Migration 002 (`002_profiles`) is the first real migration.** It adds the `profiles` table
  with the partial unique index of A17, `chats.profile_id`, `chats.client_label` and
  `agents.profile_id` (nullable, the two foreign keys `ON DELETE SET NULL`). It is additive, so it
  applies both to an empty file and on top of a version-1 database with its data intact; nothing
  is backfilled (legacy chats keep null `profile_id` / `client_label`). Migration 003
  (`003_chat_drafts`) adds the `chat_drafts` table of N9 (`chat_id` primary key, `ON DELETE CASCADE`).
  Migration 004 (`004_agent_origin_client`) adds `agents.origin` (`NOT NULL DEFAULT 'chat'`) and
  `agents.client_label` (nullable) and backfills them on a live database: `origin = 'spawn'` where
  `parent_id IS NOT NULL`, `chat` for the rest; `client_label` from the agent's oldest chat that
  recorded one, else from its grants when they allow exactly one concrete label. Tests upgrade a
  version-3 database holding data.
- **D13. Migration 005 (`005_chats_as_unit`)** adds `chats.parent_chat_id` (nullable, no foreign
  key) and `messages.sender` (nullable JSON), both additive, and an index on `parent_chat_id`. It
  backfills best-effort on a live database: a legacy `kind = 'spawn'` chat with a `peer_agent_id`
  gets, as its parent chat, the *oldest human chat of that peer agent*; every other chat keeps a
  null parent. A trigger clears `parent_chat_id` on the children when a chat is deleted, so nothing
  dangles. No existing row or table is dropped or rewritten, and the retired columns
  (`peer_agent_id`, `kind`) stay as they were. Tests upgrade a version-4 database holding data.
- **D7. A pre-migrations database is refused, not adopted.** If `calls` exists and
  `schema_migrations` does not, the hub exits with a message naming the file and saying to delete
  it. Silently adopting it would leave a database missing every column 001 declares, failing at
  the first insert instead of at startup.
- **D8.** The hub refuses to start against a database whose recorded version is *newer* than the
  binary knows, rather than operating on a schema it does not understand.

### 6.3 Retention

Two retention policies now coexist, and conflating them was an inconsistency in the earlier draft
("indefinite chat persistence" alongside a 30-day call log).

| Data | Policy | Setting |
|---|---|---|
| `calls` | pruned by age **and** row count | `RETENTION_DAYS` (30), `MAX_ROWS` (100 000) |
| `agents`, `chats`, `messages`, `edges`, `grants` | never pruned automatically | — |
| `push_subscriptions` (§8.7) | removed only when the push service reports it gone (404/410), or by hand | — |
| `runs` | pruned by age, but only rows no surviving message references | `RETENTION_DAYS` |
| `inbox` | delivered rows pruned after 7 days | `INBOX_RETENTION_DAYS` |

- **D9.** Because chats outlive the call log, a message's `tool_results` hold the **full payload**,
  not a reference (A7). The `call_id` link is a convenience that may dangle; the UI degrades to
  "call record expired" rather than showing an empty result. `messages.run_id`, by contrast, is
  **not** allowed to dangle: messages are never pruned, so the retention job keeps any `runs` row
  still referenced by one. A run row is small and bounded by the number of turns that produced it;
  a dangling `run_id` would break the per-run grouping the Chat view renders.
- **D10.** Chats and agents are deleted only explicitly, and by soft delete (`deleted_at`).
  A hard-delete endpoint exists for GDPR-shaped requests and removes descendants' rows too.
- **D11. Growth is unbounded by design** for chats, which is a deployment fact worth stating:
  `/api/stats` reports database size and row counts per table so it is visible before it is a
  problem.

## 7. HTTP API and streaming

All under the private listener. JSON bodies, camelCase fields, RFC 3339 UTC timestamps with an
explicit `+00:00` offset. The camelCase rule covers this REST/SSE surface only: **MCP tool
arguments and results stay snake_case** (`chat_id`, `exit_code`), because they sit in the same
namespace as tunnelled servers' tools and a model should not have to learn two conventions inside
one catalog. The handler layer is the boundary, and it is the only place that translates.

### 7.1 Existing surface (preserved)

| Method | Path | Notes |
|---|---|---|
| GET | `/api/connections` | live registry snapshot |
| GET | `/api/endpoints` | H7 |
| POST | `/api/connections/{cid}/servers/{server}/tools/{tool}/call` | manual call |
| POST | `/api/connections/{cid}/servers/{server}/restart` | 204 |
| GET | `/api/calls` | filters: `label`, `server`, `tool`, `status`, plus new `agentId`, `chatId`, `source`; `limit` clamped to 1000 |
| GET | `/api/calls/{id}` | |
| GET | `/api/events` | global SSE change feed |
| GET | `/api/ws` | the console's live connection: the change feed and chat streams over one WebSocket (N9) |
| GET | `/metrics` | Prometheus |

- **N1.** A tool that answers with an MCP error is a **successful call that returned an error**:
  HTTP 200 with `status: "error"` in the body. Only an unknown connection/server/tool is 404.
  Folding the two together would make the console report transport failures as tool bugs.

### 7.2 New surface

| Method | Path | Notes |
|---|---|---|
| GET/POST | `/api/agents` | list / create. Agent JSON carries `origin`, `clientLabel`, `profileId`. `POST` with a `clientLabel` key (even `null`) creates a `manual` record with no chat (A20) and takes `profileId?`; without it the legacy behaviour applies (A20). The console uses `/api/chats` instead (A26); these routes remain for the Graph view and API callers |
| GET/PATCH/DELETE | `/api/agents/{id}` | `PATCH` also takes `profileId`, `clientLabel` (A24) and `name`; `DELETE` soft-deletes and cancels descendants |
| GET | `/api/agents/{id}/grants` · PUT | grant set; `PUT` applies A14 propagation atomically |
| GET | `/api/graph` | agents + edges + grants + live-connection join, in one response |
| PUT | `/api/graph/edges/{a}/{b}` | `{allowed}`; symmetric (D14) — order of `a`/`b` does not matter and governs both directions. There is no switchboard.* tool that opens an edge (§5.5); only the console (a trusted human operator), here, may connect any two chats at all |
| GET/POST | `/api/chats` | list (filter by `agentId`, `kind`, `tag`, `q`; each Chat carries `parentChatId`) / create. `POST` takes the primary form `{title?, profileId?, systemPrompt?, clientLabel?, model?, parentChatId?}` that creates the chat and its record together (A26; 400 on a `*`/empty label, 404 on an unknown profile or parent), or, when `agentId` is present, the legacy attach form `{agentId, title?}` (A20), which takes precedence |
| GET/PATCH/DELETE | `/api/chats/{id}` | `PATCH` sets `title`, `tags`, `archived`, `activeLeafId`, `selectMessageId` and rebinds `profileId`, `systemPrompt`, `clientLabel` (A27); `DELETE` is permanent and cascades to child chats and their records (A28), returning `200 {deletedChats}` |
| GET | `/api/chats/{id}/tools` | `{clientLabel, clientConnected, tools[]}`: what the chat's agent can call right now (A21) |
| GET | `/api/chats/{id}/system-prompt` | `{systemPrompt, source, profileId, profileName, model, toolCount}`: the exact system prompt the next turn sends (A25) |
| GET/POST | `/api/profiles` | list (default first, then by name) / create (409 on a name clash) |
| GET/POST | `/api/skills` | list by name / create (`{name, description?, body, auto?}`; `name` is a slug; 400 on a bad one, 409 on a clash) |
| PATCH/DELETE | `/api/skills/{id}` | any subset of `name`, `description`, `body`, `auto` / delete |
| GET/PATCH/DELETE | `/api/profiles/{id}` | `PATCH` takes any subset, including `isDefault: true` (A17); `DELETE` is 409 for the default or the only profile |
| GET | `/api/hub-tools` | every `switchboard.*` tool (the `switchboard.chat.*` names, M4) with its `requires` (M1, A21) |
| GET | `/api/chats/{id}/messages` | `?leaf=` walks that leaf; `?tree=1` returns the whole DAG. Each message carries `sender` (B7), null unless another chat injected it |
| POST | `/api/chats/{id}/messages` | append a user message and start a run; `Idempotency-Key` honoured (R5) |
| POST | `/api/chats/{id}/branch` | `{fromMessageId, content?}` — creates a sibling and re-points the leaf |
| GET/PUT | `/api/chats/{id}/draft` | unsent composer text, `{draft, updatedAt}`; empty `PUT` deletes (N9) |
| GET | `/api/chats/{id}/stream` | **per-chat SSE**, see 7.4 |
| GET | `/api/runs/{id}` · POST `/api/runs/{id}/cancel` | |
| POST | `/api/runs/{id}/approvals/{callId}` | `{approved, reason?}` (W2) |
| POST | `/api/runs/{id}/questions/{callId}` | `{answers: [{answers: [string]}]}` (M6) |
| GET | `/api/approvals` | every approval pending in any run, oldest first (N10) |
| GET | `/api/models` | configured providers, models and prices (never keys) |
| GET | `/api/stats` | db size, row counts, active runs |

The profile, hub-tools, chat-tools and system-prompt routes (and every chat route above) exist only
with `AGENTS_ENABLED=true`, like the rest of the table (E7).

- **N9. Chat drafts are server-side.** `PUT /api/chats/{id}/draft` `{draft}` stores the chat's
  unsent composer text (`chat_drafts`, one row per chat; body capped at 256 KiB, else 413), an
  empty or whitespace-only draft deletes the row, and `GET` returns `{draft: "", updatedAt: null}`
  when there is none. A `PUT` publishes `{type: "chat", chatId}` so other tabs refetch. Sending a
  message by a human (`POST .../messages`, the user-sibling form of `/branch`) deletes the draft in
  the same transaction that persists the user message, and a failed delete never fails the send.
  Deleting the chat cascades to its draft.

- **N10. Pending approvals are listable for notification.** `GET /api/approvals` returns
  `{approvals: [{runId, chatId, agentId, agentName, chatTitle, callId, tool, arguments, expiresAt}]}`
  (oldest first, `[]` never null): every approval currently pending in any run, where `tool` is the
  exposed name the agent sees, `arguments` the full object and `expiresAt` when W3 auto-denies. The
  hub publishes `{type: "agent", agentId}` when an approval becomes pending and again when it is
  resolved or times out (as the agent enters and leaves `blocked`), so a client that refetches the
  list on `agent` events always sees the change promptly.

### 7.3 The global event bus is lossy on purpose

`/api/events` fans out from a bounded per-subscriber queue and **drops the oldest event when a
subscriber falls behind**. That is correct for *change notifications* — a client that missed
"connections changed" just refetches — and it is the existing behaviour.

- **N2.** It is **not** correct for anything where every item matters. Token deltas must never go
  through this bus: a dropped delta silently corrupts the visible message with no way to detect
  it. This is why 7.4 exists as a separate mechanism, and it is a hard rule, not a preference.
- **N3.** Events on the global bus are therefore all *idempotent notifications*:
  `{type: "connections"}`, `{type: "call", call}`, `{type: "agent", agentId}`,
  `{type: "graph"}`, `{type: "chat", chatId}`, `{type: "profile"}`. None carries incremental content.

### 7.4 Per-chat streaming

- **N4.** `GET /api/chats/{id}/stream` opens a dedicated SSE stream scoped to one chat. Its queue
  is **unbounded within a bounded byte budget**; if a client exceeds it, the stream is *closed*
  with an explicit `overflow` event rather than silently dropping deltas — a visible failure the
  client recovers from by refetching the message.
- **N5. Every frame carries a monotonic `seq`** per run, and the SSE event id is the pair
  **`{runId}:{seq}`** — a bare `seq` would be ambiguous on a chat stream, which carries a queued
  run after the one before it (R4). `Last-Event-ID` resumes from the next `seq` of that run, so a
  phone that loses signal mid-answer catches up instead of showing a truncated message. Deltas are
  retained per run in a ring buffer until the run ends; if the requested `seq` has already been
  evicted, or the run is gone, the stream sends the same `overflow` event as N4 and the client
  refetches the message rather than resuming from a hole.
- **N6. Frame types:** `run_started`, `delta` (`{seq, messageId, contentIndex, text}`),
  `tool_call` (`{callId, name, arguments}`), `tool_result`, `approval_required`,
  `message_done` (the persisted message, authoritative), `run_done`, `error`.
- **N7. The persisted message is authoritative.** A client that has assembled a message from
  deltas replaces it wholesale with the `message_done` payload. Deltas are an optimisation for
  perceived latency, never the source of truth.
- **N8. Keepalive** comment every 15 s, `X-Accel-Buffering: no`, `Cache-Control: no-cache,
  no-transform` — the same headers the existing stream already needs to survive nginx.

## 8. Web console

React 18 + TypeScript + Vite + Tailwind, built to `hub/web/dist` and embedded with `embed.FS`.
The hub serves `index.html` for any unmatched non-`/api` path so client-side routing works, and
serves hashed assets with long cache headers.

- **N9. One WebSocket per tab.** Browsers allow about six HTTP/1.1 connections per host and an SSE
  stream never ends, so two streams per tab starved the pool after a few tabs (history that would
  not load, reloads that hung). The console therefore carries both feeds over `GET /api/ws` (same
  origin only), which the pool does not count. Messages are JSON: `{"op":"sub","chat","last"?}` and
  `{"op":"unsub","chat"}` from the client; `hello`, `event` (change feed), `frame` (`chat`, `name`,
  `id`, `data`), `overflow` (`chat`, `reason`) and `hb` (every 15 s) from the hub. The feeds keep
  their contracts (N2): the change feed is lossy, a chat subscription is lossless or ends in
  `overflow`. A `sub` with no `last` replays every run of that chat still in flight from its first
  frame (skipping one whose start was evicted), so a tab opened mid-reply shows the whole reply. The
  client reconnects with backoff, treats 45 s of silence as a dead link, resubscribes from each
  chat's last frame, and refetches everything after a reconnect. A saved composer draft is its own
  change-feed event (`draft`), so other tabs update it without refetching the chat. The SSE routes
  remain for other clients.
- **U1. Single artifact.** `go build` produces one binary containing the console. Nothing is
  fetched at runtime; the loopback-only-box constraint that made the old console build-free is
  satisfied by building ahead of time instead of not building at all.
  `hub/web/dist/` is **not** committed: `npm run build` in `hub/web` has to produce it before any
  `go build`/`go vet`/`go test` against `hub/`, or `embed.go`'s `go:embed all:dist` fails outright
  with "no matching files". Every build path does this itself rather than trusting a checked-in
  copy - CI's `hub (go)`, `end-to-end` and `console (playwright)` jobs each run `npm ci && npm run
  build` first, the Nix package builds the console with its own derivation (`nix/console.nix`, via
  `buildNpmPackage`) and copies its output in, and the Docker image has a dedicated `node:alpine`
  stage for it.
- **U1a. Asset URLs are absolute** (`base: "/"`). Every unmatched path serves `index.html` so
  client-side routing works, which means a relative asset URL would resolve against whatever route
  the browser is showing — `/calls/` would ask for `/calls/assets/…`. A hub behind a
  path-prefixing proxy sets `base` to that prefix at build time.
- **U1b. Caching is split.** Vite fingerprints every asset, so those are served
  `immutable, max-age=31536000`; `index.html` names them and is served `no-store`. Getting this
  backwards leaves a browser asking for the assets of a build that is no longer in the binary.
- **U2. Mobile first.** Every view is usable at 375 px. Concretely: the connections tree, the
  chat list and the graph sidebar collapse into a slide-over drawer below `md`; tables become
  card lists rather than horizontally scrolling; tap targets are ≥44 px; the chat composer is
  pinned above the keyboard with `dvh` units and `env(safe-area-inset-bottom)`.
- **U3. Data layer.** TanStack Query for fetch/cache; the global SSE feed (7.3) invalidates
  queries by key rather than patching cache entries by hand. Per-chat streams (7.4) are consumed
  by a dedicated hook that keeps deltas in component state and commits on `message_done`.
- **U4. Theming.** Tailwind with CSS custom properties. The console is Dracula by default,
  regardless of `prefers-color-scheme`; Alucard (Dracula's light variant) is an explicit opt-in
  via `data-theme="light"`. The look is deliberately minimal and compact: a 14 px monospace base
  (15 px on touch devices), 2 px corner radii, flat surfaces with hairline borders, and a slim
  header.
- **U5. Routes.** `/connections`, `/calls`, `/endpoints`, `/chat`, `/chat/:chatId`, `/graph`,
  `/graph/:agentId`, `/prompts`, `/prompts/:profileId`. (`/graph/:agentId` is keyed by the chat's
  execution record, the graph's own id space, and is never shown as such.) `/agents` and
  `/agents/:profileId` redirect to their `/prompts` counterparts, so old links and bookmarks keep
  working. `/` is not a page: it redirects to the last place visited (U25), else `/connections`.
  Anything that identifies a resource is in the URL (U28), so every one of these is linkable and
  survives a reload. (An earlier draft listed a `/settings` route; nothing implements it and the
  hub's settings are environment-only, so it is not a route.)

### 8.1 Chat view

- **U6. List pane.** All chats as a client -> chat -> child chat tree (U37), newest activity first,
  one row per chat with badges: model, a "legacy" tag on old peer conversations, token total, cost,
  status dot, tags. Search by title and full-text over message content (SQLite FTS5).
  Multi-select for bulk archive/delete/export. The search text and archived toggle are remembered
  by the browser (U26). There is no second list: a chat is one row and nothing under it expands
  except its own child chats.
- **U6a. Archive is quick, delete is not.** Hovering a row (or focusing anything inside it) shows
  an **Archive** button; on `(hover: none)` screens it is always shown, with a ≥44 px target. It
  issues `PATCH /api/chats/{id} {archived: true}` **without a confirmation**, removes the row
  optimistically, refetches, and stays out of the way of the row's link (it never opens the chat).
  Archiving hides a chat and is reversible, so it asks nothing. If the archived chat is the one
  open, the console falls back to bare `/chat` and drops that tab's remembered location (U27). When
  the list is showing archived chats the same button reads **Unarchive** (`archived: false`).
  **Delete** is permanent and has its own, deliberately two-step control (U52); the two are not
  merged, so "hide" never costs a confirm and "gone" always does.
- **U7. Thread pane.** Messages rendered from `activeLeafId` (A5). Markdown with syntax
  highlighting; code blocks copyable. Assistant messages stream token-by-token.
- **U8. Branch affordances.** Any message with siblings shows `‹ n/m ›`. Every assistant message
  has **Regenerate**; every message the human typed has **Edit** - both create a sibling (A4). A
  message injected by another chat (U54) has neither: it is not the human's to edit and there is
  nothing to regenerate. A "branch here" action on any message, injected ones included, starts a
  new leaf from that point. Branching never leaves the chat. The model named under an assistant
  message is a picker: choosing one regenerates that reply with it (`POST /branch` with `model`),
  as a new sibling.
- **U8c. Questions.** A pending `switchboard.user.ask` renders in the thread as a card: each
  question with its options (radio, or checkboxes when `multiSelect`) and an always-present box for
  the user's own answer, **Send answers** and **Skip**. Its chat is marked "has a question" in the
  list and raises the browser notification with the question as its body (U33).
- **U8d. Model pickers.** Every model choice is a searchable list, not a `<select>`: click to open,
  type to fuzzy-filter (subsequence match, consecutive runs and word starts score higher), arrows
  and Enter to pick, Esc to close. The composer's "default" choice names the chat's model:
  `gpt-oss (chat default)`.
- **U8b. Skills.** The Prompts tab lists skills under the prompts (`/prompts/skills/:skillId`,
  U5) with a form for name, description, instructions and "let the model use it" (§5.2b). In a
  chat's composer a message that is a bare `/prefix` opens a menu of skills (arrows, Tab or Enter
  to complete, Esc to close, tap on a phone).
- **U8a. Overview.** An **Overview** toggle in the thread header opens the whole message tree
  (`?tree=1`) as a node graph in a right-hand panel, like a member list: a column from `md` up,
  a slide-over below it, remembered in `mcpsb.ui.v1.chat.overview`. Only user and assistant turns
  are nodes (tool results fold away); the path the thread shows is highlighted, and clicking a
  turn selects it (`selectMessageId`, A5), switching the thread to that branch.
- **U9. Tool call inspection.** Each tool call renders as a collapsible card: name, resolved
  `label/project/server`, duration, status, and raw argument/result JSON with a copy button and a
  link to the corresponding row in Calls.
- **U10. Approvals.** A blocked run renders an inline card with the full arguments, Approve /
  Deny, and the remaining time before auto-deny (W3).
- **U34. Optimistic approval.** Approve and Deny remove the card at once: the approval leaves the run
  query and the global approvals query before the POST is sent, and the buttons are disabled while
  it is in flight. On success `["run", id]` and `["approvals"]` are invalidated; on a failure the
  approval is restored and an error toast says why, except `409` (already decided or timed out),
  where gone is the truth. A `tool_result` frame for the call, or a `run_done`, also drops the card.
- **U35. Running-tool status.** While a run is active the thread shows "Running `<tool>`…" from
  the `tool_call` frame until its `tool_result`, listing parallel calls ("Running run_command,
  fetch__fetch…") and skipping calls that wait for approval. Names are the exposed names, in
  monospace, as-is. The approval card headline is "Approve `<tool>`?" with the arguments, and a
  tool card with no result yet shows a spinner beside its name.
- **U36. Attention and notifications.** A chat needs the user when a run waits for approval
  (`GET /api/approvals`, any chat), a run failed, or the chat finished its turn while it was not
  the visible, focused one (a question from a chat is just its reply; a reply to a message another
  chat injected counts the same). Old peer conversations never raise attention. The Chat tab carries a badge
  with the count, list rows are marked "needs approval" / "run failed" / "new reply" until the chat is
  opened, and the document title is prefixed `(N) `. Data comes from `approvals`, `chats` and `agents`
  (the chats' run records) queries invalidated by the global feed's `agent` and `chat` events; a failing or missing
  `/api/approvals` reads as none. "Last seen" per chat lives in `mcpsb.ui.v1` (`attention.seen`); the
  first load is a silent baseline, so it never raises a storm. Notification text speaks of chats
  ("wants to run <tool>", "replied", "the run failed") under the chat's title. Browser notifications are opt-in through
  an explicit "Enable notifications" button (never on page load), raised only when the tab is hidden or the chat
  is not the open one, with one `tag` per chat, focusing the tab and opening the chat on click, and
  silently absent when unsupported or denied. The state machine is `lib/attention` (`AttentionTracker`).
- **U37. Chat tree: client, chat, child chats.** The list is a tree. **Client groups** come first,
  one per `clientLabel` that a listed chat carries (alphabetical), plus a **No client** group last
  (`clientLabel: null`); a group header shows the client through the shared badge (U40: `label ·
  project`, environment chips, greyed when the client is not connected right now), the number of
  chats under it, a **+ chat** button that opens the New chat dialog with that client preselected
  (U50), and, while collapsed, the count of chats that need the user (U36). Under a group are its
  **root chats**, ordered by latest activity anywhere in their subtree; under a chat are its
  **child chats** (`parentChatId`, U56), recursively, each a normal row. A chat whose parent is not
  in the list (deleted, archived, or not matching a search) and every legacy peer chat
  (`kind: agent`, or `spawn` with `peerAgentId`; tagged "legacy") is simply a root: there is no
  "Older chats" node and no other special grouping. The tree never groups or labels by prompt or
  profile. A row's status dot is its run record's status (idle, running, waiting, blocked, error).
  A group or a chat with children collapses (remembered as `chat.tree.collapsed`, keyed by
  `client:<label>`, `client-none` and chat ids); a collapsed chat shows how many sub-chats it
  hides, its children are not rendered, and the open chat's ancestors are always open. While a
  search is active everything is open so the matches are visible. Rows are at least 44 px and
  indentation stops growing after three levels. Pure construction is `lib/chatTree`
  (`buildChatTree`).
- **U11. Composer.** Model override for the next turn, attachment of files (stored as content
  blocks), stop button while a run is active, and a visible budget meter (turns / tokens / cost
  against the run's limits). The unsent text is a **server-side draft** (N9), not browser memory,
  so it follows the chat to another device; U24 governs only what the browser itself remembers.
- **U12. Export.** A chat exports as JSON (full DAG) or Markdown (the active path).
- **U19. New chat.** The top **New chat** button, and **+ chat** on a client group (which
  preselects that client), open the New chat dialog (U50). Creating issues one `POST /api/chats`
  and opens the chat. Nothing else creates a chat in the console except a sub-chat spawned from the
  Graph (U18). A chat with no client at all is simply a chat whose client is None (A19).
- **U20. Chat header chips.** The header shows the chat's prompt ("prompt: <name>" for a profile,
  "custom prompt", or "no prompt") and its client through the shared badge (U40; greyed when not
  connected, from `clientConnected`), or "no client". A **Settings** button opens the chat settings
  (U55).
- **U21. Tools panel.** A chat has a panel listing what it can call right now, from
  `GET /api/chats/{id}/tools`: MCP tools grouped by `label/project/server` and hub tools
  (`switchboard.chat.*` and friends, named as the API returns them), each with
  its description, schema and annotations. It refetches on `chat`, `agent` and `connections`
  events, so a client dropping or reconnecting is visible.

- **U50. New chat dialog.** One form, in a dialog (a bottom sheet below `md`): an optional
  **title**, a **system prompt** source and an **MCP client**. The prompt is a saved prompt from the
  Prompts tab, by live reference and preselecting the default one; **None**, no system prompt; or
  **Custom...**, a textarea. The client is exactly one connected client, each shown with the shared
  badge (U40) and whether it is connected, or **None**, no client tools. It issues `POST /api/chats`
  with `clientLabel` always present (`null` for None) and, for a prompt, `profileId: <id>` (and no
  `systemPrompt`); for None `profileId: null`; for Custom `profileId: null` and the text as
  `systemPrompt` (docs/CHAT_MODEL_API.md: an omitted `profileId` would mean the default prompt, so
  "no prompt" is always an explicit null). A hub error is shown inline and the dialog stays open.
  The user never creates or names an agent; there is no such concept in the Chat panel.
- **U51. System prompt viewer.** The chat header has a **System prompt** button opening a bottom
  sheet below `md` and a right-hand drawer from `md` up, showing `GET /api/chats/{id}/system-prompt`
  on every open: the exact text (monospace, whitespace preserved, never truncated) with a copy
  button, the source ("from prompt X", "this chat's own prompt", or "none - no system prompt is
  sent"), the model, the tool count, and, when the source is a saved prompt, a link to edit it
  (`/prompts/:profileId`).
- **U52. Easy delete.** Beside Archive (U6a) every chat row has a **Delete** button, shown on hover
  and focus-within and always on touch. Pressing it arms the row in place: the button becomes
  "Delete?" with a check mark and a cross (no modal), which disarms by itself after a few
  seconds; a chat with sub-chats says so ("Delete + 2 sub-chats?") because deleting a chat deletes
  its child chats too. The check mark deletes (`DELETE /api/chats/{id}`, the chat and its
  sub-chats removed from the list at once, a toast reporting `deletedChats`, the open chat, or one
  inside the deleted subtree, falling back to bare `/chat` and dropping that tab's remembered
  location, U27). The chat header menu has **Delete chat...** with the same inline two-step, worded
  "Delete this chat and its sub-chats?". Multi-select bulk delete keeps its confirm, which says
  the same.
- **U53. One hook per resource.** Run records (`agents`, one per chat: status and model) and
  prompts (`profiles`) are read through one shared hook each (`api/resources`: `useAgents`,
  `useProfiles`, `useAgent`) with one cached shape (a plain array), under the roots `["agents"]`
  and `["profiles"]` so the global feed's prefix invalidation reaches them; chats have their own
  single hook (`useChats`) shared by the Chat panel, attention and the Graph. No view builds its
  own key or parses these responses; a Vitest case keeps every module's query keys distinct.

- **U54. Injected messages.** A user-role message that carries `sender` (`{chatId, chatTitle,
  kind}`, docs/CHAT_MODEL_API.md) was put into this chat by another chat, not typed by the human,
  and renders as its own kind of block: left-aligned, tinted with an accent edge, headed "from
  <chatTitle>" where the title is a chip linking to `/chat/<sender.chatId>`, plus a kind label
  (`message`, `reply`, and `task` for a `spawn`). The model-facing preamble line the hub puts first
  (`[Message from chat "..." ...]`) is not repeated in the block. It has no **Edit** and no
  **Regenerate** (U8); **Branch here**, the `‹ n/m ›` arrows and copy remain. The stream reducer
  keeps `sender` through `message_done` so a message injected while the chat is open renders the
  same way, and a `chat` event refetches the open chat's messages so one injected while it is idle
  appears without a reload.
- **U55. Chat settings.** The header **Settings** button opens the same form as the New chat dialog
  (U50, one component), filled with the chat's title, prompt and client, and saves with
  `PATCH /api/chats/{id}` sending only what changed: `title`; `clientLabel` (an offline current
  client stays selectable, greyed); and, when the prompt changed, `profileId` (a prompt id, or
  `null`), plus `systemPrompt` (the text for Custom, empty for None) when it is `null`. A chat
  with no profile opens on Custom (with its text) or None according to
  `GET /api/chats/{id}/system-prompt`. Nothing changed means nothing is sent. The prompt viewer,
  tools panel and chips refetch after a save.
- **U56. Sub-chats.** A chat spawned by another (`switchboard.chat.spawn`, or **Spawn sub-chat** in
  the Graph, U18) has `parentChatId` and is shown nested under its parent (U37) rather than in a
  group of its own, inheriting the parent's client, capabilities, approval mode and prompt
  reference (docs/CHAT_MODEL_API.md). Deleting a parent deletes them (U52).

### 8.2 Graph view

- **U13. Canvas.** React Flow. Nodes are the *visible* chats (U31) laid out as a tree (children
  below parents, via ELK or dagre); a node exists for every run record that has a chat (the graph
  data is still `/api/graph`, joined to `GET /api/chats` by `agentId`, one chat per record); edges
  are communication edges, drawn only when both ends are visible. Pan/zoom/fit, and on mobile a pinch-zoom canvas
  with the detail panel as a bottom sheet.
- **U14. Node content.** The chat's title, model chip, status dot (`idle`/`running`/`waiting`/
  `blocked`/`done`/`error`), last activity age, live token/cost counters, unread mailbox badge, and
  a depth indicator. A running chat pulses; the node of a currently-streaming run shows its latest
  tool call. Selecting a node opens its panel, whose **Open chat** action links to `/chat/<id>`.
- **U15. Edge editing.** Click an edge to toggle `allowed`; drag between **any** two nodes to
  connect them — not just parent to child (D14) — which is how two unrelated chats end up able to
  talk directly. The parent-to-child structural edge is drawn distinctly from communication edges
  and, being immutable (D14), is not editable or deletable from the graph at all (delete the
  sub-chat instead, which cascades, D13).
- **U16. Permissions panel.** Selecting a chat opens a panel listing every `ServerRef` it may
  use, joined against the live registry so offline ones are greyed with a "not connected" note
  (I3, A15). Toggles write through `PUT /api/agents/{id}/grants` and show the inherited/explicit/
  human source (A13). A toggle that would widen beyond the parent warns before applying. The panel
  also edits the chat's run settings (model, budget, "can create sub-chats", "can message other
  chats", auto-wake, approval), sending only what changed; its title, prompt and client are the
  chat's own settings (U55). **Delete chat...** confirms, names how many sub-chats go with it, and
  deletes through `DELETE /api/chats/{id}`.
- **U17. Real time without polling.** The view subscribes to the global feed (7.3) and refetches
  `/api/graph` on `agent`/`graph` events, so conversations appear and disappear (U31) as runs
  start and finish; the viewport is refit only on first data, when the canvas goes from empty to
  non-empty, and when the filter is toggled, never on every status change. There is **no TTL cache**: the earlier draft's 5-second
  cache would add staleness to a system that already has push invalidation. If `/api/graph`
  becomes expensive, it is memoised in-process and invalidated on write — never expired by time.
- **U18. Spawn sub-chat dialog.** **Spawn sub-chat** on a chat's panel creates a child through
  `POST /api/chats` with `parentChatId`: a title, a model (the parent's, preselected) and an
  optional prompt of its own (empty follows the parent's). The hub gives it the parent's client
  and grants (A16-A22); the dialog no longer lists grants, since nothing in the request can widen
  them. The Graph has no control that creates a root chat; that is the Chat panel's job (U50).

- **U31. Running only by default.** A *conversation* is a chat tree (a root chat and all its
  sub-chats). By default the graph shows only trees in which at least one live chat is
  `running`, `waiting` or `blocked`; idle/done/error trees and their edges are hidden, and
  deleted chats never count. The pure rule is `visibleAgents(nodes, selectedId, runningOnly)`.
- **U32. Toggle and persistence.** The toolbar has a "Running only" / "All chats" switch, on by
  default and remembered in localStorage (`mcpsb.ui.v1`, key `graph.runningOnly`). Next to it a
  count reads "N running · M hidden". When nothing runs, the canvas says "No chats are
  running. Show all chats to browse older ones." with a button that switches to "All chats".
- **U33. Selected-tree exception.** The tree of the chat selected via `/graph/:agentId` stays
  visible even when idle, so a deep link or selection never vanishes, and is marked "not
  running". Deselecting lets it be filtered out again.

### 8.3 Prompts tab

- **U22. Prompts.** The `/prompts` route lists saved prompts (profiles on the wire; default first;
  the selected one is `/prompts/:profileId`, U28) and creates, edits, deletes
  and re-defaults them through `/api/profiles*`, refetching on `profile` events. The editor has
  name, description, system prompt, model, the two hub-tool capabilities (`canSpawn`, `canMessage`,
  worded "Can create sub-chats" and "Can message other chats"), approval mode and budget; it has
  **no MCP client, server or grant controls**, because a prompt has none (A16). Chats follow their
  prompt live, so editing one changes them. Deleting the default or the only prompt is disabled
  with the reason shown.
- **U23. Hub tools.** The Connections view has a "Hub tools" section listing the `switchboard.*`
  tools from `GET /api/hub-tools` (`switchboard.chat.*` and the rest, named by the API) with the
  capability each requires, so an operator can see what ticking "Can create sub-chats" or "Can
  message other chats" on a prompt actually grants (M1). These are not a client and
  are never listed under one (I7).
- **U29. Model pickers show what the hub knows.** Every model picker (prompt form, chat run settings,
  sub-chat dialog, chat composer) labels each option `provider/model · 32k ctx` when the model's
  `contextWindow` is known (32768 -> `32k`, 1048576 -> `1M`) and appends `no tool support` when
  `supportsTools === false`; such a model stays selectable but a line under the picker says "this
  model does not advertise tool calling". Models with `discovered: true` are grouped under a
  "Discovered" heading when declared ones are also listed. All fields are optional and absent ones
  render nothing.
- **U30. Truncation is visible.** When a run's usage says `truncated: true` (the prompt filled the
  model's context window), the chat shows a non-modal warning in the thread, with the window size
  when `contextWindow` is present, advising a larger context or fewer tools. Usage lacking these
  fields never breaks the budget meter.

### 8.4 Remembering where you were

The requirement is that clicking away from the chat to inspect a connection or a tool, and back,
reopens the same chat, and likewise in every other panel. It is met **in the browser only**: the
hub stores nothing for it and there is no API. (Composer drafts are the one server-side exception,
N9, because they are user content rather than navigation.)

- **U24. Browser storage, versioned, never required.** UI memory lives in `localStorage` under the
  namespace `mcpsb.ui.v1.*`; each value is `{v: 1, d: …}` JSON. Every access is wrapped: a missing
  or throwing `localStorage` (private windows, blocked site data), corrupt JSON, a different `v`,
  or a value that fails validation each read as "nothing remembered", and a failed write is
  ignored. The console behaves identically without it, and nothing is ever remembered on the
  server. Changing a stored shape bumps `v1`, orphaning old data rather than migrating it.
- **U25. Per-tab route memory.** For each top-level tab (`/connections`, `/calls`, `/endpoints`,
  `/chat`, `/graph`, `/prompts`) the console remembers the last full location visited under it
  (path and query, minus one-shot `args`). A tab's nav link goes to its remembered location, so
  *Chat* reopens `/chat/<lastChatId>` and *Graph* `/graph/<lastAgentId>`; **clicking the tab you
  are already on goes to its bare root**, which is how you deselect (and is then what is
  remembered). A remembered location that is not under its own tab is ignored. `/` redirects to
  the most recent remembered location, defaulting to `/connections`, so reloading the bare origin
  puts you back; a **deep link typed into the URL bar always wins** and is never rewritten from
  memory. Memory written when the tab was still called Agents (`/agents`, `/agents/<id>`, as a tab
  key, a location or `last`) is read as the same place under `/prompts`, so an upgrade loses
  nothing.
- **U26. UI-only state is remembered per view.** Connections' filter text, the chat list's
  search text and archived toggle, and which chat-list nodes are collapsed (U37) are stored under
  their own keys. State that is merely visual has no memory (scroll and the graph's pan and
  zoom are not restored; the graph refits on load).
- **U27. Dead ids fall back cleanly.** A remembered or linked chat that answers `404`, or a chat
  or prompt absent from the first list the view loads, sends the view to its bare tab with
  `replace` and drops that tab's memory (and `/` memory that pointed into it). The check lives in
  the views' own queries: the shell makes **no request** to validate memory. An id created moments
  ago is never checked against a list that predates it.
- **U28. The URL is the source of truth for what identifies a resource.** Open chat
  (`/chat/:chatId`), selected graph chat (`/graph/:agentId`), selected prompt
  (`/prompts/:profileId`; only the unsaved "new prompt" form is local state), the Connections tool
  (`?connection=&server=&tool=`) and the Calls filters and selected row (`?label=&…&call=`) are
  all in the URL, which makes the route memory of U25 sufficient for them and keeps every state
  linkable. `localStorage` holds only what the URL cannot.

### 8.5 Clients and their environment

- **U40. The client badge.** Wherever the console shows an MCP client it uses one shared
  component: `label · project` (project muted; no separator when the client reported none) and one
  small chip per detected environment kind (`devcontainer`, `container`, `direnv`, `nix-shell`
  with `(pure)`/`(impure)` when reported, `venv`), each with its own subtle tint. Its tooltip
  carries the workspace path and the small `details`; its `aria-label` summarises client, project
  and kinds. Long names truncate rather than overflow, so it holds at 375 px. The point is that a
  client in the wrong place (a dev container instead of the host, the wrong direnv or devshell)
  is visible before it is picked.
- **U41. Connections shows it everywhere.** Every machine in the Connections tree carries the
  badge, and the tool detail header shows it with the workspace path. The filter also matches
  the project, the workspace and the environment kinds (typing `devcontainer` or a project name
  narrows the tree). A client that reports no `environment` (an older client) shows no chips and
  a subtle "environment not reported", never a warning.
- **U42. Picking a client uses the same badge.** Every place a client is chosen or its chats are
  grouped (the New chat dialog and chat settings' client picker, the Chat panel's client groups)
  shows the same badge, fed from `GET /api/connections`; a client that is not currently connected
  shows its label only.

### 8.6 Installability (PWA)

The console is installable as a Progressive Web App — most relevantly to a phone home screen,
where it should open full-screen, survive a browser restart, and be reachable without hunting for
a tab.

- **U60. Web app manifest.** `GET /manifest.webmanifest` (served by the console's static host, not
  the API) declares `name`/`short_name`, `start_url: "/"`, `display: "standalone"`,
  `theme_color`/`background_color` matching the console's dark/light defaults, and an icon set
  (192px and 512px, plus a maskable variant) so Android and iOS both offer "Add to Home Screen"
  with a proper icon.
- **U61. Service worker.** A minimal service worker is registered on load and does **not** attempt
  offline use of live data (chats, the graph, calls are all realtime and meaningless stale) — no
  app-shell caching strategy beyond the static JS/CSS bundle, so the installed app always shows
  current data the moment it has a connection. Its other job is push (§8.7). Registration failure
  (unsupported browser, denied) degrades silently to the ordinary web app; installability is a
  bonus, never a requirement.
- **U62. Install prompt.** The console listens for `beforeinstallprompt` on Chromium and surfaces a
  small, dismissible "Install app" affordance (not a modal) rather than the bare browser default;
  iOS Safari has no such event, so there the same affordance instead shows the manual "Share → Add
  to Home Screen" steps when the UA looks like iOS Safari and the app is not already installed
  (`navigator.standalone`).

### 8.7 Push notifications

The two events worth interrupting the user for on a phone they are not looking at: **a run
finished** (so they can read the answer) and **a run needs human approval** (§5.8, it is otherwise
just sitting `blocked`). Everything else stays in-app.

- **Web Push, standard and self-hosted.** No third-party push service beyond the browser
  vendor's own (FCM/APNs/Mozilla push endpoints, reached only by the browser, never by the hub
  directly) — the hub holds its own VAPID key pair (`PUSH_VAPID_PUBLIC_KEY`,
  `PUSH_VAPID_PRIVATE_KEY`, generated once and kept in the deployment's secrets, §12) and speaks
  the Web Push protocol directly. No accounts, no external SaaS.
- **`push_subscriptions`** (new table, §6.1): `id`, `endpoint` (unique), `p256dh`, `auth` (the
  subscription's keys), `created_at`, `last_seen_at`, and an optional `label` (browser/device,
  read from `navigator.userAgent` at subscribe time, shown in Settings so a stale phone can be
  removed). Not scoped to an agent or chat — this is a single-operator system (§10.1) and every
  subscription gets every notification.
- **Routes:** `POST /api/push/subscribe` `{subscription, label?}` → `{id}` (upsert on `endpoint`);
  `DELETE /api/push/subscriptions/{id}`; `GET /api/push/vapid-public-key` (so the console never
  needs the key build-time-baked). All 404 when push is not configured (no VAPID keys set).
- **Trigger points.** The hub sends a push, fire-and-forget (a delivery failure or an expired
  subscription — HTTP 404/410 from the push service — just removes that subscription, logged, never
  retried or surfaced as an error) on exactly two transitions: a run reaching `done`/`error` for a
  **top-level human chat** (not on every descendant's completion — a spawned sub-chat finishing is
  not interrupt-worthy, its parent chat is what the human is watching) and a run entering `blocked`
  on approval (§5.8). Payload: `{title: chatTitle, body: preview, chatId, kind: "done" | "error" |
  "approval"}`, kept under the ~4 KB push payload budget.
- **Service worker `push` handler.** Shows a `Notification` from the payload; `notificationclick`
  focuses an existing console tab/window if one is open (`clients.matchAll` +
  `client.navigate`/`focus`) and otherwise opens `/chat/<chatId>`, so tapping the notification
  always lands on the chat in question.
- **Subscribing.** A Settings toggle ("Notify me on this device") requests
  `Notification.requestPermission()` and, on grant, `pushManager.subscribe` with the VAPID public
  key, then posts it to the hub. Denial or an unsupported browser hides the toggle behind a note,
  never a broken control.

## 9. Harness server

### 9.1 General behaviour

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
  confined; only their `cwd` is. See section 10.
- **S4. Errors.** Expected failures are reported one of two ways, by tool family:
  - The *typed file tools* (9.2) report them **in the result** (`success`, `deleted`, `moved` = `false`
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
  confirm risky calls (9.5).

### 9.2 Typed file and shell tools

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
| out `truncated` | boolean | more remains; call again with a larger `offset`. **This is the only valid end-of-file test**: `offset` and `limit` count characters in text mode while `size` is always bytes, so `offset + len(raw_text) == size` does not hold for non-ASCII files |

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

### 9.3 Search and edit tools

| Tool | Arguments | Returns |
|---|---|---|
| `tree_of_files` | `root="."` | nested dict: subdirectories by name, files under `"__files__"`; skips `.git`, `node_modules`, `__pycache__`, `.venv` |
| `find_files` | `pattern`, `root="."`, `limit=1000` | sorted root-relative paths of files whose path or name matches a glob such as `**/*.py`. Inside a git repository `.gitignore` is honoured (tracked plus untracked-not-ignored files); otherwise the directories above are skipped |
| `ripgrep` | `query`, `path="."`, `glob=None`, `ignore_case=false`, `context=0`, `max_results=200` | `{matches: [{file, line_no, text, is_match}], truncated}`; `context` adds up to 20 surrounding lines per match (`is_match: false`); `truncated` when `max_results` was hit; requires `rg` |
| `read_lines` | `path`, `start=1`, `end=None` | lines `start..end`, 1-based inclusive |
| `edit_file` | `path`, then either `new_content`, or `old_str` + `new_str` (+ `replace_all=false`) | confirmation. `old_str` must occur **exactly once** unless `replace_all`; zero matches, several matches without `replace_all`, an empty `old_str`, or a missing file is an error, so the wrong spot is never edited silently |
| `run_python` | `code`, `timeout=120` | `{exit_code, stdout, stderr, truncated}` from a fresh interpreter run in the root; output capped. Field names and the `timeout` default match `run_command` deliberately — the two are siblings, and a model that has learnt `exit_code` on one should not have to retry against `returncode` on the other. (The current implementation returns `returncode`; renaming it is part of this revision.) |

`run_bash` and `list_dir` were removed in favour of `run_command` and `dir_list` (fewer, sharper tools
choose better). `tree_of_files` stays because its nested shape is different.

### 9.4 Git tools

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

### 9.5 Background processes

For work that outlives one call (dev servers, watchers, slow builds).

| Tool | Arguments | Returns |
|---|---|---|
| `process_start` | `command`, `cwd=None`, `env=None` | `{id, pid}`; at most 16 at once, all killed when the harness exits |
| `process_read` | `id`, `wait=0` | `{id, stdout, stderr, running, exit_code, dropped}`: only output produced since the previous read; `wait` (max 30 s) pauses until exit or timeout first; `dropped` means older output overflowed the 1,000,000-character buffer |
| `process_kill` | `id` | `{id, killed, message}`; kills the whole process group; unread output stays readable |

Finished, fully-read processes are forgotten when a new one starts. An unknown `id` is an error.

### 9.6 Tool annotations

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


## 10. Security model

### 10.1 What was already true

The tunnel token is the only credential on the public listener. The private listener relies on not
being reachable, optionally hardened with `MCP_SWITCHBOARD_PRIVATE_TOKEN`. The install command
shown in the console contains the tunnel token, which is why it renders only on the private
listener.

Anyone who can call a hub's `/mcp` endpoints can use the harness on every connected machine. The
harness's root confinement stops file tools escaping by mistake or by path trick, and annotations
let well-behaved clients ask before destructive calls, but neither is a security boundary:
`run_command` can do whatever the client's user can. **Access to the private listener is shell
access to every machine running a default client.**

### 10.2 What the orchestrator changes

- **X1.** The sentence above now reads: *an LLM has shell on every connected machine.* The hub
  autonomously issues `run_command` against real machines, driven by text that may include tool
  output, file contents and web pages. Prompt injection is therefore a remote-code-execution
  vector, not a content problem.
- **X2. Grants are the only real control**, and they are coarse (server-level). The practical
  advice is: **do not grant the harness to an agent that also reads untrusted input.** A research
  agent gets `fetch`; a coding agent gets `harness` on one machine; they are different agents
  connected by an edge, and the edge carries text, not capability.
- **X3. Approvals (§5.8) are a usability feature, not a boundary.** They depend on annotations
  supplied by the upstream server, which the hub does not verify. They stop accidents; they do
  not stop an adversary.
- **X4. The blast radius of a compromised hub grew.** It now holds LLM provider API keys and the
  full text of every conversation, including whatever files agents read. Backups of
  `/var/lib/mcp-switchboard` are now sensitive.
- **X5. No user model.** There is exactly one principal on the private listener. Multi-user
  auth, per-user chat ownership and audit-by-user are out of scope (§13) — which means
  `PRIVATE_TOKEN` is a shared password, and everyone who has it is the same person as far as the
  hub is concerned.
- **X6. Recommended posture.** Keep the private listener on loopback and reach it over Tailscale
  or an SSH tunnel. Run clients as a dedicated unprivileged user. Use `--no-harness` on any
  machine where shell access for the hub's operator is not acceptable. Set a tree-wide
  `max_cost_micros` before enabling spawning, because a runaway agent tree is a billing incident
  before it is anything else.
- **X7. Secrets handling.** Provider keys follow the existing rule: a value starting with `/` that
  names an existing regular file is replaced by its contents, so sops-nix and systemd
  `LoadCredential` need no special support. Keys are never returned by any endpoint, never
  written to the call log, and redacted from Loki payloads.
- **X8. Model-supplied identifiers are never trusted.** A chat id or name (`to`, `chat_id`, …) in a
  `switchboard.*` call is authorised against the *calling* chat's identity, which the hub knows from the session's scope
  (`/mcp/agent/{id}`), not from the arguments. A tool call cannot assert who it is *in its
  arguments* — but the scope URL does assert it, and that is the whole of the authentication. An
  agent (record) id is therefore a **capability**: anyone who can reach the private listener and knows (or
  guesses, though uuidv7 makes that impractical) an id can act as that agent, including one holding
  `(*, *, *)`. This is why `/mcp/agent/{id}` is internal to the run loop (§5.5) and why X6's
  "keep the private listener on loopback" is not optional advice once agents are enabled. If Q2 is
  ever answered yes, `PRIVATE_TOKEN` must become mandatory under `AGENTS_ENABLED=true`.
- **X9. One client per chat narrows the blast radius by construction.** X2's advice — do not give
  an agent that reads untrusted input the harness of a machine it should not touch — used to depend
  on the operator assembling grants correctly. A chat holds
  `(client, *, *)` for the one client chosen for it, or nothing, (A19), and every
  sub-chat it spawns holds a subset (A12, M3). Prompt injection in its chats can therefore reach
  the servers of one machine, not the whole fleet. This is still coarse — every server of that
  client, harness included — and approvals remain advisory (X3); pick the client accordingly.

## 11. Observability

### 11.1 Cardinality rule

**Label values must be bounded and come from configuration.** `label`, `project`, `server`, `tool`,
`status`, `source`, `provider` and `model` qualify. `agent_id`, `chat_id`, `run_id`,
`connection_id` and error text **do not** and must never be Prometheus labels.

> The earlier agent draft put `agent_id` and `chat_id` on every metric. With agents spawning
> agents that is unbounded cardinality by construction, and it would take the hub's own
> `/metrics` endpoint down before it took anything else down. Per-agent numbers belong in the
> store and are served by `/api/stats` and the Graph view; Prometheus gets aggregates.

### 11.2 Series

Existing: `mcpsb_connections_active`, `mcpsb_servers{state}`, `mcpsb_tools_total{label,server}`,
`mcpsb_tool_calls_total{label,server,tool,status,source}`,
`mcpsb_tool_call_duration_seconds{label,server,tool}`, `mcpsb_tunnel_frames_total{direction}`,
`mcpsb_loki_dropped_total`.

New:

| Metric | Type | Labels |
|---|---|---|
| `mcpsb_agents_total` | gauge | `status`, `project` |
| `mcpsb_agent_spawns_total` | counter | `project` |
| `mcpsb_runs_active` | gauge | `project` |
| `mcpsb_runs_total` | counter | `project`, `status`, `finish_reason` |
| `mcpsb_run_duration_seconds` | histogram | `project` |
| `mcpsb_agent_turns_total` | counter | `project`, `provider`, `model` |
| `mcpsb_llm_tokens_total` | counter | `provider`, `model`, `kind` (`input`/`output`/`cache_read`/`cache_write`) |
| `mcpsb_llm_cost_micros_total` | counter | `provider`, `model` |
| `mcpsb_llm_request_duration_seconds` | histogram | `provider`, `model` |
| `mcpsb_llm_errors_total` | counter | `provider`, `model`, `kind` |
| `mcpsb_agent_messages_total` | counter | `project` |
| `mcpsb_agent_message_delivery_seconds` | histogram | `project` |
| `mcpsb_approvals_total` | counter | `outcome` (`approved`/`denied`/`timeout`) |
| `mcpsb_stream_clients` | gauge | `kind` (`global`/`chat`) |
| `mcpsb_store_rows` | gauge | `table` |

- **O1.** `source` on `mcpsb_tool_calls_total` gains the value `agent`; `status` gains `denied`.
  Denials are counted there and nowhere else — a separate `..._denied_total` series would carry the
  same numbers under a second name and the two would eventually disagree.
- **O2. Fixed-domain series are pre-created at zero** so a fresh hub scrapes as zeroes rather than
  absent series, which would make `rate()` and alerts silently no-op until the first event.

### 11.3 Logs

- **O3.** Structured logs (`log/slog`, JSON) to stderr. The optional Loki exporter keeps the
  current contract: bounded queue, drops and counts drops, never blocks or fails a tool call.
- **O4.** New Loki event kinds: `run_started`, `run_finished`, `agent_spawned`, `agent_message`,
  `grant_changed`, `approval`. Each carries `agentId`/`runId` as *fields* (Loki handles high
  cardinality in the body; only Prometheus labels are constrained).
- **O5. No message content in logs.** Prompts and completions stay in the store. An operator who
  wants them reads the chat.

## 12. Deployment and build

- **E1.** `go build ./cmd/mcp-switchboard-hub` produces a static binary (`CGO_ENABLED=0` using a
  pure-Go SQLite driver, so the container stays `FROM scratch`-able and the Nix build needs no C
  toolchain). The driver is **`modernc.org/sqlite`**, pinned by name because the console's chat
  search (U6) needs FTS5 and pure-Go drivers differ on whether they ship it; V5 asserts an FTS5
  query works, so a driver swap that drops it fails CI rather than surfacing as an empty search
  box.
- **E2. Dockerfile** is three stages: a node stage building the console (U1), a Go stage building
  the binary against that output, and a minimal runtime stage with a non-root user and the
  `/var/lib/mcp-switchboard` volume. The runtime stage is `FROM alpine` rather than `scratch`: a non-root user
  needs `/etc/passwd`, the volume needs an owner, TLS to Loki or an LLM provider needs a CA bundle,
  and the healthcheck needs a shell. That is ~8 MB for four reasons, against a 26 MB image.
  The build stage is pinned to `--platform=$BUILDPLATFORM` and cross-compiles via `GOARCH`, so an
  arm64 image emulates only `apk add` in the runtime stage rather than a whole compile — QEMU is
  still set up in CI, but it costs seconds rather than minutes. The documented caveat stays: **the
  hub binds `127.0.0.1` by default, which inside a container means reachable from nothing** — set
  `TUNNEL_HOST`/`PRIVATE_HOST` to `0.0.0.0` or use `--network=host`.
- **E3. Nix.** `packages.hub` is `buildGoModule` (`nix/hub.nix`), with **no `buildNpmPackage`**:
  the committed `web/dist` means the Nix build needs one toolchain and no npm fetch, which is also
  what keeps it buildable offline. Its source is narrowed with `lib.fileset` to the files the build
  reads, so editing a test fixture does not invalidate the derivation. `packages.client` and
  `packages.harness` stay on uv2nix, and the overlay hands out all three. The Python hub is no
  longer packaged; it stays a uv workspace member only so its tests keep running until it is
  deleted.
- **E4. NixOS module** keeps its option surface — every setting is still a `MCP_SWITCHBOARD_*`
  environment variable with the same path-substitution semantics — so the rewrite changed almost
  nothing in it: the package default, and `logLevel`, which now takes slog's names and keeps
  Python's `WARNING`/`CRITICAL` as accepted aliases so an existing configuration does not silently
  drop to INFO. `DynamicUser=true` and the existing hardening stay. It gains
  `services.mcp-switchboard.llm.{provider,apiKeyFile,modelsFile}` plus `agents.enable` when the
  orchestrator lands.
- **E5. CI.** New jobs: `go test ./...` with `-race`, `golangci-lint`, `npm ci && npm run build &&
  npm test` for the console, and a `dist/` freshness check. Existing Python jobs (client, harness)
  are unchanged; the `hub (py…)` job is removed and the end-to-end job builds the Go binary first.
- **E6. Config additions** (all `MCP_SWITCHBOARD_*`): `AGENTS_ENABLED` (default `false` — the hub
  is a gateway first and must keep working with no LLM configured), `LLM_MODELS`,
  `LLM_<PROVIDER>_API_KEY`, `LLM_<PROVIDER>_BASE_URL`, `LLM_OPENAI_COMPATIBLE_KIND`, `LLM_OPENAI_COMPATIBLE_DISCOVER`, `LLM_OLLAMA_NUM_CTX`, `AGENT_MAX_DEPTH`, `AGENT_MAX_CHILDREN`,
  `AGENT_MAX_CONCURRENT_RUNS`, `AGENT_MAX_PARALLEL_TOOL_CALLS`, `AGENT_DEFAULT_BUDGET` (including
  `max_lifetime_cost_micros`, B4), `AGENT_DEFAULT_GRANTS`, `AGENT_REPLY_TIMEOUT`,
  `APPROVAL_TIMEOUT`, `INBOX_RETENTION_DAYS`, `SHUTDOWN_GRACE`. These are the only names for these
  limits; §5 refers to them exactly as spelled here.
- **E7.** With `AGENTS_ENABLED=false` the hub behaves exactly as it does today: no `switchboard.*`
  tools, no `/mcp/agent/*` scope, no profile routes, no Chat, Graph or
  Agents views in the console, no LLM dependency at runtime.

## 13. Non-goals

Unchanged: non-stdio upstream transports; inbound connections to clients; sandboxing tool
execution; application-level heartbeats (WebSocket ping/pong only — see PROTOCOL.md).

Added for this revision:

- **Durable run resumption.** Runs die with the hub (A3). Checkpointing a partially-completed
  turn across restarts is a large problem and is not solved here.
- **Multi-user auth and per-user data.** One principal per hub (X5).
- **Per-tool permission granularity.** Grants are server-level (A10).
- **Horizontal scaling.** One hub process owns the registry, the runs and the SQLite file. The
  `Store` interface makes a multi-replica future *possible*, not supported.
- **RAG, vector stores, long-term agent memory.** An agent's context is its chat.
- **Agent-authored agents beyond the tree.** An agent can only create descendants and can only
  act on its own subtree.

## 14. Testing

- **V1.** Go unit tests per package, run with `-race`. `internal/registry` scope composition and
  `internal/agents` grant resolution (A12–A15) get table-driven tests, because those are the two
  places where a subtle bug is a security bug.
- **V2.** Python unit tests per package unchanged (`client/tests`, `servers/harness/tests`).
- **V3. Protocol conformance (P2/P4).** A test in each language asserts its constants against
  `docs/protocol.json`; the end-to-end suite runs the real Python client process against the real
  Go hub binary with real MCP servers and calls tools both through the API and as an MCP consumer.
- **V4. Envconf parity (P3).** Both languages run `docs/envconf-cases.json`.
- **V5. Store tests** run against a temp SQLite file: migrations apply to an empty file and are
  idempotent on a second open, a database carrying a `calls` table with no `schema_migrations` is
  refused with D7's message, and an FTS5 query asserts the driver of E1 supports the chat search of
  U6. Migration 002 is tested twice over: it applies to an empty file (the version assertion is
  now 2), and a database migrated only to 001 and holding an agent and a chat upgrades to 002 with
  both rows intact and the new columns null. Profile tests cover the single-default invariant
  (the schema itself refuses a second default), atomic default moves, name clashes, and deletion
  nulling `profile_id` on chats and agents.
- **V6. Orchestrator tests** use a scripted fake provider (a deterministic `llm.Provider` that
  replays a canned sequence of deltas and tool calls), so run-loop, budget, cancellation, approval
  and mailbox behaviour are tested with no network and no cost. Two cases are named explicitly
  because they are the failures this revision exists to prevent: **(a)** two agents with a
  symmetric allowed edge, each answering the other, terminate on the lifetime cost ceiling (B5) in
  bounded time — the A9 regression test; **(b)** a tree of `AGENT_MAX_CONCURRENT_RUNS + 4` agents
  all blocked in `send` with `wait: true` still makes progress, proving waiting runs hold no slot
  (R7). Profile tests add: the default `Assistant` is seeded exactly once; a manual (or legacy
  profile-created) agent holds exactly `(client, *, *)` even when `AGENT_DEFAULT_GRANTS` is `(*, *, *)` (A19); a
  spawned child inherits capabilities, approval, model and profile and records the parent chat's
  client, may only narrow capabilities and approval, and **can never reach another client even when
  asked for `(other, *, *)` or `(*, *, *)`** (A12, M3); and `ChatTools` returns exactly the run
  loop's catalog (A21). API tests run the profile routes against a fake `Service` for error mapping
  and against the real Manager and SQLite end to end.
- **V7. Console tests:** Vitest for hooks (branch navigation over the message DAG, stream
  assembly and resume-by-`seq`), Playwright for one end-to-end chat flow against a hub with the
  fake provider, run at a mobile viewport as well as desktop.
  The browser memory of 8.4 has Vitest cases for the storage wrapper (corrupt JSON, throwing
  storage, version mismatch, unknown ids) and the pure `nextLocationForTab`, and a Playwright spec
  (desktop and mobile) for chat/tool memory across tabs, restore on `/`, a deleted chat, and the
  hover archive of U6a. Chat-panel specs (desktop and mobile) cover creating an agent (profile and
  client; None and None), a chat under it, the client -> agent tree, easy delete (U52) and the
  system prompt viewer (U51), with `page.route` mocks where the hub build may lag.
- **V8. NixOS VM test** extended to assert the console is served and `/api/agents` answers.

## 15. Migration plan

Ordered so that the tree is working at every step and nothing is rewritten twice.

1. **Freeze the wire.** Extract `docs/protocol.json`; add the Python-side conformance test
   (P1, P2). No behaviour change.
2. **Remove `server/`.** A stale duplicate of `servers/harness`, never tracked by git; its Python
   sources are already gone and only `server/__pycache__` remains on disk. Delete the directory and
   add it to `.gitignore`'s siblings check so it cannot come back as a shadow of `servers/`.
3. **Go hub at parity**, no agents: tunnel, registry, sessions, all five non-agent scopes, calls,
   API, metrics, Loki. Starts with `git mv hub legacy/hub` so the Go tree can occupy the `hub/`
   paths this document uses throughout (§4.2); `legacy/hub` stayed importable and testable until
   step 10 deleted it. Gate: the existing end-to-end suite passes against the Go binary
   unmodified.
4. **Console rewrite at parity**: Connections, Calls, Endpoints in React, mobile-ready. Gate: a
   Playwright run covering what the old console did.
5. **Store** for the new entities (§6), with no orchestrator behind them yet. Migration 001 is
   written once, at step 3 and grew until a development database at version 1 held real data; it
   is frozen now (D6), and the profile change (§5.2a) is `002_profiles`, the agent origin and client `004` (D12), chats as the unit `005` (D13).
6. **Orchestrator core**: agents, grants, run loop, fake provider, `/mcp/agent/{id}`. Gate: V6.
7. **Chat view** with streaming and branching (§8.1, §7.4).
8. **Graph view** with edges and permission editing (§8.2), plus the `switchboard.chat.*` tools.
9. **Real providers**, cost table, budgets, approvals.
10. **Cut over** deployment: Dockerfile, flake, NixOS module, CI (§12). Retire `legacy/hub` (done), and
    delete any existing `calls.db` rather than migrating it: the Go hub refuses a pre-migrations
    database with D7's message.

Steps 1–2 are worth doing now regardless of whether the rest proceeds.

## 16. Open questions

Not blocking the design, but each one changes an implementation detail:

- **Q1.** Should an agent's chat be visible to its parent? Currently a parent sees only the
  replies it receives, not the child's internal transcript, while the console sees everything.
  If parents should be able to read a child's chat, that is a new `switchboard.chat.read` tool
  and a new way for context to leak upward.
- **Q2.** Should `/mcp/agent/{id}` be reachable by *external* consumers (so Claude Code could
  borrow an agent's exact permission set), or only internally by the run loop?
- **Q3.** Attachments: content blocks in SQLite, or a blob directory with hashes? Images in a
  conversation will dominate database size fast.
- **Q4.** Does an agent get one harness root per agent (requiring per-agent client instances or a
  harness change) or do all agents on a machine share one root? Today they share, so two agents
  on one machine can overwrite each other's work and share the 16-process cap (§9.5). This is
  the biggest practical limitation of the current design and may deserve a `workdir` argument on
  the harness tools.
- **Q5.** Prompt caching: do we pin a stable tool-catalog ordering and system prompt prefix per
  agent to keep Anthropic/OpenAI caches warm across turns? T1 assumes yes; it needs measuring.
*Resolved since the previous draft:* **Q6** (blocking `send`) — `wait: true` stays, but a waiting
run releases its concurrency slot (R7), which was the actual problem: without that, a tree wider or
deeper than `AGENT_MAX_CONCURRENT_RUNS` deadlocks, every slot held by a parent waiting on a child
that can never be scheduled. The parent's `max_wall_seconds` still runs while it waits, which is
what `AGENT_REPLY_TIMEOUT` bounds.
