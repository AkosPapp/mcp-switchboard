-- 014: the agent's own persistent notes for a chat (capability pass, Oct 2026).
-- The agent writes what it LEARNS about the project through
-- switchboard.note.append — conventions, environment traps, decisions — and the
-- hub re-injects the whole block into every later system prompt, so knowledge
-- outlives the context window (compaction summarizes; notes persist). The tool
-- layer caps the body at 16 KB. "" = the chat keeps no notes.

ALTER TABLE chats ADD COLUMN notes TEXT NOT NULL DEFAULT '';
