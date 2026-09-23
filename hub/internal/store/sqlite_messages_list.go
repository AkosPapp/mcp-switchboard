package store

import (
	"context"
	"fmt"
)

// ListMessages returns every message of a chat in creation order: the whole
// DAG, for GET /api/chats/{id}/messages?tree=1 and the JSON export (U12).
// It is deliberately not part of the ChatStore interface; readers that need it
// declare their own narrow interface.
func (s *SQLiteStore) ListMessages(ctx context.Context, chatID string) ([]Message, error) {
	rows, err := s.read.QueryContext(ctx,
		"SELECT "+messageCols+" FROM messages WHERE chat_id = ? ORDER BY seq", chatID)
	if err != nil {
		return nil, fmt.Errorf("store: listing messages of %s: %w", chatID, err)
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading message row: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
