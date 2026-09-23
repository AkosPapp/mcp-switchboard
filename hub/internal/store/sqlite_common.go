package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// inTx runs fn in one transaction on the single write connection. Everything
// inside fn must use the tx, never s.write: the pool has one connection and a
// second query on it would wait for the transaction it is part of.
func (s *SQLiteStore) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func epoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

func timeOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now().UTC()
	}
	return t.UTC()
}

func mustTime(s string) time.Time {
	t, _ := parseTime(s)
	return t
}

func timePtr(s sql.NullString) *time.Time {
	if !s.Valid {
		return nil
	}
	t := mustTime(s.String)
	return &t
}

func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return FormatTime(*t)
}

func strArg(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// rawArg stores JSON as text, or NULL when there is none and no default.
func rawArg(r json.RawMessage, fallback string) any {
	if len(r) == 0 {
		if fallback == "" {
			return nil
		}
		return fallback
	}
	return string(r)
}

func rawFrom(s sql.NullString) json.RawMessage {
	if !s.Valid {
		return nil
	}
	return json.RawMessage(s.String)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	return min(limit, MaxLimit)
}

// placeholders returns "?,?,?" and the args for an IN list.
func inList(ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

func notFound(what, id string) error {
	return fmt.Errorf("%w: %s %s", ErrNotFound, what, id)
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func queryStrings(ctx context.Context, q queryer, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
