-- 015: the /optimize_skills unlock (docs/improvements.md follow-up). A chat
-- starts with no self-modification tools; running /optimize_skills sets this
-- flag and the switchboard.optimize.* tools (read other conversations, edit
-- prompts and skills) appear in that chat's catalog until it is switched off.
-- 0 = locked (the default everywhere, always).

ALTER TABLE chats ADD COLUMN optimize INTEGER NOT NULL DEFAULT 0;
