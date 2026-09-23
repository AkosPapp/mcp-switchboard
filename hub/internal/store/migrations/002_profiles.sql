-- 002: agent profiles, per-chat client, tool inspection (docs/PROFILES_API.md).
--
-- A profile is a reusable template for the working agent a chat is created
-- with. It is not an agent instance and carries no grants. Additive only: it
-- applies cleanly to a database that is already at version 1 and holds data.

CREATE TABLE profiles (
    id             TEXT PRIMARY KEY,
    name           TEXT NOT NULL UNIQUE,
    description    TEXT NOT NULL DEFAULT '',
    system_prompt  TEXT NOT NULL DEFAULT '',
    -- {provider, model, ...}; NULL means "the first configured model".
    model          TEXT,
    capabilities   TEXT NOT NULL DEFAULT '{"can_spawn":false,"can_message":false}',
    approval       TEXT NOT NULL DEFAULT 'destructive',
    budget         TEXT NOT NULL DEFAULT '{}',
    is_default     INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

-- At most one default profile, enforced by the schema.
CREATE UNIQUE INDEX profiles_default_idx ON profiles (is_default) WHERE is_default = 1;

-- Deleting a profile leaves the chats and agents made from it untouched.
ALTER TABLE chats  ADD COLUMN profile_id   TEXT REFERENCES profiles (id) ON DELETE SET NULL;
ALTER TABLE chats  ADD COLUMN client_label TEXT;
ALTER TABLE agents ADD COLUMN profile_id   TEXT REFERENCES profiles (id) ON DELETE SET NULL;
