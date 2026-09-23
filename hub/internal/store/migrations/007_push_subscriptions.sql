-- Web Push subscriptions (spec.md 8.7). One row per browser subscription the
-- console has registered on this device. `endpoint` is unique because
-- re-subscribing the same browser (a permission re-prompt, a service worker
-- update) must update the existing row rather than accumulate duplicates that
-- would each get a separate, redundant notification.

CREATE TABLE push_subscriptions (
    id             TEXT PRIMARY KEY,
    endpoint       TEXT NOT NULL UNIQUE,
    p256dh         TEXT NOT NULL,
    auth           TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    last_seen_at   TEXT NOT NULL,
    -- Operator-facing only ("Akos's phone"); never required.
    label          TEXT
);
