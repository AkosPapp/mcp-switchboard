-- 003: server-side chat drafts. Text typed in a chat's composer but not sent,
-- kept so it survives a reload and follows the user across devices. One row per
-- chat; an empty draft has no row. Additive only.

CREATE TABLE chat_drafts (
    chat_id     TEXT PRIMARY KEY REFERENCES chats (id) ON DELETE CASCADE,
    content     TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
