-- The whole schema, as of the Go rewrite.
--
-- This file keeps growing until this revision ships. There is no deployed Go
-- database yet, so editing 001 in place is cheaper and far clearer than a chain
-- of migrations that exists only to record what we changed our minds about
-- during development. From the release onwards 001 is frozen and every further
-- change is 002, 003, ... - real migrations against real data.
--
-- Nothing here uses IF NOT EXISTS. A migration runs exactly once against a
-- database at the version below it; a table that already exists is a bug in the
-- runner, and IF NOT EXISTS would hide it until the first failing insert.

CREATE TABLE calls (
    id                TEXT PRIMARY KEY,
    connection_id     TEXT NOT NULL,
    label             TEXT NOT NULL,
    server            TEXT NOT NULL,
    tool              TEXT NOT NULL,
    exposed_name      TEXT NOT NULL,
    arguments         TEXT NOT NULL,
    result            TEXT,
    error             TEXT,
    -- 'ok', 'error' or 'denied'. Left as free text rather than a CHECK: a new
    -- status value should be a code change, not a migration, and the console
    -- renders an unknown one verbatim instead of the row vanishing.
    status            TEXT NOT NULL,
    -- 'console', 'mcp', 'api' or 'agent'.
    source            TEXT NOT NULL,
    -- Both representations of the same instant: the text is what the API
    -- serves verbatim, the epoch is what every ORDER BY and retention sweep
    -- compares, because comparing ISO-8601 strings only works by accident.
    started_at        TEXT NOT NULL,
    started_at_epoch  REAL NOT NULL,
    duration_ms       REAL NOT NULL,
    -- Set only for calls the orchestrator made on an agent's behalf (R1), so
    -- the Calls view can be filtered down to one agent, chat or run. Nullable
    -- because console, MCP and API calls have no run to belong to.
    agent_id          TEXT,
    chat_id           TEXT,
    run_id            TEXT
);

CREATE INDEX calls_started_at_idx ON calls (started_at_epoch DESC);
CREATE INDEX calls_server_idx     ON calls (server);
CREATE INDEX calls_tool_idx       ON calls (tool);
CREATE INDEX calls_status_idx     ON calls (status);
CREATE INDEX calls_label_idx      ON calls (label);
CREATE INDEX calls_source_idx     ON calls (source);

-- Partial: the overwhelming majority of rows are not agent calls, and an index
-- that skips them stays small enough to be worth having.
CREATE INDEX calls_agent_idx ON calls (agent_id) WHERE agent_id IS NOT NULL;
CREATE INDEX calls_chat_idx  ON calls (chat_id)  WHERE chat_id  IS NOT NULL;
CREATE INDEX calls_run_idx   ON calls (run_id)   WHERE run_id   IS NOT NULL;
