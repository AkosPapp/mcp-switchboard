-- For a JSON export to be sufficient to reproduce a conversation byte for
-- byte (and therefore usable for provider-side prompt-cache-friendly replay),
-- an assistant message must carry the exact system prompt and tool
-- definitions it was generated against - not just what a chat resolves to
-- *now* (its profile/grants can change after the fact). `model` already
-- existed but only ever held {provider, model}; from now on it holds the
-- full turn options (max_tokens, temperature, thinking) actually sent.

ALTER TABLE messages ADD COLUMN system_prompt TEXT;
ALTER TABLE messages ADD COLUMN tools TEXT;
