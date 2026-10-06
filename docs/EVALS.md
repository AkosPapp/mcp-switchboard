# Agent evals

`evals/run_evals.py` measures which models in `llm-models.json` can do useful agent work with the hub's
tools, and catches regressions when tool descriptions or prompts change. It is stdlib-only Python 3.10+ and talks
to a **running** hub over REST (orchestrator on, `AGENTS_ENABLED`). It never imports hub code.

Requirements: the hub's private listener, at least one connected MCP client that offers the harness server
(it needs `python3` with `pytest` for the `fix_failing_test` task), and the models reachable by the hub.

## Run

```sh
python evals/run_evals.py --dry-run                          # connectivity + what would run
python evals/run_evals.py --models all --json out.json
python evals/run_evals.py --models openai-compatible/gpt-oss:20b --tasks fix_failing_test,json_transform
python evals/run_evals.py --client my-laptop --min-pass 0.8  # exit 1 if any model is below 80%
python -m pytest evals/tests -q                              # unit tests (fake hub, no LLM)
```

Flags: `--hub URL` (env `EVAL_HUB_URL`, default `http://127.0.0.1:8099`), `--token T` (env `EVAL_HUB_TOKEN`, the
private-listener token), `--client LABEL` (default: first connection with a harness server), `--server NAME`
(harness server name, default `harness`), `--models all|provider/model,...`, `--tasks a,b`, `--tasks-dir DIR`,
`--profile NAME|ID`, `--timeout SEC` (overrides every task), `--json FILE`, `--keep`, `--with-web`,
`--min-pass FRACTION` (0 = informational), `--dry-run`.

## What a run does

For every (model, task): discover the harness root on the client (`run_command pwd` through
`POST /api/connections/{cid}/servers/{server}/tools/{tool}/call`), create `<root>/eval-sandbox/<tag>/<model>-<task>`,
write the task's setup files with `file_write`, create a chat (default or `--profile` prompt, model pinned, that one
client), send the prompt, poll `GET /api/runs/{id}` until it ends, collect metrics, run the verifier through the
same manual-call endpoint (so remote clients work), then delete the chat and sandbox unless `--keep`.

Per model the runner clones the base profile into a temporary one with `approval: "never"` and the model pinned
(chats reference profiles live, so this is what makes the model under test the one that runs); it is deleted at the
end. If a run still asks for approval, it is auto-approved only when the task has `auto_approve: true`, otherwise
the cell is ERROR.

## Results and metrics

Table cell: `STATUS <tool calls>c <seconds>s`. Per model: pass rate = PASS / all cells (FAIL and ERROR both count
against it).

- **PASS**: run finished and every verifier check passed.
- **FAIL**: run finished but a check failed, or the task timeout / `max_turns` was hit (the run is cancelled).
- **ERROR**: not the model's answer: hub unreachable, model not offered by the hub, sandbox setup failed, the run
  ended `error`/`cancelled`/`interrupted`, or it is waiting for input. Errors do not crash the harness.
- **tool_calls / tool_errors / turns**: from the chat's assistant messages (`toolCalls`, `toolResults[].error`),
  merged with `GET /api/calls?chatId=` (the call log wins when it holds more).
- **tokens_in / tokens_out / cost_usd**: summed from assistant messages (`tokenInput`, `tokenOutput`, `costMicros`);
  0 for local models without prices.
- **seconds**: wall time for setup, run and verification.

`--json` writes `{hub, client, results[], summary{}}`; each result also has `note`, `checks`, `chat_id`, `sandbox`.
Exit codes: 0 ok, 1 a pass rate below `--min-pass`, 2 environment problem (hub unreachable, no client, bad flags).

## Tasks

Built in (`evals/eval_tasks.py`): `fix_failing_test`, `edit_file_precise`, `find_and_report`, `json_transform`,
`tool_discipline`, and `web_lookup` (only with `--with-web`; needs a profile that offers `switchboard.web.*`).

Add a task as a Python entry in `eval_tasks.py` or a JSON file in `evals/tasks/*.json` (one object or a list; the
same name replaces a built-in):

```json
{"name": "write_hello", "description": "...",
 "prompt": "Write hello to {sandbox}/out.txt",
 "setup": {"files": {"in.txt": "seed"}},
 "verify": [{"type": "file_equals", "path": "out.txt", "content": "hello", "strip": true}],
 "timeout": 240, "max_turns": 12, "auto_approve": false, "requires_web": false}
```

`{sandbox}` is the task directory (absolute, on the client); setup paths and verifier paths are relative to it.
Check types: `command` (`command`, `expect_exit`=0, runs in the sandbox), `file_equals` (`content`, optional
`strip`), `file_contains` / `file_not_contains` (`substrings`), `file_regex` (`pattern`), `file_nonempty`,
`csv_equals` (`rows`, cells compared as trimmed strings), `tool_called` (`tool` substring of an exposed tool name in
the chat). Prefer verifiers that check the outcome and also guard against cheating (for example, that the test file
is unchanged). Tasks are validated at load; run the unit tests after adding one.

## As a regression check

1. Before changing tool descriptions or the system prompt, run `--json before.json` on the models you care about.
2. Change, rebuild/restart the hub, run `--json after.json`.
3. Compare per-cell status and `tool_calls`/`tool_errors`: a task that flips PASS to FAIL, or a jump in tool errors or
   calls for the same task, points at the description you touched. Tool errors and extra calls are often the earlier
   signal. Small local models are nondeterministic, so rerun a flipped cell a couple of times before concluding, or
   use `--min-pass` in CI for a coarse gate.

Validation status: the unit tests exercise the full run path against an in-process fake hub; the harness has not
been run against a real hub or LLM, so field shapes (for example the manual-call result envelope) are handled
tolerantly and should be confirmed on first live use.
