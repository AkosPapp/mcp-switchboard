# What is left to implement

Companion to [`../spec.md`](../spec.md), which is normative. This file is only a status
board: what is done, what is not, and what each remaining step depends on. When the two
disagree, the spec wins and this file is stale.

The ordering comes from spec.md §15. It exists so the tree works at every step and nothing
is written twice.

## Done

| Step | What | Gate that proved it |
|---|---|---|
| 1 | Wire frozen: `docs/protocol.json`, `docs/envconf-cases.json` | Python and Go both assert against the manifests |
| 2 | `server/` removed | — |
| 3 | **Go hub at parity**: tunnel, registry, sessions, five scopes, calls, API, metrics, Loki | The end-to-end suite, unchanged, passes against the compiled binary |
| 4 | **Console rewritten** in React + Vite + TS + Tailwind, embedded with `embed.FS` | 39 Vitest + 18 Playwright (desktop and mobile) against the real binary |
| 10 (part) | Deployment cut over early: `nix/hub.nix`, flake, NixOS module, Dockerfile, CI | NixOS VM test: two VMs, real tunnel, real tool call, console served |

The hub binary is the shipped artifact everywhere: the container, the Nix package, the
NixOS unit and both e2e suites all run *it*, not an in-process import of it.

## Not started

### Step 5 — Store for the orchestrator entities

Seven tables (`agents`, `chats`, `messages`, `edges`, `grants`, `runs`, `inbox`) plus the
FTS5 virtual table and triggers behind the chat search of U6. No orchestrator behind them
yet: this step is schema, queries and tests only.

Migration 001 grows to cover them — there is no deployed database to stay compatible with
(D6/D7), so the schema stays one file until this revision ships.

The parts that need care, because they are where a subtle bug is a correctness bug:

- the recursive CTE that walks `active_leaf_id` → root and reverses it (A5), rather than
  N queries;
- `messages.last_active_child_id`, which is what makes sibling navigation remember a
  branch's leaf (A6);
- the partial unique index on agent names — `parent_id` is NULL for roots and SQLite treats
  NULLs as distinct, and soft-deleted siblings must not hold a name hostage (§5.2);
- grant propagation (A14) and message-plus-leaf-pointer updates (A4) being atomic (D2).

### Step 6 — Orchestrator core

Agents, grants, the run loop, `/mcp/agent/{id}`, and the `switchboard.*` tools. Testable
end to end with no network and no credentials: V6 requires a scripted fake provider that
replays a canned sequence of deltas and tool calls.

Two named regression tests, because they are the failures the design exists to prevent:

- two agents with a symmetric allowed edge terminate on the lifetime cost ceiling (A9/B5);
- a tree of `AGENT_MAX_CONCURRENT_RUNS + 4` agents all blocked in `send` with `wait: true`
  still makes progress, proving a waiting run holds no concurrency slot (R7).

Depends on step 5.

### Step 7 — Chat view

Branching conversations over the message DAG, per-chat SSE with resume-by-`seq`, tool-call
inspection, approvals, the composer with its budget meter (§8.1, §7.4).

Depends on step 6. There is nothing to render before the run loop exists: the pane is a
walk of the DAG, the branch affordances are sibling navigation over `parent_id`, and
assistant messages arrive as streamed deltas that `message_done` then replaces
wholesale (N7).

### Step 8 — Graph view

React Flow canvas of the agent tree, communication edges, the permissions panel, and the
`switchboard.agent.*` tools behind it (§8.2). Depends on step 6.

### Step 9 — Real providers

`anthropic`, `openai` and `openai-compatible` behind one streaming interface (L1/L2), the
model and price registry (L4/L5), budgets and approvals wired to real spend.

Until this lands, `AGENTS_ENABLED` stays `false` by default and the hub is a gateway that
keeps working with no LLM configured at all (E7).

### Step 10 — Retire the Python hub

The deployment half is already done. What remains is deletion:

- delete `legacy/hub/`;
- drop it from `pyproject.toml`'s `[tool.uv.workspace]` members and regenerate `uv.lock`;
- remove the `hub (py…)` job from CI;
- delete any existing `calls.db` rather than migrating it (D7).

It is kept for now only so its tests keep running as a reference while the Go side settles.

## Known gaps and debts

Things that are true today and would surprise someone reading the spec alone.

- **`/mcp/agent/{id}` is a seam, not an implementation.** `internal/api/api.go` and the
  mcpserver package mark where the orchestrator surface goes; neither serves it.
- **`denied` is plumbed but never written.** The status, the `source = "agent"` value and
  the `agent_id`/`chat_id`/`run_id` columns exist end to end; nothing produces them until
  step 6.
- **The NixOS module has no orchestrator options yet.** E4's
  `services.mcp-switchboard.llm.{provider,apiKeyFile,modelsFile}` and `agents.enable` land
  with step 9. Everything else is already expressible, because every setting is an
  `MCP_SWITCHBOARD_*` variable.
- **`hub/web/dist/` is committed** and the root `.gitignore` has an explicit exception for
  it. `go build` has to work without npm; CI rebuilds it and fails if the committed copy is
  stale (U1). Run `npm run build` in `hub/web` and commit the result when the console
  changes.
- **The Playwright suite needs three toolchains** (Go, Node, Python) plus a browser, because
  it drives the shipped binary with a real client behind it. `npx playwright install
  chromium` first, or use the `console-e2e` CI job as the reference invocation.
- **Both e2e fixtures run the hub from a temp directory**, not the repo, because the hub
  reads `./.env` by default and a developer's own settings would otherwise leak into a test
  run.

## Open questions

Carried from spec.md §16; none blocks the next step, each changes an implementation detail.

| | Question | Bites at |
|---|---|---|
| Q1 | Is an agent's chat visible to its parent? | step 6 |
| Q2 | Is `/mcp/agent/{id}` reachable by external consumers? | step 6 |
| Q3 | Attachments: content blocks in SQLite, or a blob directory? | step 7 |
| Q4 | One harness root per agent, or one shared per machine? | step 6 |
| Q5 | Pin tool-catalog ordering and prompt prefix for provider caching? | step 9 |

Q6 (whether a blocking `send` holds the parent's run) is resolved: it does, but a waiting
run releases its concurrency slot (R7).
