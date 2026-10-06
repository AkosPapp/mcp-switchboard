-- Chat-level conversation preferences (docs/improvements.md, Oct 2026):
--   model_pref      JSON model config chosen when the chat is created; it
--                   overrides the profile's default model for every turn
--                   (a per-message override still beats it) (I2).
--   context_limit   prompt-token budget within the model's window; drives the
--                   context meter and auto-compaction (I4, I5, I11).
--   approval        per-chat approval mode, overriding the owning agent's
--                   (empty/NULL = agent default) (I3).
--   auto_approve    no tool call pauses for a human, irreversible ones
--                   included — the user's explicit decision (I1).
--   summary / summarized_at / summarize_upto_msg
--                   the latest context compression: a summary standing in for
--                   every active-path message up to (and including) the named
--                   message; empty summary = nothing compressed yet (I11).
--   effort          reasoning effort the composer sends with each turn
--                   (empty = the model default) (I12).
ALTER TABLE chats ADD COLUMN model_pref   TEXT;
ALTER TABLE chats ADD COLUMN context_limit INTEGER;
ALTER TABLE chats ADD COLUMN approval     TEXT;
ALTER TABLE chats ADD COLUMN auto_approve INTEGER NOT NULL DEFAULT 0;
ALTER TABLE chats ADD COLUMN summary            TEXT;
ALTER TABLE chats ADD COLUMN summarized_at      TEXT;
ALTER TABLE chats ADD COLUMN summarize_upto_msg TEXT;
ALTER TABLE chats ADD COLUMN effort             TEXT;
