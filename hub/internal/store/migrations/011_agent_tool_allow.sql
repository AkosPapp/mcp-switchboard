-- 011: per-agent tool allowlist. A JSON array of glob patterns on the UPSTREAM
-- tool name (e.g. ["file_read","git_*"]) that narrows what the agent's
-- per-server grants already allow. NULL means no narrowing. switchboard.*
-- hub tools are not affected.

ALTER TABLE agents ADD COLUMN tool_allow TEXT;
