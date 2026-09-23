-- 006: edges become symmetric (spec.md D14, docs/CHAT_MODEL_API.md). One row
-- stands for an unordered pair, stored with the lexicographically smaller id
-- first, so `(a,b)` and `(b,a)` are the same row and `allowed` governs both
-- directions. SQLite cannot rename a primary key column set in place, so the
-- table is rebuilt; any pre-existing directional rows are folded into their
-- canonical pair, preferring allowed=1 if either direction had it and keeping
-- the earliest created_at / latest updated_at.

CREATE TABLE edges_new (
    agent_a_id     TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    agent_b_id     TEXT NOT NULL REFERENCES agents (id) ON DELETE CASCADE,
    allowed        INTEGER NOT NULL,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (agent_a_id, agent_b_id),
    CHECK (agent_a_id < agent_b_id)
);

INSERT INTO edges_new (agent_a_id, agent_b_id, allowed, created_at, updated_at)
SELECT
    MIN(from_agent_id, to_agent_id) AS a,
    MAX(from_agent_id, to_agent_id) AS b,
    MAX(allowed),
    MIN(created_at),
    MAX(updated_at)
FROM edges
WHERE from_agent_id <> to_agent_id
GROUP BY a, b;

DROP TABLE edges;
ALTER TABLE edges_new RENAME TO edges;

CREATE INDEX edges_b_idx ON edges (agent_b_id);
