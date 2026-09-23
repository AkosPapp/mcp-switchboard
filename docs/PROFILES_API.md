# Agent profiles, per-chat client, tool inspection

> **Superseded in part by [CHAT_MODEL_API.md](CHAT_MODEL_API.md)** (chats are the unit: `POST /api/chats` creates a
> chat and its execution record together and takes `systemPrompt`, `parentChatId`; agents are no longer user-facing;
> the tools are `switchboard.chat.*`) and by [AGENT_MODEL_API.md](AGENT_MODEL_API.md) (manually created agents,
> live profile reference, optional client). Where they differ, that file wins. Stale below:
> "a chat is created from a profile and creates its working agent" (agents are now created by
> hand with `POST /api/agents`, chats are created under them; the `{profileId?, clientLabel}` form
> of `POST /api/chats` is the legacy path and `clientLabel` is now optional there), and "existing
> agents are unaffected by later profile edits" (an agent's profile reference is live).

Contract for the "Agents panel" change. Sits on top of docs/API.md (same conventions:
camelCase JSON, RFC 3339 `+00:00` times, errors as `{error: string}`; 404 for everything
here when `AGENTS_ENABLED=false`).

## Concepts

- **Profile** (UI name: "agent"): a reusable *template*. A system prompt, a default model,
  the hub-provided tool permissions (`canSpawn`, `canMessage` → the `switchboard.*` tools,
  spec §5.5/M1) and an approval mode (W1). **A profile is not an agent instance** and has
  **no MCP client / server / grant settings at all.** Exactly one profile is the default.
- **MCP client** = strictly a client process that dials the hub, identified by its
  `label` (the `label` of a connection). Not an MCP server, not a hub tool.
- **A chat** (legacy form, see the note above) is created from a profile plus one client label. The hub creates the
  working agent instance for that chat (a root agent, `kind` human chat) with the
  profile's prompt/model/capabilities/approval and grants
  `{label: <client>, project: "*", server: "*", allowed: true, source: "explicit"}` and
  nothing else. The chat therefore reaches every server of that one client, plus the hub
  tools its profile allows.
- **Sub-chats inherit**: a child chat created by `switchboard.chat.spawn` gets the parent's
  client restriction (already guaranteed by A12: grants ⊆ parent's), and, unless the
  spawn call says otherwise, the parent's `capabilities`, `approval` and `model`. The child
  chat records the same `clientLabel`/`profileId` as the parent's chat.

## Profile JSON

```ts
interface Profile {
  id: string;
  name: string;                    // unique among profiles
  description: string;
  systemPrompt: string;
  model: { provider: string; model: string; [k: string]: unknown } | null; // null = first configured model
  capabilities: { canSpawn: boolean; canMessage: boolean };
  approval: "never" | "destructive" | "always";
  budget: Record<string, number>;  // same keys as agents.budget; {} = hub defaults
  isDefault: boolean;
  createdAt: string; updatedAt: string;
}
```

## Routes

| Method | Path | Notes |
|---|---|---|
| GET | `/api/profiles` | `{profiles: Profile[]}`, default first, then by name |
| POST | `/api/profiles` | body: Profile fields except id/isDefault/timestamps (`name`, `systemPrompt` required). 409 name taken. 201 |
| GET/PATCH | `/api/profiles/{id}` | PATCH takes any subset incl. `isDefault: true` (atomically un-defaults the others; `false` on the current default is 400 — pick another instead) |
| DELETE | `/api/profiles/{id}` | 409 if it is the default and others exist; deleting the only profile is 409. Existing chats/agents are untouched (their `profileId` becomes null) |
| POST | `/api/chats` | **changed**: body `{profileId?: string, clientLabel?: string, title?: string, model?: object}` (legacy form; creates an `origin: "chat"` agent). `profileId` omitted → the default profile. `clientLabel` is optional (omitted/null → client-less agent), must not be `*` (400). The console uses `{agentId, title?}` instead, see AGENT_MODEL_API.md. Response is the Chat (below). The old `{agentId}` form keeps working for API callers |
| GET | `/api/hub-tools` | `{tools: HubTool[]}`: every `switchboard.*` tool the hub provides |
| GET | `/api/chats/{id}/tools` | `{clientLabel: string \| null, clientConnected: boolean, tools: ChatTool[]}`: what this chat's agent can call right now |

On first start with the orchestrator enabled and no profiles, the hub seeds one default
profile named `Assistant` (prompt `You are a helpful assistant.`, no hub tools, approval
`destructive`).

```ts
interface Chat { /* everything in docs/API.md, plus: */
  profileId: string | null;
  clientLabel: string | null;      // the one client this chat may use; null for legacy chats
}

interface HubTool {
  name: string;                    // real dotted name, e.g. "switchboard.chat.spawn"
  description: string;
  inputSchema: object;
  annotations: { readOnlyHint?: boolean; destructiveHint?: boolean; idempotentHint?: boolean; openWorldHint?: boolean };
  requires: "canSpawn" | "canMessage" | "canSpawn or canMessage" | "always";
}

interface ChatTool {
  name: string;                    // exposed name (`label__[project__]server__tool` or `switchboard.…`)
  description: string;
  origin: "mcp" | "hub";
  server?: { label: string; project: string; server: string };  // origin "mcp"
  annotations?: HubTool["annotations"];
  inputSchema?: object;
}
```

`/api/chats/{id}/tools` is computed exactly the way the run loop computes a turn's catalog
(§5.4: the agent's grants ∩ live registry, then the hub tools its capabilities allow), so
it never disagrees with what the model is offered. A chat whose client is offline returns
`clientConnected: false` and only the hub tools.

## Global events

Profile changes publish `{type: "profile"}` on the global bus (new event type; the console
invalidates its profile queries on it).
