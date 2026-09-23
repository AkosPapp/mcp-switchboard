package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// driverName is the pure-Go driver (spec.md E1). It is pinned by name because
// CGO_ENABLED=0 is a build requirement and because the chat search of U6 needs
// FTS5, which pure-Go drivers differ on shipping; the test suite asserts an
// FTS5 query works, so swapping the driver fails CI rather than quietly
// emptying the search box.
const driverName = "sqlite"

// Options configure a SQLite store. The zero value is usable apart from Path.
type Options struct {
	Path string

	// RetentionDays and MaxRows bound the call log twice over, by age and by
	// size, so an unattended hub cannot grow its database without limit (D9).
	// Either at zero disables that half of the rule.
	RetentionDays int
	MaxRows       int

	// InboxRetentionDays is how long delivered mailbox rows are kept (6.3);
	// zero means DefaultInboxRetentionDays.
	InboxRetentionDays int

	// ReadConns sizes the reader pool. WAL lets readers run concurrently with
	// the writer, so this is just "how many console requests may hit the disk
	// at once".
	ReadConns int
}

// Defaults matching the settings the hub exposes (§6.3).
const (
	DefaultRetentionDays = 30
	DefaultMaxRows       = 100_000
	// DefaultInboxRetentionDays is INBOX_RETENTION_DAYS' default (6.3).
	DefaultInboxRetentionDays = 7
	defaultReadConns          = 8
)

// SQLiteStore is the Store implementation. It holds two handles over one file:
// a write handle capped at a single connection and a pool of readers. WAL gives
// concurrent readers, and serialising writes onto one connection removes
// SQLITE_BUSY from the write path entirely rather than retrying around it (D3).
type SQLiteStore struct {
	path   string
	write  *sql.DB
	read   *sql.DB
	opts   Options
	closed bool
}

var _ Store = (*SQLiteStore)(nil)

// Open prepares the database file, applies migrations and returns a ready
// store. It is the only place that runs migrations, and it runs them on the
// write handle before the readers are handed out.
func Open(ctx context.Context, opts Options) (*SQLiteStore, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("store: no database path given")
	}
	if opts.RetentionDays == 0 {
		opts.RetentionDays = DefaultRetentionDays
	}
	if opts.MaxRows == 0 {
		opts.MaxRows = DefaultMaxRows
	}
	if opts.InboxRetentionDays == 0 {
		opts.InboxRetentionDays = DefaultInboxRetentionDays
	}
	if opts.ReadConns <= 0 {
		opts.ReadConns = defaultReadConns
	}
	if dir := filepath.Dir(opts.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: cannot create %s: %w", dir, err)
		}
	}

	write, err := sql.Open(driverName, dsn(opts.Path))
	if err != nil {
		return nil, fmt.Errorf("store: opening %s: %w", opts.Path, err)
	}
	write.SetMaxOpenConns(1)
	// The one write connection is worth keeping alive: reopening it would mean
	// re-running every PRAGMA on the next insert.
	write.SetMaxIdleConns(1)
	write.SetConnMaxLifetime(0)

	if err := write.PingContext(ctx); err != nil {
		write.Close()
		return nil, fmt.Errorf("store: opening %s: %w", opts.Path, err)
	}

	migrations, err := loadMigrations(migrationFS, migrationDir)
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := migrate(ctx, write, opts.Path, migrations); err != nil {
		write.Close()
		return nil, fmt.Errorf("store: %w", err)
	}

	read, err := sql.Open(driverName, dsn(opts.Path))
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("store: opening %s for reading: %w", opts.Path, err)
	}
	read.SetMaxOpenConns(opts.ReadConns)
	read.SetMaxIdleConns(opts.ReadConns)

	return &SQLiteStore{path: opts.Path, write: write, read: read, opts: opts}, nil
}

// dsn spells out D3's pragmas. They are per-connection settings (journal_mode
// aside, which is persistent), so they belong in the DSN where every connection
// the pool opens gets them, not in a one-off statement after Open.
func dsn(path string) string {
	pragmas := []string{
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"foreign_keys(ON)",
		"busy_timeout(5000)",
	}
	q := make(url.Values)
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	return "file:" + path + "?" + q.Encode()
}

