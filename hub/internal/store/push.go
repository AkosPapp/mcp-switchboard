package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const pushColumns = `id, endpoint, p256dh, auth, created_at, last_seen_at, label`

// UpsertPushSubscription inserts a new subscription, or - when the endpoint
// is already registered - refreshes its keys and last_seen_at while keeping
// the original id and created_at. SQLite's "ON CONFLICT ... DO UPDATE" makes
// this one statement instead of a select-then-branch.
func (s *SQLiteStore) UpsertPushSubscription(ctx context.Context, sub PushSubscription) (PushSubscription, error) {
	now := time.Now().UTC()
	created := sub.CreatedAt
	if created.IsZero() {
		created = now
	}
	lastSeen := sub.LastSeenAt
	if lastSeen.IsZero() {
		lastSeen = now
	}

	_, err := s.write.ExecContext(ctx,
		`INSERT INTO push_subscriptions (`+pushColumns+`)
		 VALUES (?,?,?,?,?,?,?)
		 ON CONFLICT (endpoint) DO UPDATE SET
		     p256dh = excluded.p256dh,
		     auth = excluded.auth,
		     last_seen_at = excluded.last_seen_at,
		     label = excluded.label`,
		sub.ID, sub.Endpoint, sub.P256dh, sub.Auth,
		FormatTime(created), FormatTime(lastSeen), nullableStringPtr(sub.Label),
	)
	if err != nil {
		return PushSubscription{}, fmt.Errorf("store: upserting push subscription: %w", err)
	}

	row := s.write.QueryRowContext(ctx,
		"SELECT "+pushColumns+" FROM push_subscriptions WHERE endpoint = ?", sub.Endpoint)
	rec, err := scanPushSubscription(row)
	if err != nil {
		return PushSubscription{}, fmt.Errorf("store: reading back push subscription: %w", err)
	}
	return rec, nil
}

// ListPushSubscriptions returns every registered subscription, oldest first.
func (s *SQLiteStore) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.read.QueryContext(ctx,
		"SELECT "+pushColumns+" FROM push_subscriptions ORDER BY created_at, rowid")
	if err != nil {
		return nil, fmt.Errorf("store: listing push subscriptions: %w", err)
	}
	defer rows.Close()

	out := make([]PushSubscription, 0, 8)
	for rows.Next() {
		rec, err := scanPushSubscription(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing push subscriptions: %w", err)
	}
	return out, nil
}

// DeletePushSubscription removes one subscription by id. Deleting a row that
// does not exist is not an error - the console may be asking the hub to
// forget a subscription it forgot already.
func (s *SQLiteStore) DeletePushSubscription(ctx context.Context, id string) error {
	if _, err := s.write.ExecContext(ctx,
		"DELETE FROM push_subscriptions WHERE id = ?", id); err != nil {
		return fmt.Errorf("store: deleting push subscription %s: %w", id, err)
	}
	return nil
}

// DeletePushSubscriptionByEndpoint removes a subscription by its endpoint,
// for cleanup after a push service reports the endpoint gone (404/410).
func (s *SQLiteStore) DeletePushSubscriptionByEndpoint(ctx context.Context, endpoint string) error {
	if _, err := s.write.ExecContext(ctx,
		"DELETE FROM push_subscriptions WHERE endpoint = ?", endpoint); err != nil {
		return fmt.Errorf("store: deleting push subscription for endpoint: %w", err)
	}
	return nil
}

func scanPushSubscription(sc scanner) (PushSubscription, error) {
	var (
		rec        PushSubscription
		createdAt  string
		lastSeenAt string
		label      sql.NullString
	)
	err := sc.Scan(&rec.ID, &rec.Endpoint, &rec.P256dh, &rec.Auth, &createdAt, &lastSeenAt, &label)
	if err != nil {
		if err == sql.ErrNoRows {
			return rec, err
		}
		return rec, fmt.Errorf("store: reading push subscription row: %w", err)
	}
	if t, ok := parseTime(createdAt); ok {
		rec.CreatedAt = t
	}
	if t, ok := parseTime(lastSeenAt); ok {
		rec.LastSeenAt = t
	}
	rec.Label = optional(label)
	return rec, nil
}

func nullableStringPtr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}
