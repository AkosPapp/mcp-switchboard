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

-- ---------------------------------------------------------------------------
-- The agent orchestrator (spec.md section 5.2).
--
-- Foreign keys are declared only where deleting the parent must delete the
-- child (hard delete, D10). Pointers that would form a cycle or that are
-- allowed to dangle - chats.active_leaf_id, messages.parent_id,
-- messages.last_active_child_id, messages.run_id, calls.*_id - are plain
-- columns; messages.run_id in particular is protected by the retention job
-- rather than by a constraint (D9).
-- ---------------------------------------------------------------------------

CREATE TABLE agents (
    id                 TEXT PRIMARY KEY,
    parent_id          TEXT REFERENCES agents (id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    description        TEXT NOT NULL DEFAULT '',
    project            TEXT,
    model              TEXT NOT NULL DEFAULT '{}',
    system_prompt      TEXT NOT NULL DEFAULT '',
    depth              INTEGER NOT NULL DEFAULT 0,
    status             TEXT NOT NULL DEFAULT 'idle',
    budget             TEXT NOT NULL DEFAULT '{}',
    capabilities       TEXT NOT NULL DEFAULT '{"can_spawn":false,"can_message":false}',
    approval           TEXT NOT NULL DEFAULT 'destructive',
    auto_wake          INTEGER NOT NULL DEFAULT 1,
    -- Cumulative, self plus every descendant (B5).
    token_total        INTEGER NOT NULL DEFAULT 0,
    cost_total_micros  INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL,
    last_activity_at   TEXT NOT NULL,
    deleted_at         TEXT
);

-- Partial and COALESCEd: parent_id is NULL for roots and SQLite treats NULLs as
-- distinct, and a soft-deleted sibling must not hold a name hostage (5.2).
CREATE UNIQUE INDEX agents_name_idx
    ON agents (COALESCE(parent_id, ''), name) WHERE deleted_at IS NULL;
CREATE INDEX agents_parent_idx ON agents (parent_id);

CREATE TABLE chats (
    id                 TEXT PRIMARY KEY,
    agent_id           TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    peer_agent_id      TEXT REFERENCES agents (id) ON DELETE SET NULL,
    title              TEXT NOT NULL DEFAULT '',
    kind               TEXT NOT NULL DEFAULT 'human',
    active_leaf_id     TEXT,
    tags               TEXT NOT NULL DEFAULT '[]',
    token_total        INTEGER NOT NULL DEFAULT 0,
    cost_total_micros  INTEGER NOT NULL DEFAULT 0,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL,
    archived_at        TEXT
);

CREATE INDEX chats_agent_idx   ON chats (agent_id, updated_at DESC);
CREATE INDEX chats_updated_idx ON chats (updated_at DESC);

-- seq is the rowid: stable insertion order (siblings are shown in it), and the
-- key the FTS index is bound to. An implicit rowid would be renumbered by
-- VACUUM and silently detach the index.
CREATE TABLE messages (
    seq                   INTEGER PRIMARY KEY AUTOINCREMENT,
    id                    TEXT NOT NULL UNIQUE,
    chat_id               TEXT NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    parent_id             TEXT,
    role                  TEXT NOT NULL,
    content               TEXT NOT NULL DEFAULT '[]',
    -- The text blocks of content, flattened by the store on insert; what the
    -- chat search (U6) indexes. Tool arguments and results are deliberately
    -- not searchable: they are large and mostly noise.
    search_text           TEXT NOT NULL DEFAULT '',
    tool_calls            TEXT,
    tool_results          TEXT,
    token_input           INTEGER NOT NULL DEFAULT 0,
    token_output          INTEGER NOT NULL DEFAULT 0,
    cost_micros           INTEGER NOT NULL DEFAULT 0,
    latency_ms            INTEGER NOT NULL DEFAULT 0,
    model                 TEXT,
    finish_reason         TEXT,
    run_id                TEXT,
    last_active_child_id  TEXT,
    created_at            TEXT NOT NULL
);

CREATE INDEX messages_chat_parent_idx ON messages (chat_id, parent_id);
CREATE INDEX messages_parent_idx      ON messages (parent_id);
CREATE INDEX messages_run_idx         ON messages (run_id) WHERE run_id IS NOT NULL;

-- Chat search (U6). External-content: the index stores no copy of the text.
CREATE VIRTUAL TABLE messages_fts USING fts5(
    search_text,
    content = 'messages',
    content_rowid = 'seq'
);

CREATE TRIGGER messages_fts_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts (rowid, search_text) VALUES (new.seq, new.search_text);
END;

CREATE TRIGGER messages_fts_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, search_text)
    VALUES ('delete', old.seq, old.search_text);
END;

CREATE TRIGGER messages_fts_au AFTER UPDATE OF search_text ON messages BEGIN
    INSERT INTO messages_fts (messages_fts, rowid, search_text)
    VALUES ('delete', old.seq, old.search_text);
    INSERT INTO messages_fts (rowid, search_text) VALUES (new.seq, new.search_text);
END;

CREATE TABLE edges (
    from_agent_id  TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    to_agent_id    TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    allowed        INTEGER NOT NULL,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (from_agent_id, to_agent_id)
);

CREATE INDEX edges_to_idx ON edges (to_agent_id);

CREATE TABLE grants (
    agent_id    TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    label       TEXT NOT NULL,
    project     TEXT NOT NULL DEFAULT '',
    server      TEXT NOT NULL,
    allowed     INTEGER NOT NULL,
    -- 'inherited', 'explicit' or 'human'.
    source      TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (agent_id, label, project, server)
);

CREATE TABLE runs (
    id                     TEXT PRIMARY KEY,
    agent_id               TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    chat_id                TEXT NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    "trigger"              TEXT NOT NULL,
    triggered_by_agent_id  TEXT,
    status                 TEXT NOT NULL,
    budget_snapshot        TEXT NOT NULL DEFAULT '{}',
    usage                  TEXT NOT NULL DEFAULT '{}',
    error                  TEXT,
    -- 'budget' for the refusal recorded by B5, and mirrors the last message's
    -- finish_reason otherwise.
    finish_reason          TEXT,
    -- Queue time. started_at is null until the run actually starts, and the
    -- difference is the delivery latency of B3.
    created_at             TEXT NOT NULL,
    created_at_epoch       REAL NOT NULL,
    started_at             TEXT,
    finished_at            TEXT,
    finished_at_epoch      REAL
);

CREATE INDEX runs_agent_idx  ON runs (agent_id, created_at_epoch DESC);
CREATE INDEX runs_chat_idx   ON runs (chat_id, created_at_epoch DESC);
CREATE INDEX runs_status_idx ON runs (status);

CREATE TABLE inbox (
    seq                 INTEGER PRIMARY KEY AUTOINCREMENT,
    id                  TEXT NOT NULL UNIQUE,
    agent_id            TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    from_agent_id       TEXT,
    chat_id             TEXT NOT NULL,
    message_id          TEXT NOT NULL,
    created_at          TEXT NOT NULL,
    delivered_at        TEXT,
    delivered_at_epoch  REAL
);

CREATE INDEX inbox_agent_idx ON inbox (agent_id, delivered_at);

-- R5: a repeated Idempotency-Key within its window returns the original run.
CREATE TABLE idempotency_keys (
    key               TEXT PRIMARY KEY,
    run_id            TEXT NOT NULL,
    created_at_epoch  REAL NOT NULL
);
