-- 004: how an agent came to be, and the one MCP client it is bound to
-- (docs/AGENT_MODEL_API.md). Additive only: applies to a database at version 3
-- that holds data.
--
--   origin        'manual' (created by hand in the Chat panel), 'spawn' (created
--                 by another agent or as a child), 'chat' (the per-chat root
--                 agent that POST /api/chats used to create).
--   client_label  the one client the agent's grants name, NULL for none.

ALTER TABLE agents ADD COLUMN origin       TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE agents ADD COLUMN client_label TEXT;

-- Existing children are spawns; every existing root was made by a chat.
UPDATE agents SET origin = 'spawn' WHERE parent_id IS NOT NULL;

-- Client: from the agent's oldest chat that recorded one, else from its grants
-- when they allow exactly one concrete label.
UPDATE agents SET client_label = (
    SELECT c.client_label FROM chats c
    WHERE c.agent_id = agents.id AND c.client_label IS NOT NULL
    ORDER BY c.created_at, c.rowid LIMIT 1);

UPDATE agents SET client_label = (
    SELECT MIN(g.label) FROM grants g WHERE g.agent_id = agents.id AND g.allowed = 1)
WHERE client_label IS NULL
  AND (SELECT COUNT(DISTINCT g.label) FROM grants g WHERE g.agent_id = agents.id AND g.allowed = 1) = 1
  AND (SELECT MIN(g.label) FROM grants g WHERE g.agent_id = agents.id AND g.allowed = 1) <> '*';
