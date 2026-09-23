package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const runCols = `id, agent_id, chat_id, "trigger", triggered_by_agent_id, status, budget_snapshot, usage,
	error, finish_reason, created_at, started_at, finished_at`

func scanRun(sc scanner) (Run, error) {
	var (
		r                                     Run
		by, errMsg, finish, started, finished sql.NullString
		budget, usage, created                string
	)
	err := sc.Scan(&r.ID, &r.AgentID, &r.ChatID, &r.Trigger, &by, &r.Status, &budget, &usage,
		&errMsg, &finish, &created, &started, &finished)
	if err != nil {
		return r, err
	}
	r.TriggeredByAgentID, r.Error, r.FinishReason = optional(by), optional(errMsg), optional(finish)
	r.BudgetSnapshot, r.Usage = json.RawMessage(budget), json.RawMessage(usage)
	r.CreatedAt, r.StartedAt, r.FinishedAt = mustTime(created), timePtr(started), timePtr(finished)
	return r, nil
}

func getRunTx(ctx context.Context, q queryer, id string) (*Run, error) {
	r, err := scanRun(q.QueryRowContext(ctx, "SELECT "+runCols+" FROM runs WHERE id = ?", id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading run %s: %w", id, err)
	}
	return &r, nil
}

func (s *SQLiteStore) CreateRun(ctx context.Context, r Run) (Run, error) {
	if r.ID == "" {
		r.ID = NewID()
	}
	if r.Status == "" {
		r.Status = RunQueued
	}
	r.CreatedAt = timeOrNow(r.CreatedAt)
	var finishedEpoch any
	if r.FinishedAt != nil {
		finishedEpoch = epoch(*r.FinishedAt)
	}
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO runs (`+runCols+`, created_at_epoch, finished_at_epoch)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.AgentID, r.ChatID, r.Trigger, strArg(r.TriggeredByAgentID), r.Status,
		rawArg(r.BudgetSnapshot, "{}"), rawArg(r.Usage, "{}"), strArg(r.Error), strArg(r.FinishReason),
		FormatTime(r.CreatedAt), timeArg(r.StartedAt), timeArg(r.FinishedAt),
		epoch(r.CreatedAt), finishedEpoch)
	if err != nil {
		return Run{}, fmt.Errorf("store: creating run: %w", err)
	}
	r.BudgetSnapshot, r.Usage = rawOr(r.BudgetSnapshot, "{}"), rawOr(r.Usage, "{}")
	return r, nil
}

func (s *SQLiteStore) GetRun(ctx context.Context, id string) (*Run, error) {
	return getRunTx(ctx, s.read, id)
}

func (s *SQLiteStore) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	var where []string
	var args []any
	if f.AgentID != "" {
		where, args = append(where, "agent_id = ?"), append(args, f.AgentID)
	}
	if f.ChatID != "" {
		where, args = append(where, "chat_id = ?"), append(args, f.ChatID)
	}
	if len(f.Status) > 0 {
		list, a := inList(f.Status)
		where, args = append(where, "status IN ("+list+")"), append(args, a...)
	}
	q := "SELECT " + runCols + " FROM runs"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at_epoch DESC, rowid DESC LIMIT ? OFFSET ?"
	args = append(args, clampLimit(f.Limit), max(f.Offset, 0))
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing runs: %w", err)
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading run row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateRun(ctx context.Context, id string, p RunPatch) (Run, error) {
	var sets []string
	var args []any
	add := func(col string, v any) { sets, args = append(sets, col+" = ?"), append(args, v) }
	if p.Status != nil {
		add("status", *p.Status)
	}
	if len(p.Usage) > 0 {
		add("usage", string(p.Usage))
	}
	if p.Error != nil {
		add("error", *p.Error)
	}
	if p.FinishReason != nil {
		add("finish_reason", *p.FinishReason)
	}
	if p.StartedAt != nil {
		add("started_at", FormatTime(*p.StartedAt))
	}
	if p.FinishedAt != nil {
		add("finished_at", FormatTime(*p.FinishedAt))
		add("finished_at_epoch", epoch(*p.FinishedAt))
	}
	var out *Run
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if len(sets) > 0 {
			res, err := tx.ExecContext(ctx, "UPDATE runs SET "+strings.Join(sets, ", ")+" WHERE id = ?", append(args, id)...)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return notFound("run", id)
			}
		}
		var err error
		out, err = getRunTx(ctx, tx, id)
		if err == nil && out == nil {
			err = notFound("run", id)
		}
		return err
	})
	if err != nil {
		return Run{}, fmt.Errorf("store: updating run %s: %w", id, err)
	}
	return *out, nil
}

func (s *SQLiteStore) CountRunning(ctx context.Context) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM runs WHERE status = 'running'").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting running runs: %w", err)
	}
	return n, nil
}

// InterruptedMessage is the error text written on runs cut short by a restart (A3).
const InterruptedMessage = "interrupted: the hub restarted while this run was in progress"

func (s *SQLiteStore) InterruptRunning(ctx context.Context) (int64, error) {
	var n int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		res, err := tx.ExecContext(ctx,
			`UPDATE runs SET status = 'interrupted', error = ?, finished_at = ?, finished_at_epoch = ?
			 WHERE status IN ('queued','running','waiting')`,
			InterruptedMessage, FormatTime(now), epoch(now))
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		_, err = tx.ExecContext(ctx,
			"UPDATE agents SET status = 'idle', updated_at = ? WHERE status IN ('running','waiting','blocked')",
			FormatTime(now))
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: interrupting runs: %w", err)
	}
	return n, nil
}

