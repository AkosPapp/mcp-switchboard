# Manually created agents, optional client, environment detection

> **Superseded in part by [CHAT_MODEL_API.md](CHAT_MODEL_API.md)** ("chats are the unit"). The user-facing
> parts of this file are stale: the console no longer creates or shows agents, a chat is created whole with
> `POST /api/chats` (which creates its execution record 1:1), chats nest under a parent chat, the LLM tools
> are `switchboard.chat.*` (the `switchboard.agent.*` names below no longer exist) and chats talk by
> injecting messages into each other. What remains valid: the `/api/agents*` routes (kept for the Graph view
> and API callers), the agent JSON, the live profile reference, one client or none, the system-prompt
> endpoint and the environment detection. Where they differ, CHAT_MODEL_API.md wins.

Contract for the "chat panel restructure". Extends docs/API.md and docs/PROFILES_API.md
(same conventions: camelCase JSON, RFC 3339 `+00:00`, errors `{detail}`; everything here
404s when `AGENTS_ENABLED=false`). Where this file contradicts PROFILES_API.md, this file wins.

## Model

- A **profile** is still a reusable system-prompt template (the Agents panel). Unchanged.
- An **agent** is now something the user **creates by hand in the Chat panel**: a name, a
  system prompt source, and an MCP client:
  - **prompt**: pick a profile (`profileId`), or **none** (`profileId: null`, and the agent's
    own `systemPrompt` is used, empty = no system prompt at all).
  - **client**: pick exactly one MCP client by label (`clientLabel`), or **none**
    (`clientLabel: null`): a client-less agent has no MCP-client grants at all and only the
    hub tools its profile allows (`canSpawn`/`canMessage`).
- **Live profile reference.** While `profileId` is set, the agent's effective
  `systemPrompt`, `model`, `capabilities`, `approval` and `budget` are the profile's *current*
  values, resolved at the start of each run (and each turn for capabilities), so editing a
  profile in the Agents panel affects every agent using it. When the profile is deleted the
  agent keeps working from the values it last had (they are copied onto the agent on every
  resolve, so deletion never blanks a prompt).
- **Chats belong to an agent** (its internal execution record); older data can have many chats per agent,
  newer data is 1:1 (CHAT_MODEL_API.md). `POST /api/chats {agentId}` attaches one more chat to an existing
  record; the primary `POST /api/chats` form creates chat and record together.
- **Sub-agents** (now *child chats*, made by `switchboard.chat.spawn`) are its children; they inherit
  its client restriction, capabilities, approval, model and profile reference, as before (A16–A22).
- ~~The Chat panel shows client groups → manually created agents → chats.~~ **Superseded:** the Chat
  panel shows client groups (by `clientLabel`, plus a "No client" group) → **chats** → their child chats
  (`parentChatId`). The grouping never uses the system prompt or the profile name.

## Agent JSON additions

```ts
interface Agent { /* everything in docs/API.md, plus: */
  origin: "manual" | "spawn" | "chat";   // manual = created from the Chat panel via POST /api/agents with clientLabel present
  clientLabel: string | null;            // the one client; null = none
  profileId: string | null;
}
```

## Routes

| Method | Path | Notes |
|---|---|---|
| POST | `/api/agents` | body adds `profileId?: string \| null` and `clientLabel?: string \| null`. If the key `clientLabel` is **present** (even `null`) the request is a *manual agent*: grants are exactly `{clientLabel,*,*}` (or none when null), `AGENT_DEFAULT_GRANTS` is not applied, `origin` = `manual` for a root, `spawn` when `parentId` is set. If `clientLabel` is absent the legacy behaviour applies (default grants; the Graph's spawn dialog still uses it). `clientLabel` must not be `*`/empty-string (400). With `profileId` set, `systemPrompt`/`model`/`capabilities`/`approval` in the body are optional and ignored for resolution (stored as the initial copy). `name` required, unique among siblings (409). Response: the Agent (201) |
| PATCH | `/api/agents/{id}` | additionally accepts `profileId` (string \| null), `clientLabel` (string \| null; changing it rewrites the agent's grants to exactly the new client and, as a human edit, propagates A14 narrowing to descendants), `name` |
| POST | `/api/chats` | superseded by CHAT_MODEL_API.md: the primary form creates chat and record together; `{agentId, title?}` is the attach form (`agentId` present takes precedence). Chat JSON `clientLabel`/`profileId` mirror the agent's |
| GET | `/api/chats/{id}/system-prompt` | `{systemPrompt: string, source: "profile" \| "agent" \| "none", profileId: string \| null, profileName: string \| null, model: {provider, model} \| null, modelIsDefault: boolean, toolCount: number}`: the **exact system prompt text the run loop sends to the model for this chat's agent next turn** (computed by the same function the run loop uses, including anything the hub prepends or appends), never truncated. `source: "none"` and `systemPrompt: ""` when there is none. `modelIsDefault` is true when `model` equals the hub's first configured model (what a turn falls back to when nothing more specific was chosen) |
| DELETE | `/api/chats/{id}` | permanent; now cascades and returns `{deletedChats}` (CHAT_MODEL_API.md) |

## Connections: environment detection

The client detects where it is running and tells the hub in `hello`; the console shows it so a
client in the wrong environment or project (a dev container instead of the host, a shell with the wrong
direnv/nix devshell) is easy to spot when picking a client.

```ts
// hello.client (additive, optional; protocol version stays 1): 
interface ClientInfo { name; version; instance; label;
  environment?: {
    kinds: ("devcontainer" | "container" | "direnv" | "nix-shell" | "venv")[];  // may be empty
    project?: string;        // the project's name, shown next to the client everywhere:
                             // devcontainer.json "name" when in a devcontainer, else the git
                             // repository root's directory name, else the cwd's basename
    workspace?: string;      // absolute cwd of the client (a path, not a secret)
    details?: Record<string, string>;   // small, non-secret: e.g. {direnvDir, nixShell: "impure", devcontainerName, image, venv}
  }
}
```

The console shows the project name beside the client label wherever a client is shown or
picked (Connections tree, new-agent client picker, the Chat panel's client groups:
`legion5 · myproject`), with small chips for `kinds`.

`GET /api/connections` each connection's `client` carries the same optional `environment`
object verbatim (absent for older clients). No secrets: never send env var *values* beyond
the fixed small set above (paths and names only).