// Close releases both handles.
func (s *SQLiteStore) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	readErr := s.read.Close()
	writeErr := s.write.Close()
	if writeErr != nil {
		return writeErr
	}
	return readErr
}

// Path is the database file, for error messages and for stat.
func (s *SQLiteStore) Path() string { return s.path }

const callColumns = `id, connection_id, label, server, tool, exposed_name, arguments, result,
	error, status, source, started_at, started_at_epoch, duration_ms, agent_id, chat_id, run_id`

// RecordCall appends one call. INSERT OR REPLACE because a caller may record an
// in-flight call and then record it again when it finishes.
func (s *SQLiteStore) RecordCall(ctx context.Context, rec CallRecord) error {
	started := rec.StartedAt.UTC()
	if started.IsZero() {
		started = time.Now().UTC()
	}
	_, err := s.write.ExecContext(ctx,
		`INSERT OR REPLACE INTO calls (`+callColumns+`)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.ConnectionID, rec.Label, rec.Server, rec.Tool, rec.ExposedName,
		encodeJSON(rec.Arguments), nullableJSON(rec.Result), nullableString(rec.Error),
		rec.Status, rec.Source,
		FormatTime(started), float64(started.UnixNano())/1e9, rec.DurationMs,
		rec.AgentID, rec.ChatID, rec.RunID,
	)
	if err != nil {
		return fmt.Errorf("store: recording call %s: %w", rec.ID, err)
	}
	return nil
}

// ListCalls returns matching rows, newest first.
func (s *SQLiteStore) ListCalls(ctx context.Context, f CallFilter) ([]CallRecord, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}

	var where []string
	var args []any
	for _, c := range []struct {
		column string
		value  string
	}{
		{"server", f.Server},
		{"tool", f.Tool},
		{"status", f.Status},
		{"label", f.Label},
		{"source", f.Source},
		{"agent_id", f.AgentID},
		{"chat_id", f.ChatID},
	} {
		if c.value != "" {
			where = append(where, c.column+" = ?")
			args = append(args, c.value)
		}
	}

	query := "SELECT " + callColumns + " FROM calls"
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	// rowid breaks ties. Timestamps do collide - two calls dispatched in the
	// same millisecond are ordinary - and without a total order a row can move
	// between pages and be returned twice or skipped.
	query += " ORDER BY started_at_epoch DESC, rowid DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing calls: %w", err)
	}
	defer rows.Close()

	out := make([]CallRecord, 0, min(limit, 128))
	for rows.Next() {
		rec, err := scanCall(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: listing calls: %w", err)
	}
	return out, nil
}

// GetCall returns nil when there is no such call - a missing call is an
// ordinary answer to the console, not a failure.
func (s *SQLiteStore) GetCall(ctx context.Context, id string) (*CallRecord, error) {
	row := s.read.QueryRowContext(ctx, "SELECT "+callColumns+" FROM calls WHERE id = ?", id)
	rec, err := scanCall(row)
	switch {
	case err == sql.ErrNoRows:
		return nil, nil
	case err != nil:
		return nil, err
	}
	return &rec, nil
}

// PurgeCalls applies both retention rules in one transaction and returns the
// rows deleted. Age alone cannot bound a hub that gets hammered for a week, and
// a row cap alone cannot bound how stale the oldest row is; together they do.
func (s *SQLiteStore) PurgeCalls(ctx context.Context) (int64, error) {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: purging calls: %w", err)
	}
	defer tx.Rollback()

	var deleted int64
	if s.opts.RetentionDays > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -s.opts.RetentionDays)
		res, err := tx.ExecContext(ctx,
			"DELETE FROM calls WHERE started_at_epoch < ?", float64(cutoff.UnixNano())/1e9)
		if err != nil {
			return 0, fmt.Errorf("store: purging calls by age: %w", err)
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	if s.opts.MaxRows > 0 {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM calls WHERE rowid NOT IN (
			     SELECT rowid FROM calls
			     ORDER BY started_at_epoch DESC, rowid DESC
			     LIMIT ?
			 )`, s.opts.MaxRows)
		if err != nil {
			return 0, fmt.Errorf("store: purging calls by count: %w", err)
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: purging calls: %w", err)
	}
	return deleted, nil
}