func (s *SQLiteStore) PurgeRuns(ctx context.Context) (int64, error) {
	if s.opts.RetentionDays <= 0 {
		return 0, nil
	}
	cutoff := epoch(time.Now().UTC().AddDate(0, 0, -s.opts.RetentionDays))
	var n int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// Only finished runs, and never one a message still points at (D9).
		res, err := tx.ExecContext(ctx,
			`DELETE FROM runs
			 WHERE status IN ('done','cancelled','interrupted','error')
			   AND COALESCE(finished_at_epoch, created_at_epoch) < ?
			   AND NOT EXISTS (SELECT 1 FROM messages WHERE messages.run_id = runs.id)`, cutoff)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		_, err = tx.ExecContext(ctx, "DELETE FROM idempotency_keys WHERE created_at_epoch < ?",
			epoch(time.Now().Add(-IdempotencyWindow)))
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: purging runs: %w", err)
	}
	return n, nil
}

const inboxCols = "id, agent_id, from_agent_id, chat_id, message_id, created_at, delivered_at"

func scanInbox(sc scanner) (InboxItem, error) {
	var it InboxItem
	var from, delivered sql.NullString
	var created string
	if err := sc.Scan(&it.ID, &it.AgentID, &from, &it.ChatID, &it.MessageID, &created, &delivered); err != nil {
		return it, err
	}
	it.FromAgentID, it.CreatedAt, it.DeliveredAt = optional(from), mustTime(created), timePtr(delivered)
	return it, nil
}

func (s *SQLiteStore) EnqueueInbox(ctx context.Context, it InboxItem) (InboxItem, error) {
	if it.ID == "" {
		it.ID = NewID()
	}
	it.CreatedAt, it.DeliveredAt = timeOrNow(it.CreatedAt), nil
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO inbox (id, agent_id, from_agent_id, chat_id, message_id, created_at) VALUES (?,?,?,?,?,?)`,
		it.ID, it.AgentID, strArg(it.FromAgentID), it.ChatID, it.MessageID, FormatTime(it.CreatedAt))
	if err != nil {
		return InboxItem{}, fmt.Errorf("store: enqueueing inbox item: %w", err)
	}
	return it, nil
}

func (s *SQLiteStore) DrainInbox(ctx context.Context, agentID string) ([]InboxItem, error) {
	type seqItem struct {
		seq  int64
		item InboxItem
	}
	var got []seqItem
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		rows, err := tx.QueryContext(ctx,
			`UPDATE inbox SET delivered_at = ?, delivered_at_epoch = ?
			 WHERE agent_id = ? AND delivered_at IS NULL RETURNING seq, `+inboxCols,
			FormatTime(now), epoch(now), agentID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var si seqItem
			var from sql.NullString
			var created string
			if err := rows.Scan(&si.seq, &si.item.ID, &si.item.AgentID, &from, &si.item.ChatID,
				&si.item.MessageID, &created, new(sql.NullString)); err != nil {
				return err
			}
			si.item.FromAgentID, si.item.CreatedAt = optional(from), mustTime(created)
			d := now
			si.item.DeliveredAt = &d
			got = append(got, si)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: draining inbox: %w", err)
	}
	// RETURNING order is unspecified; seq is arrival order.
	sort.Slice(got, func(i, j int) bool { return got[i].seq < got[j].seq })
	out := make([]InboxItem, len(got))
	for i, g := range got {
		out[i] = g.item
	}
	return out, nil
}

func (s *SQLiteStore) CountInbox(ctx context.Context, agentID string) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM inbox WHERE agent_id = ? AND delivered_at IS NULL", agentID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting inbox: %w", err)
	}
	return n, nil
}

func (s *SQLiteStore) PruneInbox(ctx context.Context) (int64, error) {
	if s.opts.InboxRetentionDays <= 0 {
		return 0, nil
	}
	cutoff := epoch(time.Now().UTC().AddDate(0, 0, -s.opts.InboxRetentionDays))
	res, err := s.write.ExecContext(ctx,
		"DELETE FROM inbox WHERE delivered_at IS NOT NULL AND delivered_at_epoch < ?", cutoff)
	if err != nil {
		return 0, fmt.Errorf("store: pruning inbox: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *SQLiteStore) ClaimIdempotencyKey(ctx context.Context, key, runID string) (string, bool, error) {
	var existing string
	claimed := false
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		now := time.Now().UTC()
		var at float64
		err := tx.QueryRowContext(ctx,
			"SELECT run_id, created_at_epoch FROM idempotency_keys WHERE key = ?", key).Scan(&existing, &at)
		switch {
		case err == nil && epoch(now)-at < IdempotencyWindow.Seconds():
			return nil // still fresh: the original run wins
		case err != nil && !isNoRows(err):
			return err
		}
		claimed = true
		_, err = tx.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key, run_id, created_at_epoch) VALUES (?,?,?)
			 ON CONFLICT (key) DO UPDATE SET run_id = excluded.run_id, created_at_epoch = excluded.created_at_epoch`,
			key, runID, epoch(now))
		return err
	})
	if err != nil {
		return "", false, fmt.Errorf("store: claiming idempotency key: %w", err)
	}
	if claimed {
		return "", true, nil
	}
	return existing, false, nil
}
