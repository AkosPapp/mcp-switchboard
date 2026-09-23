# Hub REST + SSE contract (orchestrator surface)

Normative source is `spec.md` section 7; this file is the precise wire contract the web console
builds against. **Chats are the unit** (creating, rebinding and deleting a chat, `parentChatId`, `Message.sender`,
the `switchboard.chat.*` tools): [CHAT_MODEL_API.md](CHAT_MODEL_API.md), which wins over everything
below where they differ. Manually created agents (`origin`, `clientLabel`, `profileId`, live profile reference,
`GET /api/chats/{id}/system-prompt`) are specified in [AGENT_MODEL_API.md](AGENT_MODEL_API.md), which wins over
PROFILES_API.md where they differ. Agent profiles, the legacy client selection (`POST /api/chats` with
`{profileId?, clientLabel?}`), `GET /api/hub-tools` and `GET /api/chats/{id}/tools` are specified in
[PROFILES_API.md](PROFILES_API.md); `Chat` there gains `profileId` and `clientLabel`. Implementation: `hub/internal/api/orchestrator.go`, `export.go`,
`hub/internal/chatstream`. Tests: `hub/internal/api/orchestrator_test.go`.

## Conventions

- JSON bodies, camelCase, timestamps RFC 3339 UTC with `+00:00` (`2026-01-02T03:04:05.123456+00:00`).
- Errors: `{"detail": string}` with status **400** (invalid input / `ErrInvalid`), **404**
  (unknown id / `ErrNotFound`), **409** (`ErrNameTaken`, `ErrConflict`, approval not pending), **500** (detail hidden).
- **All routes in this file exist only when the orchestrator is enabled** (`AGENTS_ENABLED`).
  When disabled they answer 404, and `GET /api/stats` carries no orchestrator fields. Detect
  the feature by probing `GET /api/models` (404 = disabled).
- Ids are 32-char hex uuidv7. `null` = absent for optional fields. Unknown request fields are ignored.
- Existing 7.1 routes are unchanged. `GET /api/calls` already accepts `agentId`, `chatId`, `source`
  filters; call rows carry `agentId`, `chatId`, `runId` when the call came from an agent.

```ts
type Ts = string;                       // RFC 3339 with +00:00
type ContentBlock = { type: "text" | "image" | "thinking" | "tool_use" | "tool_result" | string;
                      text?: string; media_type?: string; data?: string; signature?: string };
// NOTE: content blocks (like MCP tool args) are snake_case: media_type.

interface Agent {
  id: string; parentId: string | null; name: string; description: string; project: string | null;
  model: { provider?: string; model?: string; [k: string]: unknown };
  systemPrompt: string; depth: number;
  status: "idle" | "running" | "waiting" | "blocked" | "done" | "error";
  budget: Record<string, unknown>;
  capabilities: { canSpawn: boolean; canMessage: boolean };
  approval: "never" | "destructive" | "always"; autoWake: boolean;
  tokenTotal: number; costTotalMicros: number;          // self + descendants
  createdAt: Ts; updatedAt: Ts; lastActivityAt: Ts; deletedAt: Ts | null;
}
interface Grant { agentId: string; label: string; project: string; server: string; // "*" wildcard, project "" = none
                  allowed: boolean; source: "inherited" | "explicit" | "human"; createdAt: Ts; updatedAt: Ts; }
interface Edge  { fromAgentId: string; toAgentId: string; allowed: boolean; createdAt: Ts; updatedAt: Ts; }
interface Chat  { id: string; agentId: string;          // agentId: the chat's internal execution record
                  parentChatId: string | null;           // CHAT_MODEL_API.md: the chat that spawned this one
                  peerAgentId: string | null;            // legacy, no longer written
                  title: string;
                  kind: "human" | "agent" | "spawn";     // "human" for everything written now; the others are legacy
                  activeLeafId: string | null; tags: string[];
                  tokenTotal: number; costTotalMicros: number; createdAt: Ts; updatedAt: Ts; archivedAt: Ts | null;
                  profileId: string | null; clientLabel: string | null; }
interface Siblings { ids: string[]; index: number }     // creation order; index of this message in ids
interface Message {
  id: string; chatId: string; parentId: string | null; role: "system" | "user" | "assistant" | "tool";
  content: ContentBlock[];
  toolCalls: { id: string; name: string; arguments: object }[] | null;
  toolResults: { tool_call_id: string; call_id?: string; result?: unknown; error?: string | null }[] | null; // one element (R6)
  tokenInput: number; tokenOutput: number; costMicros: number; latencyMs: number;
  model: object | null;
  finishReason: "stop" | "tool_use" | "max_tokens" | "budget" | "cancelled" | "error" | null;
  runId: string | null; lastActiveChildId: string | null; createdAt: Ts;
  sender: { chatId: string; chatTitle: string; kind: "message" | "reply" | "spawn" } | null; // set on a user message another chat injected (CHAT_MODEL_API.md)
  siblings: Siblings;                                    // added by the messages/export endpoints
}
interface Run {
  id: string; agentId: string; chatId: string; trigger: "human" | "agent_message" | "spawn";
  triggeredByAgentId: string | null;
  status: "queued" | "running" | "waiting" | "done" | "cancelled" | "interrupted" | "error";
  budgetSnapshot: object; usage: object; error: string | null; finishReason: string | null;
  createdAt: Ts; startedAt: Ts | null; finishedAt: Ts | null;
}
interface PendingApproval { callId: string; tool: string; arguments: object; expiresAt: Ts }
```

