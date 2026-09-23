package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func (s *SQLiteStore) GetDraft(ctx context.Context, chatID string) (Draft, error) {
	var content, updated string
	err := s.read.QueryRowContext(ctx,
		"SELECT content, updated_at FROM chat_drafts WHERE chat_id = ?", chatID).Scan(&content, &updated)
	if isNoRows(err) {
		return Draft{}, nil
	}
	if err != nil {
		return Draft{}, fmt.Errorf("store: reading draft of %s: %w", chatID, err)
	}
	t := mustTime(updated)
	return Draft{Content: content, UpdatedAt: &t}, nil
}

func (s *SQLiteStore) SetDraft(ctx context.Context, chatID, content string) (Draft, error) {
	var out Draft
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, "SELECT 1 FROM chats WHERE id = ?", chatID).Scan(&one); err != nil {
			if isNoRows(err) {
				return notFound("chat", chatID)
			}
			return err
		}
		if strings.TrimSpace(content) == "" {
			_, err := tx.ExecContext(ctx, "DELETE FROM chat_drafts WHERE chat_id = ?", chatID)
			return err
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO chat_drafts (chat_id, content, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (chat_id) DO UPDATE SET content = excluded.content, updated_at = excluded.updated_at`,
			chatID, content, FormatTime(now)); err != nil {
			return err
		}
		out = Draft{Content: content, UpdatedAt: &now}
		return nil
	})
	if err != nil {
		return Draft{}, fmt.Errorf("store: saving draft of %s: %w", chatID, err)
	}
	return out, nil
}

func (s *SQLiteStore) DeleteDraft(ctx context.Context, chatID string) error {
	if _, err := s.write.ExecContext(ctx, "DELETE FROM chat_drafts WHERE chat_id = ?", chatID); err != nil {
		return fmt.Errorf("store: deleting draft of %s: %w", chatID, err)
	}
	return nil
}

func (s *SQLiteStore) AppendMessageClearingDraft(ctx context.Context, m Message) (Message, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := appendMessageTx(ctx, tx, &m); err != nil {
			return err
		}
		// Best effort: a failed statement does not abort the transaction in
		// SQLite, and the message must never be lost over a draft.
		_, _ = tx.ExecContext(ctx, "DELETE FROM chat_drafts WHERE chat_id = ?", m.ChatID)
		return nil
	})
	if err != nil {
		return Message{}, fmt.Errorf("store: appending message: %w", err)
	}
	return m, nil
}
