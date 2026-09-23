package store

import (
	"context"
	"database/sql"
	"fmt"
)

// chatSubtreeSQL lists a chat and every chat below it (UNION, not UNION ALL, so
// a corrupt parent cycle terminates).
const chatSubtreeSQL = `WITH RECURSIVE t(id) AS (
	SELECT id FROM chats WHERE id = ?
	UNION
	SELECT c.id FROM chats c JOIN t ON c.parent_chat_id = t.id
) SELECT id FROM t`

func (s *SQLiteStore) DeleteChatCascade(ctx context.Context, id string) (ChatDeletion, error) {
	var out ChatDeletion
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		c, err := getChatTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if c == nil {
			return notFound("chat", id)
		}
		if out.ChatIDs, err = queryStrings(ctx, tx, chatSubtreeSQL, id); err != nil {
			return err
		}
		list, args := inList(out.ChatIDs)
		cand, err := queryStrings(ctx, tx, "SELECT DISTINCT agent_id FROM chats WHERE id IN ("+list+")", args...)
		if err != nil {
			return err
		}
		// Messages, runs and drafts go with their chat (foreign keys).
		if _, err := tx.ExecContext(ctx, "DELETE FROM chats WHERE id IN ("+list+")", args...); err != nil {
			return err
		}

		// An execution record goes when no chat is left on it and no live child
		// record survives (a legacy child whose chat was never linked to its parent
		// is kept rather than swept away by the cascade).
		doomed := map[string]bool{}
		for _, a := range cand {
			var n int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM chats WHERE agent_id = ?", a).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				doomed[a] = true
			}
		}
		for changed := true; changed; {
			changed = false
			for a := range doomed {
				kids, err := queryStrings(ctx, tx, "SELECT id FROM agents WHERE parent_id = ? AND deleted_at IS NULL", a)
				if err != nil {
					return err
				}
				for _, k := range kids {
					if !doomed[k] {
						delete(doomed, a)
						changed = true
						break
					}
				}
			}
		}
		for a := range doomed {
			ag, err := getAgentTx(ctx, tx, a)
			if err != nil {
				return err
			}
			if ag == nil || (ag.ParentID != nil && doomed[*ag.ParentID]) {
				continue // gone already, or removed with its parent record
			}
			removed, err := hardDeleteAgentTx(ctx, tx, a)
			if err != nil {
				return err
			}
			out.AgentIDs = append(out.AgentIDs, removed...)
		}
		return nil
	})
	if err != nil {
		return ChatDeletion{}, fmt.Errorf("store: deleting chat %s: %w", id, err)
	}
	return out, nil
}