## Agents

| Route | Request | Response |
|---|---|---|
| `GET /api/agents?parentId=&roots=1&project=&includeDeleted=1` | | `200 {agents: Agent[]}` |
| `POST /api/agents` | `CreateAgent` below | `201 Agent` (400 bad input, 404 parent missing, 409 name taken) |
| `GET /api/agents/{id}` | | `200 Agent` (soft-deleted agents are returned, `deletedAt` set) / 404 |
| `PATCH /api/agents/{id}` | `PatchAgent` below | `200 Agent` |
| `DELETE /api/agents/{id}[?hard=1]` | | `204`. Soft delete + cancels its and descendants' runs; `hard=1` removes everything they own |

```ts
interface CreateAgent {
  name: string;                       // required, trimmed, unique among live siblings
  parentId?: string;                  // omit for a root agent
  description?: string; project?: string; systemPrompt?: string;
  model?: object;                     // {provider, model, ...}; default = first configured model
  budget?: object;                    // merged over hub defaults
  capabilities?: { canSpawn?: boolean; canMessage?: boolean };
  approval?: "never" | "destructive" | "always";   // omitted = derived per W1
  autoWake?: boolean;
  grants?: { label: string; project?: string; server: string; allowed: boolean }[]; // root: default grants if omitted; child: intersected with parent's (A12)
}
interface PatchAgent {                // every field optional
  name?: string; description?: string;
  project?: string | null;            // null or "" clears
  model?: object; systemPrompt?: string; budget?: object;
  capabilities?: { canSpawn: boolean; canMessage: boolean };
  approval?: "never" | "destructive" | "always"; autoWake?: boolean;
}
```

### Grants and graph

