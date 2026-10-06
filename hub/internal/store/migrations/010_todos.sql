-- 010: per-chat TODO / plan list. The agent rewrites the whole list on every
-- update, so rows are only ever replaced as a set. Additive only.

CREATE TABLE chat_todos (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    chat_id     TEXT NOT NULL REFERENCES chats (id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    content     TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('pending', 'in_progress', 'completed')),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

CREATE INDEX chat_todos_chat ON chat_todos (chat_id, position);
