package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Todo statuses.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// Todo is one item of a chat's plan list.
type Todo struct {
	ID        int64     `json:"id"`
	Content   string    `json:"content"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// TodoItem is the input of ReplaceTodos.
type TodoItem struct {
	Content string
	Status  string
}

// TodoStore persists per-chat plan lists.
type TodoStore interface {
	// ListTodos returns the chat's items in order (empty, not an error, when
	// there are none).
	ListTodos(ctx context.Context, chatID string) ([]Todo, error)
	// ReplaceTodos atomically replaces the whole list. ErrNotFound for an
	// unknown chat, ErrInvalid for an empty content or unknown status. Items
	// whose content is unchanged keep their created_at.
	ReplaceTodos(ctx context.Context, chatID string, items []TodoItem) ([]Todo, error)
}

func (s *SQLiteStore) ListTodos(ctx context.Context, chatID string) ([]Todo, error) {
	out, err := listTodos(ctx, s.read, chatID)
	if err != nil {
		return nil, fmt.Errorf("store: listing todos of %s: %w", chatID, err)
	}
	return out, nil
}

type todoQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func listTodos(ctx context.Context, q todoQuerier, chatID string) ([]Todo, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT id, content, status, created_at, updated_at FROM chat_todos WHERE chat_id = ? ORDER BY position, id", chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Todo{}
	for rows.Next() {
		var t Todo
		var created, updated string
		if err := rows.Scan(&t.ID, &t.Content, &t.Status, &created, &updated); err != nil {
			return nil, err
		}
		t.CreatedAt, t.UpdatedAt = mustTime(created), mustTime(updated)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ReplaceTodos(ctx context.Context, chatID string, items []TodoItem) ([]Todo, error) {
	for _, it := range items {
		if it.Content == "" {
			return nil, fmt.Errorf("%w: todo content is empty", ErrInvalid)
		}
		switch it.Status {
		case TodoPending, TodoInProgress, TodoCompleted:
		default:
			return nil, fmt.Errorf("%w: unknown todo status %q", ErrInvalid, it.Status)
		}
	}
	var out []Todo
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM chats WHERE id = ?", chatID).Scan(&one); err != nil {
			if isNoRows(err) {
				return notFound("chat", chatID)
			}
			return err
		}
		prev, err := listTodos(ctx, tx, chatID)
		if err != nil {
			return err
		}
		created := map[string]string{}
		for _, p := range prev {
			if _, ok := created[p.Content]; !ok {
				created[p.Content] = FormatTime(p.CreatedAt)
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM chat_todos WHERE chat_id = ?", chatID); err != nil {
			return err
		}
		now := FormatTime(time.Now().UTC())
		for i, it := range items {
			c := now
			if old, ok := created[it.Content]; ok {
				c = old
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO chat_todos (chat_id, position, content, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
				chatID, i, it.Content, it.Status, c, now); err != nil {
				return err
			}
		}
		out, err = listTodos(ctx, tx, chatID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: replacing todos of %s: %w", chatID, err)
	}
	return out, nil
}