| Route | Request | Response |
|---|---|---|
| `GET /api/agents/{id}/grants` | | `200 {grants: Grant[]}` (the agent's own stored set) |
| `PUT /api/agents/{id}/grants` | single grant **or** `{grants: [...]}` | `200 {grants: Grant[], revoked: Grant[]}` |
| `GET /api/graph` | | `200 GraphView` |
| `PUT /api/graph/edges/{from}/{to}` | `{allowed: boolean}` (required) | `200 Edge` |

`PUT grants` is an **upsert**, not a replace: each grant `{label, project?, server, allowed}` is
applied as a human edit (`source: "human"`, A13, may exceed the parent's set). Grants not mentioned
are untouched; to withdraw access send `allowed: false`. Narrowing propagates to descendants'
`inherited` grants (A14) in one transaction; the descendant rows that were removed come back in
`revoked`. `label` and `server` are required (`"*"` = any). 404 if the agent is unknown.

```ts
interface GraphView {
  agents:  (Agent & { unreadMail: number; currentTool?: string })[];
  edges:   Edge[];
  grants:  (Grant & { connected: boolean; orphaned?: boolean })[];  // orphaned: human grant its parent no longer holds
  servers: { label: string; project: string; server: string; connected: boolean; toolCount: number }[];
}
```

## Chats and messages

| Route | Request | Response |
|---|---|---|
| `GET /api/chats?agentId=&kind=&tag=&q=&includeArchived=1&limit=&offset=` | | `200 {chats: Chat[], limit, offset, hits?}` |
| `POST /api/chats` | primary form `{title?, profileId?, systemPrompt?, clientLabel?, model?, parentChatId?}`: creates the chat and its execution record together (see [CHAT_MODEL_API.md](CHAT_MODEL_API.md)). Legacy attach form `{agentId: string, title?: string, kind?: "human"}`: **`agentId` present takes precedence** | `201 Chat` (404 unknown/deleted agent, profile or parent; 400 a `*`/empty `clientLabel` or other kinds) |
| `GET /api/chats/{id}` | | `200 Chat` |
| `PATCH /api/chats/{id}` | `PatchChat` | `200 Chat` |
| `DELETE /api/chats/{id}` | | `200 {deletedChats: number}`. Cancels live runs first, then deletes the chat, its child chats and the execution records nothing else uses |
| `GET /api/chats/{id}/messages[?leaf=ID][&tree=1]` | | `200 MessageList` |
| `POST /api/chats/{id}/messages` | `PostMessage` (+ header `Idempotency-Key`) | `202 PostResult` |
| `POST /api/chats/{id}/branch` | `Branch` | `202 PostResult` |
| `GET /api/chats/{id}/export?format=json\|markdown` | | file download. `json` is `{chat, messages, systemPrompt, tools}`. Each **assistant message** in `messages` is byte-reproduction-complete on its own: `model` carries the full turn options actually sent (`provider`, `model`, `max_tokens`, `temperature`, `thinking`), plus its own `systemPrompt` and `tools` (the exact text/definitions that turn ran against) — these do not drift even if the chat's profile or grants change later, unlike the top-level `systemPrompt`/`tools` fields, which are only a convenience snapshot of what the chat resolves to *now* (same live-resolved data as `GET .../system-prompt` and `GET .../tools`), omitted if that can no longer be resolved. `markdown` is the active path only, without any of this |
| `GET /api/chats/{id}/draft` | | `200 {draft: string, updatedAt: string\|null}` (`""` / `null` when none) |
| `PUT /api/chats/{id}/draft` | `{draft: string}` | `200` same shape. Empty or whitespace-only deletes the draft. Body max 256 KiB (`413`). Publishes a `chat` event |
| `GET /api/chats/{id}/stream` | | SSE, see below |

- Drafts: unsent composer text, stored server-side. Posting a message or branching with new user
  text clears the chat's draft in the same transaction; deleting the chat removes it.
- Chats list is newest-updated first, `limit` clamped to 1..1000 (default 100), archived hidden
  unless `includeArchived`. `q` matches the title (case-insensitive substring) **or** message text
  (FTS, prefix words). With `q`, the response also has `hits: {[chatId]: SearchHit[]}` where
  `SearchHit = {chatId, chatTitle, agentId, messageId, role, snippet}`; the snippet wraps matches in
  `\u0002` ... `\u0003` (control chars, not markup). `tag` is an exact tag match.
- `PatchChat` (all optional): `{title?: string; tags?: string[]; archived?: boolean;
  activeLeafId?: string; selectMessageId?: string; profileId?: string | null; systemPrompt?: string;
  clientLabel?: string | null}` (the last three rebind the chat, see CHAT_MODEL_API.md). `activeLeafId` points the chat at exactly that
  message (400/404 if it belongs to another chat). `selectMessageId` is sibling navigation (A6):
  the chat is re-pointed at the deepest previously-active leaf below that message. Use it for the
  `‹ 2/3 ›` control.
- `MessageList = {chatId, activeLeafId: string | null, tree: boolean, messages: Message[]}`.
  Default: the active path, root first. `?leaf=` walks that leaf to the root (404 if the message is
  not in this chat). `?tree=1`: every message of the chat in creation order (the whole DAG; use
  `parentId` to rebuild). Every message carries `siblings`.
- `PostMessage = {content: string | ContentBlock[]; parentId?: string; model?: object}`. A string is
  one text block. Empty content is 400. Appends a user message under the active leaf (or `parentId`)
  and enqueues a run. `Idempotency-Key`: a repeat within 10 minutes returns the original
  ids with `deduplicated: true` (still 202). Use a fresh uuid per user action, reuse it on retry.
- `Branch = {fromMessageId: string; content?: string | ContentBlock[]; model?: object}`. With content
  on a user message: an "edit and resend" sibling + run. Without content, on an assistant message:
  "regenerate". The chat's leaf is re-pointed.
- `PostResult = {messageId: string, runId: string, deduplicated?: true}`. The run's output arrives on
  the chat stream; `message_done` is authoritative.
- Export: `format=json` (default) -> `{chat: Chat, messages: Message[]}` whole DAG with `siblings`,
  `Content-Disposition: attachment`. `format=markdown` -> `text/markdown` of the **active path**
  (`# title`, `## Role` sections, tool calls/results as fenced json). Other formats 400.

## Runs, approvals, models, stats

| Route | Request | Response |
|---|---|---|
| `GET /api/runs/{id}` | | `200 Run & {pendingApprovals: PendingApproval[]}` |
| `POST /api/runs/{id}/cancel` | | `204` (404 unknown run) |
| `GET /api/approvals` | | `200 {approvals: {runId, chatId, agentId, agentName, chatTitle, callId, tool, arguments, expiresAt}[]}`: every approval pending in any run, oldest first, `[]` when none. `tool` is the exposed name the agent sees; `expiresAt` is when W3 auto-denies. Refetch on `agent` events |
| `POST /api/runs/{id}/approvals/{callId}` | `{approved: boolean, reason?: string}` | `204`; 409 if nothing is pending for that call; 400 if `approved` missing |
| `GET /api/models` | | `200 {models: {provider: string, model: string, prices: {...}, contextWindow?: number, supportsTools?: boolean, discovered?: boolean}[]}` (never keys). `discovered` marks models found on the provider (Ollama tags, `/models`) and not declared; `contextWindow` is the model's own maximum or the declared override; the list is cached (60 s) and refreshed lazily, waiting at most ~2 s |
| `GET /api/stats` | | `200 {calls: {total, ok, error}, databaseBytes, tables: {[name]: rowCount}, activeRuns?: number, chatStreamClients?: number}` |

`tables` includes the orchestrator tables (`agents`, `chats`, `messages`, `runs`, ...).
`activeRuns` / `chatStreamClients` are present only when the orchestrator is enabled.

## Per-chat SSE: `GET /api/chats/{id}/stream`

Headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache, no-transform`,
`X-Accel-Buffering: no`. 404 for an unknown chat. The first bytes are the comment `: connected`; a
comment `: keepalive` is sent every 15 s when idle. Ignore comments.

Each event:

```
id: {runId}:{seq}
event: {type}
data: {json}
```

`seq` is per run, starts at 1, strictly increasing, assigned by the hub. A chat stream carries the
frames of consecutive runs (a queued run follows the previous one, R4), so the id is the pair.
The `data` JSON is the flat payload plus `type`, `chatId`, `runId`, `seq`:

```ts
type Frame =
  | { type: "run_started"; agentId?: string; /* + run info */ }
  | { type: "delta"; messageId: string; contentIndex: number; text: string }
  | { type: "tool_call"; callId: string; name: string; arguments: object; messageId?: string }
  | { type: "tool_result"; callId: string; /* result / error / messageId */ }
  | { type: "approval_required"; callId: string; tool: string; arguments: object; expiresAt: Ts }
  | { type: "message_done"; message: Message }   // the persisted message: AUTHORITATIVE (N7)
  | { type: "run_done"; status: Run["status"]; /* usage, error */ }
  | { type: "error"; message: string }
  // all frames also carry: chatId: string; runId: string; seq: number
  ;
```

Exact extra fields of each type are whatever the orchestrator publishes; always read
`type`, `runId`, `seq` and treat unknown fields as optional. Replace an assembled message wholesale
with the `message_done` payload.

**Overflow** (hub-generated, no `id:`):

```
event: overflow
data: {"type":"overflow","reason":"slow_consumer"|"evicted"|"unknown_run"}
```

After `overflow` the server ends the stream. The client must **close its EventSource** (otherwise
it auto-reconnects with the stale `Last-Event-ID` and loops), refetch `GET /api/chats/{id}/messages`
(+ `GET /api/runs/{runId}` if it cares about state), then reconnect **without** Last-Event-ID.

**Resume.** On reconnect the browser sends `Last-Event-ID: {runId}:{seq}` (EventSource does this
automatically; `?lastEventId=` is accepted where headers cannot be set). The stream replays the
rest of that run after `seq`, then every retained frame of later runs on the chat, then goes live
with no gap or duplicate. Rings are per run, bounded (8192 frames / 4 MiB), kept until the run
ends plus 2 minutes. If the position was evicted, the run is unknown (hub restart, expired), or the
id is malformed, the reply is a single `overflow` as above. Without `Last-Event-ID` the stream is
live-only: a client should fetch messages first, then subscribe, and reconcile via `message_done`.

Each subscriber has a ~1 MiB byte budget queue; a client too slow to drain it gets `overflow`
(reason `slow_consumer`) and is closed. Deltas never ride the global `/api/events` bus (N2).
