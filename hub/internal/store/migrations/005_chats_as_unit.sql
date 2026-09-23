-- 005: chats are the unit (docs/CHAT_MODEL_API.md). Additive only: applies to a
-- database at version 4 that holds data, and touches no existing row except the
-- best-effort backfill of chats.parent_chat_id.
--
--   chats.parent_chat_id  the chat that spawned this one (sub-chat nesting in the
--                         console). Plain column, no foreign key: a trigger below
--                         clears it when the parent goes away.
--   messages.sender       JSON {chatId, chatTitle, kind} on a user-role message
--                         injected by another chat (kind: message | reply | spawn);
--                         NULL on everything a human or a model wrote.

ALTER TABLE chats ADD COLUMN parent_chat_id TEXT;
ALTER TABLE messages ADD COLUMN sender TEXT;

-- Backfill: a legacy 'spawn' chat (the child's chat recording its parent as
-- peer) hangs under the oldest human chat of that parent agent. Everything else
-- stays NULL.
UPDATE chats SET parent_chat_id = (
    SELECT h.id FROM chats h
    WHERE h.agent_id = chats.peer_agent_id AND h.kind = 'human'
    ORDER BY h.created_at, h.rowid LIMIT 1)
WHERE kind = 'spawn' AND peer_agent_id IS NOT NULL;

CREATE INDEX chats_parent_idx ON chats (parent_chat_id) WHERE parent_chat_id IS NOT NULL;

CREATE TRIGGER chats_parent_ad AFTER DELETE ON chats BEGIN
    UPDATE chats SET parent_chat_id = NULL WHERE parent_chat_id = old.id;
END;
