-- 013: a built-in skill, seeded for every hub (docs/improvements.md I16):
-- "harness-review" turns the agent onto the harness ITSELF — the call history,
-- the approvals that fired, the failed and slow calls — and asks for a
-- concrete, cited change list instead of vibes. -- Run it directly with /harness-review; auto=0 on purpose: it is a
-- deliberate act to review the harness, so the model is never nudged into it
-- and the default system prompt (and the skill.load tool's mere existence)
-- stays exactly as it was for hubs that never use it. INSERT OR IGNORE: a user who deletes or renames it
-- never gets it back on upgrade.

INSERT OR IGNORE INTO skills (id, name, description, body, auto, created_at, updated_at) VALUES (
    'skill_seeded_harness_review',
    'harness-review',
    'Review how the harness is behaving in this hub and propose concrete improvements',
    '# Harness review

You are reviewing the switchboard harness (the MCP server the client dials: it
runs shell commands and reads/writes files in one project worktree) as
observed THROUGH THIS HUB: the tool calls in this chat history, which ones
needed approval, which failed, which were slow or repeated.

Do this:

1. Walk the recent tool calls (yours and siblings'' in this chat). Group them:
   reads, writes, patches, command runs, repeats of the same command.
2. For each friction you find, name it concretely and cite evidence
   (tool name, argument snippet, outcome). Examples of what has mattered:
   - a command run five times because its output was truncated;
   - approvals that fire on read-only tools because annotations were missing;
   - patches that failed on ambiguous context;
   - output formats the shell harness could return more usefully.
3. Propose AT MOST five improvements, ordered by payoff. Each one: the
   problem, the change (which layer: harness tool, hub gate/annotation,
   console), and how it would have changed an observed call above.
4. Do NOT edit any code in this review turn. End by asking which items to
   implement; then implement the chosen ones with tests, following
   AGENTS.md (gofmt, `go test ./...`, the harness suite, and never committing
   anything the user did not ask for).

Rules: evidence over speculation; every claim must point at a real call or a
real file (cite file:line when you read one); if the history is too short to
judge, say so and list what you would look for instead.',
    0,
    '2026-10-06T00:00:00Z',
    '2026-10-06T00:00:00Z'
);
