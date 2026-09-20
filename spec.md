# mcp-switchboard specification

Behavioural and technical spec for the whole system. The tunnel wire format is specified
separately in [docs/PROTOCOL.md](docs/PROTOCOL.md); this document covers everything around it.

## 0. Status of this revision

Three changes land together, because each one forces the others:

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
| Console | `hub/web/` | TypeScript | Single-page app embedded in the hub binary. Connections, Calls, Endpoints, **Chat**, **Graph**. |

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
- **C7. Lifecycle.** Sends `hello` (including each server's `project` when set), then supervises
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
  | `/mcp/agent/{agent_id}` | agent | `label__[project__]server__tool`, filtered to that agent's grants, plus `switchboard.*`. **Internal to the run loop** — not offered to external consumers (§5.5, Q2) |

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

Paths below are final. The Python hub is moved to `legacy/hub/` in one `git mv` at the start of
step 4 of §15, so `hub/` means the Go tree from that point on and nothing in this document has to
be read as provisional. `legacy/hub/` is deleted at step 11.

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

An **agent** is a durable, named configuration: a model, a system prompt, a set of tool grants,
and a place in a parent→child tree. It is not a process.

A **run** is one execution of an agent against one chat: the loop *prompt → model → tool calls →
model → …* until the model stops calling tools, a budget is exhausted, or it is cancelled. Runs
are the unit of concurrency, cancellation and metering.

A **chat** is a durable conversation owned by one agent, holding a **DAG of messages**.

- **A1.** Agents outlive runs. Killing a run does not delete the agent; deleting an agent
  cancels its runs and its descendants' runs.
- **A2.** Inference happens **in the hub process**. The hub holds provider API keys. This is the
  decision that makes the hub a stateful application rather than a gateway, and it is why §10 is
  substantially longer than it used to be.
- **A3.** Runs do not survive a hub restart. On startup, any run still marked `running` is marked
  `interrupted` with a message explaining why, and its chat remains readable and branchable.
  Durable resumption is explicitly **out of scope** for this revision (§13).

### 5.2 Entities

`agents`

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
| `agent_id` | uuid | the agent that owns and executes this chat |
| `peer_agent_id` | uuid, null | set when this chat is one side of an agent↔agent thread (§5.5) |
| `title` | string | auto-generated from the first user message, editable |
| `kind` | enum | `human` \| `agent` \| `spawn` |
| `active_leaf_id` | uuid, null | which leaf of the message DAG is the "current" conversation |
| `tags` | json array | |
| `token_total`, `cost_total_micros` | int | denormalised running totals, for list rendering without a join |
| `created_at`, `updated_at` | timestamptz | |
| `archived_at` | timestamptz, null | |

> A chat has exactly one *owning* agent. An agent↔agent exchange is **two chats**, one per side,
> linked by `peer_agent_id` — not one shared chat. This keeps each agent's context window its own
> and avoids a shared-mutable-history problem the moment the two sides branch independently.

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
| `model` | json, null | the exact model that produced an assistant message (may differ from the agent's current config) |
| `finish_reason` | string, null | `stop` \| `tool_use` \| `max_tokens` \| `budget` \| `cancelled` \| `error` |
| `run_id` | uuid, null | which run produced it |
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

`edges` — agent↔agent communication permissions:

| Field | Type | Notes |
|---|---|---|
| `from_agent_id`, `to_agent_id` | uuid | composite primary key |
| `allowed` | bool | |
| `created_at`, `updated_at` | timestamptz | |

- **A8.** A parent→child edge is created `allowed = true` on spawn. Any other edge defaults to
  denied; absence of a row means denied.
- **A9.** Cycles are permitted structurally — two agents may be allowed to message each other —
  but message loops are bounded by the budget rules of §5.6, not by the graph shape. This is
  deliberate: the failure this project exists to replace shipped a symmetric `heartbeat` that
  answered `heartbeat`, an infinite loop measured at ~80 msg/s (see PROTOCOL.md). A graph that
  permits cycles **must** have a termination argument that does not depend on the graph.

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
  value. `(*, *, *)` is "everything", which is what a root agent created from the console gets by
  default unless `MCP_SWITCHBOARD_AGENT_DEFAULT_GRANTS` says otherwise.
- **A12. Inheritance and the escalation rule.** On spawn, a child's grant set is the parent's,
  intersected with whatever the parent asked for. **An agent can never grant a child more than it
  holds itself.** This is enforced at every point where an agent *writes* a grant — spawn, and
  `switchboard.mcp.grant` — against the granting agent's set at that moment.
  At **call time the agent's own stored grant set is authoritative and is not re-derived from its
  ancestors.** Narrowing reaches descendants through A14's propagating transaction, which is the
  single mechanism for it; re-deriving at call time would additionally revoke the `human` grants
  that A13 and A14 exist to preserve, and the two rules cannot both hold.
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
  in parallel, because two runs appending to one `active_leaf_id` is a lost-update race.
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
  runs in `running` only. A run blocked on a peer's reply (`switchboard.agent.send` with
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
3. Compose names at `Scope.ALL` (`label__[project__]server__tool`) so names are stable regardless
   of what else is connected — an agent's prompt cache must not be invalidated because an
   unrelated machine dialled in.
4. Append the `switchboard.*` tools the agent is allowed to use (§5.5).
5. Drop names failing I4, and drop duplicates loudly.

- **T1.** Tool-set churn is expensive: it invalidates provider prompt caches. The hub therefore
  recomputes a running agent's catalog **only between turns**, never mid-turn, and a server that
  disappears mid-turn yields a `server not connected` tool error rather than a vanishing tool.
  Between turns it is recomputed only if a grant or the live registry actually changed, so a stable
  system re-sends a byte-identical tool block. Ordering is stable too, by composed name — see Q5.

### 5.5 Built-in `switchboard.*` tools

Exposed **only at `/mcp/agent/{id}`**, where the hub knows which agent is calling (X8). They are
never listed at `/mcp` or at the host/project/server scopes: those scopes have no principal, so
"the caller's children", "the caller's grants" and "the caller's mailbox" have no referent there.
Whether external consumers should be able to attach to `/mcp/agent/{id}` at all is Q2, and until it
is answered the scope is internal to the run loop. Every one of these is itself logged as a call.

| Tool | Input | Output | Behaviour |
|---|---|---|---|
| `switchboard.agent.spawn` | `{name, description, model?, system_prompt, grants?, budget?}` | `{agent_id, chat_id}` | Creates a child of the calling agent. `model` defaults to the parent's. `grants` are intersected with the parent's (A12). Fails if the child's depth (`parent.depth + 1`) would exceed `AGENT_MAX_DEPTH`, or if the caller already has `AGENT_MAX_CHILDREN` live direct children. |
| `switchboard.agent.send` | `{to_agent_id, message, wait?}` | `{message_id, reply?}` | Requires an `allowed` edge. Appends to the recipient's peer chat and enqueues a run. With `wait: true` (the default when a parent addresses its own child) the calling run goes `waiting` — releasing its concurrency slot, R7 — for up to `AGENT_REPLY_TIMEOUT` and receives the reply as the tool result; on timeout it resumes with a tool error and the reply, if it ever arrives, lands in the mailbox. With `wait: false` it returns immediately and the reply always arrives in the mailbox. |
| `switchboard.agent.list` | `{scope?}` | `[{agent_id, name, description, status, depth}]` | Children by default; `scope: "reachable"` lists every agent with an allowed edge. Never reveals agents the caller cannot message. |
| `switchboard.agent.stop` | `{agent_id}` | `{cancelled}` | Cancels a descendant's runs. Only on descendants. |
| `switchboard.inbox.read` | `{wait?}` | `[{from_agent_id, message_id, message}]` | Drains the caller's mailbox (§5.6). `wait` up to 30 s. |
| `switchboard.graph.set_edge` | `{from_agent_id, to_agent_id, allowed}` | `{ok}` | Only within the caller's own subtree. Applies immediately and publishes a change event. |
| `switchboard.mcp.grant` | `{agent_id, label, project?, server, allowed}` | `{ok}` | Only on descendants, and only within the caller's own grants (A12). |
| `switchboard.mcp.list_servers` | `{}` | `[{label, project, server, connected, tool_count}]` | The caller's own grants joined against the live registry, so an agent can see what it may use and what is currently reachable. |

- **M1.** `switchboard.agent.*` tools are only listed for an agent whose
  `capabilities.can_spawn` / `can_message` flags are set. A leaf worker agent gets none of them,
  which is both cheaper (fewer tokens) and safer.
- **M2. Annotations.** `agent.list` and `mcp.list_servers` are `readOnlyHint: true`. Everything
  else changes state and is annotated honestly, so §5.8's approval gate can see it:
  `agent.spawn` and `agent.send` are `destructiveHint: false, openWorldHint: true` (effects outside
  the caller); `agent.stop`, `graph.set_edge` and `mcp.grant` are `destructiveHint: true,
  openWorldHint: false` (they kill work or rewrite permissions); `inbox.read` is
  `readOnlyHint: false, idempotentHint: false` because it *drains* the mailbox — the same reasoning
  that makes the harness's `process_read` non-idempotent (§9.6).

### 5.6 Mailbox, wake-up and termination

The draft this replaces had no answer for *how a stopped agent receives a message*. MCP is
request/response; an agent that is not running cannot be called. So:

- **B1. Every agent has a durable mailbox** (`inbox` table: `id, agent_id, from_agent_id,
  chat_id, message_id, delivered_at`).
- **B2. Delivery wakes the recipient.** `switchboard.agent.send` appends to the mailbox and
  enqueues a run for the recipient's peer chat if one is not already queued. An agent with
  `auto_wake = false` accumulates mail instead and shows a badge in the Graph view.
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
  plus a key — covers Ollama, vLLM, OpenRouter, LM Studio). Each is a file in `internal/llm/`.
- **L3. Credentials** come from `MCP_SWITCHBOARD_LLM_<PROVIDER>_API_KEY`, subject to the
  file-path substitution rule (P3), so sops/systemd credentials work with no new mechanism.
  A key is never returned by any API endpoint and never logged.
- **L4. Model registry.** `GET /api/models` lists configured providers and their usable models,
  for the model picker. Models are declared in config (`MCP_SWITCHBOARD_LLM_MODELS`, a JSON file
  path) rather than discovered, because pricing must be attached to them anyway.
- **L5. Cost.** Prices are `{input_micros_per_mtok, output_micros_per_mtok, cache_read, cache_write}`
  per model in that same file. A model with no price entry records `cost_micros = 0` and is
  flagged `cost_unknown` in the UI rather than guessing. All money is integer micros; no floats.
- **L6. Tool-call translation.** The provider layer converts between our provider-neutral content
  blocks and each provider's schema. Tool JSON Schemas pass through from `tools/list` untouched;
  a schema a provider rejects causes that *tool* to be dropped for that provider with a warning,
  never the whole request to fail.

### 5.8 Human-in-the-loop

- **W1.** Every agent carries `approval: "never" | "destructive" | "always"` as a stored column
  (§5.2). It defaults to `destructive` when the agent is *created* holding a grant that matches a
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
  sub-interfaces (`CallStore`, `AgentStore`, `ChatStore`, `GrantStore`, `RunStore`) so a caller
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
- **D6. Migration 001 is the whole schema**, in one file: `calls` (including `agent_id`, `chat_id`,
  `run_id`, and `denied` among the `status` values), the orchestrator tables (`agents`, `chats`,
  `messages`, `edges`, `grants`, `runs`, `inbox`) with their indexes including the partial unique
  index on agent names (§5.2), and the FTS5 virtual table and triggers backing the chat search of
  U6. The sequence starts here; 002 onwards are real changes made after this revision ships.
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
arguments and results stay snake_case** (`to_agent_id`, `exit_code`), because they sit in the same
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
| GET | `/metrics` | Prometheus |

- **N1.** A tool that answers with an MCP error is a **successful call that returned an error**:
  HTTP 200 with `status: "error"` in the body. Only an unknown connection/server/tool is 404.
  Folding the two together would make the console report transport failures as tool bugs.

### 7.2 New surface

| Method | Path | Notes |
|---|---|---|
| GET/POST | `/api/agents` | list / create |
| GET/PATCH/DELETE | `/api/agents/{id}` | `DELETE` soft-deletes and cancels descendants |
| GET | `/api/agents/{id}/grants` · PUT | grant set; `PUT` applies A14 propagation atomically |
| GET | `/api/graph` | agents + edges + grants + live-connection join, in one response |
| PUT | `/api/graph/edges/{from}/{to}` | `{allowed}` |
| GET/POST | `/api/chats` | list (filter by `agentId`, `kind`, `tag`, `q`) / create |
| GET/PATCH/DELETE | `/api/chats/{id}` | `PATCH` sets `title`, `tags`, `activeLeafId` |
| GET | `/api/chats/{id}/messages` | `?leaf=` walks that leaf; `?tree=1` returns the whole DAG |
| POST | `/api/chats/{id}/messages` | append a user message and start a run; `Idempotency-Key` honoured (R5) |
| POST | `/api/chats/{id}/branch` | `{fromMessageId, content?}` — creates a sibling and re-points the leaf |
| GET | `/api/chats/{id}/stream` | **per-chat SSE**, see 7.4 |
| GET | `/api/runs/{id}` · POST `/api/runs/{id}/cancel` | |
| POST | `/api/runs/{id}/approvals/{callId}` | `{approved, reason?}` (W2) |
| GET | `/api/models` | configured providers, models and prices (never keys) |
| GET | `/api/stats` | db size, row counts, active runs |

### 7.3 The global event bus is lossy on purpose

`/api/events` fans out from a bounded per-subscriber queue and **drops the oldest event when a
subscriber falls behind**. That is correct for *change notifications* — a client that missed
"connections changed" just refetches — and it is the existing behaviour.

- **N2.** It is **not** correct for anything where every item matters. Token deltas must never go
  through this bus: a dropped delta silently corrupts the visible message with no way to detect
  it. This is why 7.4 exists as a separate mechanism, and it is a hard rule, not a preference.
- **N3.** Events on the global bus are therefore all *idempotent notifications*:
  `{type: "connections"}`, `{type: "call", call}`, `{type: "agent", agentId}`,
  `{type: "graph"}`, `{type: "chat", chatId}`. None carries incremental content.

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

- **U1. Single artifact.** `go build` produces one binary containing the console. Nothing is
  fetched at runtime; the loopback-only-box constraint that made the old console build-free is
  satisfied by building ahead of time instead of not building at all.
  **`hub/web/dist/` is committed**, because `go build` has to work without npm — for `go install`,
  for the Nix build, and for anyone who checks the repo out to change one line of Go. `npm run
  build` in `hub/web` regenerates it. The Vite build is byte-reproducible for a given lockfile and
  node version, so CI rebuilds it and fails on any diff; that is a real check rather than a
  heuristic about timestamps.
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
- **U4. Theming.** Tailwind with CSS custom properties, dark mode by `prefers-color-scheme` with
  an explicit override, matching the existing console's palette.
- **U5. Routes.** `/connections`, `/calls`, `/endpoints`, `/chat`, `/chat/:chatId`, `/graph`,
  `/graph/:agentId`, `/settings`.

### 8.1 Chat view

- **U6. List pane.** All chats, newest activity first, grouped by agent, with badges: agent name,
  model, kind (`human`/`agent`/`spawn`), token total, cost, status, tags. Search by title and
  full-text over message content (SQLite FTS5). Multi-select for bulk archive/delete/export.
- **U7. Thread pane.** Messages rendered from `activeLeafId` (A5). Markdown with syntax
  highlighting; code blocks copyable. Assistant messages stream token-by-token.
- **U8. Branch affordances.** Any message with siblings shows `‹ n/m ›`. Every assistant message
  has **Regenerate**; every user message has **Edit** — both create a sibling (A4). A "branch
  here" action on any message starts a new leaf from that point. Branching never leaves the chat.
- **U9. Tool call inspection.** Each tool call renders as a collapsible card: name, resolved
  `label/project/server`, duration, status, and raw argument/result JSON with a copy button and a
  link to the corresponding row in Calls.
- **U10. Approvals.** A blocked run renders an inline card with the full arguments, Approve /
  Deny, and the remaining time before auto-deny (W3).
- **U11. Composer.** Model override for the next turn, attachment of files (stored as content
  blocks), stop button while a run is active, and a visible budget meter (turns / tokens / cost
  against the run's limits).
- **U12. Export.** A chat exports as JSON (full DAG) or Markdown (the active path).

### 8.2 Graph view

- **U13. Canvas.** React Flow. Nodes are agents laid out as a tree (children below parents, via
  ELK or dagre); edges are communication edges. Pan/zoom/fit, and on mobile a pinch-zoom canvas
  with the detail panel as a bottom sheet.
- **U14. Node content.** Name, model chip, status dot (`idle`/`running`/`waiting`/`blocked`/
  `done`/`error`), last activity age, live token/cost counters, unread mailbox badge, and a
  depth indicator. A running agent pulses; the node of a currently-streaming run shows its latest
  tool call.
- **U15. Edge editing.** Click an edge to toggle `allowed`; drag between nodes to create one. The
  parent→child structural edge is drawn distinctly from communication edges and cannot be deleted
  (deleting it would orphan an agent — delete the agent instead).
- **U16. Permissions panel.** Selecting an agent opens a panel listing every `ServerRef` it may
  use, joined against the live registry so offline ones are greyed with a "not connected" note
  (I3, A15). Toggles write through `PUT /api/agents/{id}/grants` and show the inherited/explicit/
  human source (A13). A toggle that would widen beyond the parent warns before applying.
- **U17. Real time without polling.** The view subscribes to the global feed (7.3) and refetches
  `/api/graph` on `agent`/`graph` events. There is **no TTL cache**: the earlier draft's 5-second
  cache would add staleness to a system that already has push invalidation. If `/api/graph`
  becomes expensive, it is memoised in-process and invalidated on write — never expired by time.
- **U18. Spawn dialog.** Creating a child pre-fills the parent's model and grants, shows what will
  be inherited, and refuses (client-side, and again server-side) a grant the parent lacks.

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
- **X8. Agent-supplied identifiers are never trusted.** `agent_id` in a `switchboard.*` call is
  authorised against the *calling* agent's identity, which the hub knows from the session's scope
  (`/mcp/agent/{id}`), not from the arguments. A tool call cannot assert who it is *in its
  arguments* — but the scope URL does assert it, and that is the whole of the authentication. An
  `agent_id` is therefore a **capability**: anyone who can reach the private listener and knows (or
  guesses, though uuidv7 makes that impractical) an id can act as that agent, including one holding
  `(*, *, *)`. This is why `/mcp/agent/{id}` is internal to the run loop (§5.5) and why X6's
  "keep the private listener on loopback" is not optional advice once agents are enabled. If Q2 is
  ever answered yes, `PRIVATE_TOKEN` must become mandatory under `AGENTS_ENABLED=true`.

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
- **E2. Dockerfile** becomes **two** stages, not three: a Go stage building the binary and a
  minimal runtime stage with a non-root user and the `/var/lib/mcp-switchboard` volume. There is no
  node stage, because `web/dist` is committed (U1) — one toolchain in the image build, and an
  offline build works. The runtime stage is `FROM alpine` rather than `scratch`: a non-root user
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
  `LLM_<PROVIDER>_API_KEY`, `LLM_<PROVIDER>_BASE_URL`, `AGENT_MAX_DEPTH`, `AGENT_MAX_CHILDREN`,
  `AGENT_MAX_CONCURRENT_RUNS`, `AGENT_MAX_PARALLEL_TOOL_CALLS`, `AGENT_DEFAULT_BUDGET` (including
  `max_lifetime_cost_micros`, B4), `AGENT_DEFAULT_GRANTS`, `AGENT_REPLY_TIMEOUT`,
  `APPROVAL_TIMEOUT`, `INBOX_RETENTION_DAYS`, `SHUTDOWN_GRACE`. These are the only names for these
  limits; §5 refers to them exactly as spelled here.
- **E7.** With `AGENTS_ENABLED=false` the hub behaves exactly as it does today: no `switchboard.*`
  tools, no `/mcp/agent/*` scope, no Chat or Graph views in the console, no LLM dependency at
  runtime.

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
  U6.
- **V6. Orchestrator tests** use a scripted fake provider (a deterministic `llm.Provider` that
  replays a canned sequence of deltas and tool calls), so run-loop, budget, cancellation, approval
  and mailbox behaviour are tested with no network and no cost. Two cases are named explicitly
  because they are the failures this revision exists to prevent: **(a)** two agents with a
  symmetric allowed edge, each answering the other, terminate on the lifetime cost ceiling (B5) in
  bounded time — the A9 regression test; **(b)** a tree of `AGENT_MAX_CONCURRENT_RUNS + 4` agents
  all blocked in `send` with `wait: true` still makes progress, proving waiting runs hold no slot
  (R7).
- **V7. Console tests:** Vitest for hooks (branch navigation over the message DAG, stream
  assembly and resume-by-`seq`), Playwright for one end-to-end chat flow against a hub with the
  fake provider, run at a mobile viewport as well as desktop.
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
   paths this document uses throughout (§4.2); `legacy/hub` stays importable and testable until
   step 10 deletes it. Gate: the existing end-to-end suite passes against the Go binary
   unmodified.
4. **Console rewrite at parity**: Connections, Calls, Endpoints in React, mobile-ready. Gate: a
   Playwright run covering what the old console did.
5. **Store** for the new entities (§6), with no orchestrator behind them yet. Migration 001 is
   written once, at step 3, and grows until this revision ships — there is no deployed database to
   keep compatible with (D6), so the schema stays one file until the first change after release.
6. **Orchestrator core**: agents, grants, run loop, fake provider, `/mcp/agent/{id}`. Gate: V6.
7. **Chat view** with streaming and branching (§8.1, §7.4).
8. **Graph view** with edges and permission editing (§8.2), plus `switchboard.agent.*` tools.
9. **Real providers**, cost table, budgets, approvals.
10. **Cut over** deployment: Dockerfile, flake, NixOS module, CI (§12). Retire `legacy/hub`, and
    delete any existing `calls.db` rather than migrating it (D7).

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
