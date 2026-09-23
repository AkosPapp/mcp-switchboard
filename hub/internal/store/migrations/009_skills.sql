-- 009: skills. A skill is a named, reusable block of instructions: the user
-- runs it with a /name command, and (when `auto` is set) the model may load it
-- itself, after seeing its name and description in the system prompt.

CREATE TABLE skills (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    description  TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL DEFAULT '',
    auto         INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
