# Chats are the unit; chats talk to each other

Contract for "drop the concept of agents". Supersedes the *user-facing* parts of
docs/AGENT_MODEL_API.md and docs/PROFILES_API.md (where they differ this file wins; the
`/api/agents*` routes keep working unchanged for the Graph view and API callers). Conventions as
in docs/API.md (camelCase, RFC 3339 `+00:00`, errors `{detail}`, 404 when `AGENTS_ENABLED=false`).

## Model

- **The user sees only chats.** A chat has a title, a system prompt (a profile reference, custom
  text, or none) and exactly one MCP client (or none). There is no "create an agent", no agent
  row in the Chat panel, no "+ chat under an agent".
- Internally every chat still has one execution record (the existing `agents` row). It is **1:1**
  with the chat for anything created from now on: creating a chat creates its record, deleting
  the chat deletes it. Old data where one agent has several chats keeps working (a run is
  already per chat); such chats simply show up as independent chats.
- **Sub-chats.** A chat that spawns another (`switchboard.chat.spawn`) gets a *child chat*, shown
  nested under its parent chat (`parentChatId`). It inherits client, capabilities, approval, model
  and prompt reference as sub-agents did (A16–A22).
- **Chats talk by injecting messages.** There are no separate "peer" conversations any more.
  `switchboard.chat.send` appends a message to the **recipient's own chat** as a `user`-role
  message carrying `sender` metadata, then wakes the recipient exactly like a human message
  would. It is always fire-and-forget: there is no `wait`, no synchronous reply, and no special
  routing back. If the recipient wants to answer, it calls `switchboard.chat.send` back to the
  sender, exactly like any other message — edges are symmetric, so it always can. The one
  exception is `switchboard.chat.spawn`'s own `message`: a spawned child's *final* answer is still
  returned to the spawning chat automatically, because that is a property of spawning a task, not
  of `chat.send`. To the model the injected message reads as a user turn starting with a fixed
  preamble line naming the sender chat, e.g.
  `[Message from chat "researcher" (id 01ab…). Reply with switchboard.chat.send to that id.]`
  followed by the text. The console renders it as a distinct message ("from researcher", linked
  to that chat), never as something the human typed.
- **Connections are independent of the tree.** Edges are symmetric (one row per unordered pair)
  and connect *any* two chats the caller can name, not just parent to child — this is how two
  unrelated leaf chats end up able to collaborate directly. The parent/child structure, in
  contrast, is immutable once set at spawn time: nothing ever reparents a chat.
- Prior peer conversations (`kind: agent`, `spawn` chats with `peerAgentId`) stay readable as
  ordinary chats; nothing new is written to them.

## JSON

```ts
interface Chat { /* everything in docs/API.md, plus: */
  parentChatId: string | null;     // the chat that spawned this one; nesting in the console
  clientLabel: string | null;      // the one client, or null
  profileId: string | null;
  agentId: string;                 // internal; the console never shows it (Graph/API only)
}

interface Message { /* everything in docs/API.md, plus: */
  sender: null | {                 // set on a user-role message injected by another chat
    chatId: string;
    chatTitle: string;             // title at the time of sending
    kind: "message" | "reply" | "spawn";   // message = send, reply = a returned answer, spawn = initial task from the parent
  };
}
```

## Routes

| Method | Path | Notes |
|---|---|---|
| POST | `/api/chats` | **primary creation form**: `{title?: string, profileId?: string \| null, systemPrompt?: string, clientLabel?: string \| null, model?: object, parentChatId?: string}`. Creates the chat and its execution record together. `profileId` set → live profile reference (as in AGENT_MODEL_API.md); `profileId: null`/omitted with no default use → own `systemPrompt` (may be empty = none). **`profileId` omitted means the default profile only if `systemPrompt` is also omitted**; explicit `profileId: null` means none. `clientLabel` omitted or null → no client; `*`/`""` → 400. Title default: `"New chat"`, auto-title from the first message as before. The legacy `{agentId, title?}` form keeps working |
| PATCH | `/api/chats/{id}` | additionally accepts `profileId`, `systemPrompt` (own text; only used while `profileId` is null), `clientLabel` (rebinding rewrites grants as in PATCH /api/agents), plus the existing `title`, `tags`, `activeLeafId`, `archived`, `selectMessageId` |
| DELETE | `/api/chats/{id}` | permanent. Also deletes its execution record when no other chat uses it, and cascades to child chats (`parentChatId`) and their records, cancelling their runs. Response `{deletedChats: number}` |
| GET | `/api/chats` | filters as before; each Chat carries `parentChatId` (the console builds the nesting client group → chat → child chats from it) |
| GET | `/api/chats/{id}/messages` | messages now carry `sender` |

## Hub tools (LLM-facing; renamed from `switchboard.agent.*`)

| Tool | Input | Output | Behaviour |
|---|---|---|---|
| `switchboard.chat.spawn` | `{title, system_prompt, grants?, budget?, model?, capabilities?, approval?, message?}` | `{chat_id}` | creates a child chat (with its execution record) of the calling chat; `message` (optional first task) is injected as `sender.kind = "spawn"` |
| `switchboard.chat.send` | `{to_chat_id, message}` | `{message_id}` | requires an allowed edge between the two chats' records; injects into the recipient's own chat as above; always fire-and-forget, no `wait`, no inline reply |
| `switchboard.chat.list` | `{}` | `[{chat_id, title, status, relation, depth?}]` | one flat list: parent + children (`relation: "parent"`/`"child"`, with `depth`) plus every chat joined by an edge (`relation: "connected"`); no `scope` argument |
| `switchboard.chat.stop` | `{chat_id}` | `{cancelled}` | descendants only |
| `switchboard.graph.set_edge` | `{chat_id, allowed}` | `{ok}` | opens/closes a symmetric edge between the caller and `chat_id`, any chat the caller can name — not limited to its own subtree; the caller must be one of the two endpoints |
| `switchboard.inbox.read`, `switchboard.mcp.grant` (`chat_id`), `switchboard.mcp.list_servers` | | | as before with chat ids in place of agent ids |

The old `switchboard.agent.*` names are **not** listed any more (models only see the new names).
Capability flags keep their names (`canSpawn`, `canMessage`); the console words them in terms of
chats.

## Console vocabulary

User-facing text never says "agent": the Chat panel lists **chats** (client group → chat → child
chats), the Graph view shows **chats** as nodes (node opens the chat), the system-prompt tab is
called **Prompts** (route `/prompts`, `/agents` redirects to it; profiles are "prompts"), hub tool
descriptions say chat. API route names (`/api/agents`, `/api/profiles`) are unchanged.