// CallStats counts the call log by status.
func (s *SQLiteStore) CallStats(ctx context.Context) (CallStats, error) {
	rows, err := s.read.QueryContext(ctx, "SELECT status, COUNT(*) FROM calls GROUP BY status")
	if err != nil {
		return CallStats{}, fmt.Errorf("store: call stats: %w", err)
	}
	defer rows.Close()

	var out CallStats
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return CallStats{}, fmt.Errorf("store: call stats: %w", err)
		}
		out.Total += n
		switch status {
		case StatusOK:
			out.OK = n
		case StatusError:
			out.Error = n
		}
	}
	if err := rows.Err(); err != nil {
		return CallStats{}, fmt.Errorf("store: call stats: %w", err)
	}
	return out, nil
}

// Stats answers /api/stats: call counts, bytes on disk and a row count per
// table. Chats are never pruned (D11), so these numbers are the only warning a
// deployment gets before the volume fills.
func (s *SQLiteStore) Stats(ctx context.Context) (Stats, error) {
	calls, err := s.CallStats(ctx)
	if err != nil {
		return Stats{}, err
	}
	out := Stats{Calls: calls, Tables: map[string]int64{}}

	for _, suffix := range []string{"", "-wal", "-shm"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			out.DatabaseBytes += info.Size()
		}
	}

	names, err := s.tableNames(ctx)
	if err != nil {
		return Stats{}, err
	}
	for _, name := range names {
		var n int64
		// The name comes from sqlite_master, not from a caller, and SQLite has
		// no placeholder for an identifier; quoting it is the whole defence.
		if err := s.read.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&n); err != nil {
			return Stats{}, fmt.Errorf("store: counting %s: %w", name, err)
		}
		out.Tables[name] = n
	}
	return out, nil
}

func (s *SQLiteStore) tableNames(ctx context.Context) ([]string, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		 ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing tables: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: listing tables: %w", err)
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// scanner is satisfied by both *sql.Row and *sql.Rows, so one scan covers Get
// and List.
type scanner interface{ Scan(dest ...any) error }

func scanCall(sc scanner) (CallRecord, error) {
	var (
		rec          CallRecord
		arguments    string
		result       sql.NullString
		errMsg       sql.NullString
		startedAt    string
		startedEpoch float64
		agentID      sql.NullString
		chatID       sql.NullString
		runID        sql.NullString
	)
	err := sc.Scan(
		&rec.ID, &rec.ConnectionID, &rec.Label, &rec.Server, &rec.Tool, &rec.ExposedName,
		&arguments, &result, &errMsg, &rec.Status, &rec.Source,
		&startedAt, &startedEpoch, &rec.DurationMs, &agentID, &chatID, &runID,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return rec, err
		}
		return rec, fmt.Errorf("store: reading call row: %w", err)
	}

	rec.Arguments = decodeObject(arguments, map[string]any{})
	if result.Valid {
		rec.Result = decodeObject(result.String, nil)
	}
	rec.Error = errMsg.String
	// The text column is authoritative; the epoch is the fallback for a row
	// whose timestamp was written by something that did not use FormatTime.
	if t, ok := parseTime(startedAt); ok {
		rec.StartedAt = t
	} else {
		rec.StartedAt = time.Unix(0, int64(startedEpoch*1e9)).UTC()
	}
	rec.AgentID = optional(agentID)
	rec.ChatID = optional(chatID)
	rec.RunID = optional(runID)
	return rec, nil
}

func optional(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// encodeJSON never fails: history must not break a tool call. A value that will
// not marshal is stored as a description of itself, which is strictly better
// than losing the row.
func encodeJSON(v any) string {
	if v == nil {
		return "{}"
	}
	data, err := json.Marshal(v)
	if err != nil {
		data, _ = json.Marshal(map[string]string{"unserializable": fmt.Sprintf("%#v", v)})
	}
	return string(data)
}

func nullableJSON(v map[string]any) any {
	if v == nil {
		return nil
	}
	return encodeJSON(v)
}

func decodeObject(raw string, fallback map[string]any) map[string]any {
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fallback
	}
	return out
}
