# What is left to implement

Companion to [`../spec.md`](../spec.md), which is normative. This file is only a status
board: what is done, what is not, and what is worth knowing that the spec alone would not
tell you. When the two disagree, the spec wins and this file is stale.

REST and SSE shapes for the orchestrator surface are in [`API.md`](API.md).

## Done

| Step | What | Gate that proved it |
|---|---|---|
| 1 | Wire frozen: `docs/protocol.json`, `docs/envconf-cases.json` | Python and Go both assert against the manifests |
| 2 | `server/` removed | — |
| 3 | **Go hub at parity**: tunnel, registry, sessions, five scopes, calls, API, metrics, Loki | The end-to-end suite passes against the compiled binary |
| 4 | **Console rewritten** in React + Vite + TS + Tailwind, embedded with `embed.FS` | Vitest + Playwright (desktop and mobile) against the real binary |
| 5 | **Store for the orchestrator entities**: agents, chats, messages (DAG + FTS5), edges, grants, runs, inbox, idempotency keys; recursive-CTE path walk, sibling memory, atomic message+leaf+usage roll-up, A14 grant propagation | Store tests (V5) and table-driven grant resolution (V1) |
| 6 | **Orchestrator core**: scheduler (R4/R7), run loop, approvals, budgets, mailbox, `/mcp/agent/{id}`, all eight `switchboard.*` tools, metrics, Loki kinds | V6 with a scripted provider, including the symmetric-edge cost-ceiling test (A9/B5) and the `MAX_CONCURRENT_RUNS + 4` blocked-in-`send` test (R7) |
| 7 | **Chat view**: branching DAG, per-chat SSE with resume by `{runId}:{seq}`, tool cards, approvals, composer with budget meter, export | Vitest for the DAG and stream reducers; Playwright chat flow at desktop and mobile against the real hub, the real client and a mock OpenAI-compatible server |
| 8 | **Graph view**: React Flow tree, edge editing, permissions panel, spawn dialog, live via the global feed | Vitest for layout and grant logic; Playwright at desktop and mobile |
| 9 | **Real providers**: `anthropic`, `openai`, `openai-compatible` over one streaming interface; model and price registry; approvals and budgets wired to spend; NixOS `agents.*` / `llm.*` options | httptest streaming tests per provider; cost math |
| 10 | **Cut over and Python hub retired**: Nix, flake, NixOS module, Dockerfile, CI; `legacy/hub/` deleted. No `calls.db` migration: the Go hub refuses a pre-migrations database (D7), so delete any old one | NixOS VM test |
| 11 | **Agent profiles, per-chat client, tool inspection**: an Agents panel of reusable profiles (system prompt, default model, hub-tool permissions, approval; no client/server settings), a chat created from a profile plus exactly one MCP client, sub-agents inheriting both, a Hub tools section in Connections and a per-chat tool list. Migration 002 (first real migration); spec.md §5.2a, §8.3, X9; contract in `PROFILES_API.md` | Store upgrade test v1→v2 with data preserved; profile/chat/inheritance tests including "a child can never reach another client"; Playwright against the real hub |

The hub binary is the shipped artifact everywhere: the container, the Nix package, the
NixOS unit and both e2e suites all run *it*.

## Known gaps and debts

- **The NixOS VM test was extended (V8: an `agents` node, `/api/agents` answering, 404 with
  agents off) but has not been run**, only evaluated. Run `nix flake check` on a machine
  with KVM before relying on it.
- **Nothing has been run against a real LLM provider.** The providers are tested against
  `httptest` servers and a mock OpenAI-compatible server only.
- **`AGENTS_ENABLED` stays `false` by default.** With it off there is no `/mcp/agent/*`,
  no `switchboard.*` tools, no orchestrator routes, and the console hides Chat and Graph
  (E7). The console detects this by whether `GET /api/models` is a 404.
- **A default root agent gets `approval: destructive`**, so under that mode
  `switchboard.agent.spawn` and `agent.send` (`openWorldHint: true`) also pause for
  approval. That is what W1 says; it may surprise someone expecting only shell tools to.
- **`/mcp/agent/{id}` has no run behind it** when reached directly, so a call that needs
  approval is denied there rather than paused.
- **Idempotency claims its key before the run exists.** A failure in between leaves a
  dangling key for the 10-minute window.
- **`agent.send` to an `auto_wake=false` recipient with a busy peer chat** appends the
  message immediately, which could interleave with a running turn.
- **API errors are written as `{detail}`** by the Go `writeError`, while `PROFILES_API.md` says `{error}`; the console reads both.
- **Chat rename and tag editing** have no UI (the API supports both).
- **The store marks `queued` and `waiting` runs interrupted at startup**, not only
  `running` as A3 says, because they are equally dead after a restart.
- **`hub/web/dist/` is committed** and the root `.gitignore` has an explicit exception for
  it. `go build` has to work without npm; CI rebuilds it and fails if the committed copy is
  stale (U1). Run `npm run build` in `hub/web` and commit the result when the console
  changes.
- **The Playwright suite needs three toolchains** (Go, Node, Python) plus a browser, and a
  hub binary at `hub/hub` (`go build -o hub/hub ./cmd/mcp-switchboard-hub` from `hub/`).
  On NixOS `npx playwright install` does not work; point `PLAYWRIGHT_BROWSERS_PATH` at
  `nixpkgs#playwright-driver.browsers` and set `PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS=true`.
- **Both e2e fixtures run the hub from a temp directory**, not the repo, because the hub
  reads `./.env` by default and a developer's own settings would otherwise leak into a test
  run.

## Open questions

Carried from spec.md §16; each changes an implementation detail.

| | Question | Current behaviour |
|---|---|---|
| Q1 | Is an agent's chat visible to its parent? | No; a parent sees only the replies it receives |
| Q2 | Is `/mcp/agent/{id}` reachable by external consumers? | Internal to the run loop; if answered yes, `PRIVATE_TOKEN` must become mandatory under `AGENTS_ENABLED` (X8) |
| Q3 | Attachments: content blocks in SQLite, or a blob directory? | Content blocks in SQLite |
| Q4 | One harness root per agent, or one shared per machine? | Shared |
| Q5 | Pin tool-catalog ordering and prompt prefix for provider caching? | Ordering is stable (sorted by composed name); cache hit rates not measured |

Q6 (whether a blocking `send` holds the parent's run) is resolved: it does, but a waiting
run releases its concurrency slot (R7).
