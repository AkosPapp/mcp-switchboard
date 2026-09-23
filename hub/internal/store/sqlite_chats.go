package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const chatCols = `id, agent_id, peer_agent_id, title, kind, active_leaf_id, tags, token_total,
	cost_total_micros, created_at, updated_at, archived_at, profile_id, client_label, parent_chat_id`

func scanChat(sc scanner) (Chat, error) {
	var (
		c                      Chat
		peer, leaf, archived   sql.NullString
		profile, client        sql.NullString
		parentChat             sql.NullString
		tags, created, updated string
	)
	err := sc.Scan(&c.ID, &c.AgentID, &peer, &c.Title, &c.Kind, &leaf, &tags, &c.TokenTotal,
		&c.CostTotalMicros, &created, &updated, &archived, &profile, &client, &parentChat)
	if err != nil {
		return c, err
	}
	c.PeerAgentID, c.ActiveLeafID = optional(peer), optional(leaf)
	c.ProfileID, c.ClientLabel, c.ParentChatID = optional(profile), optional(client), optional(parentChat)
	_ = json.Unmarshal([]byte(tags), &c.Tags)
	if c.Tags == nil {
		c.Tags = []string{}
	}
	c.CreatedAt, c.UpdatedAt, c.ArchivedAt = mustTime(created), mustTime(updated), timePtr(archived)
	return c, nil
}

func getChatTx(ctx context.Context, q queryer, id string) (*Chat, error) {
	c, err := scanChat(q.QueryRowContext(ctx, "SELECT "+chatCols+" FROM chats WHERE id = ?", id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading chat %s: %w", id, err)
	}
	return &c, nil
}

func encodeTags(t []string) string {
	if t == nil {
		t = []string{}
	}
	b, _ := json.Marshal(t)
	return string(b)
}

func (s *SQLiteStore) CreateChat(ctx context.Context, c Chat) (Chat, error) {
	if c.ID == "" {
		c.ID = NewID()
	}
	if c.Kind == "" {
		c.Kind = ChatKindHuman
	}
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt, c.ArchivedAt = now, now, nil
	if c.Tags == nil {
		c.Tags = []string{}
	}
	_, err := s.write.ExecContext(ctx,
		`INSERT INTO chats (`+chatCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?)`,
		c.ID, c.AgentID, strArg(c.PeerAgentID), c.Title, c.Kind, strArg(c.ActiveLeafID),
		encodeTags(c.Tags), c.TokenTotal, c.CostTotalMicros, FormatTime(now), FormatTime(now),
		strArg(c.ProfileID), strArg(c.ClientLabel), strArg(c.ParentChatID))
	if err != nil {
		return Chat{}, fmt.Errorf("store: creating chat: %w", err)
	}
	return c, nil
}

func (s *SQLiteStore) GetChat(ctx context.Context, id string) (*Chat, error) {
	return getChatTx(ctx, s.read, id)
}

func (s *SQLiteStore) ListChats(ctx context.Context, f ChatFilter) ([]Chat, error) {
	var where []string
	var args []any
	for _, c := range []struct{ col, val string }{
		{"agent_id", f.AgentID}, {"peer_agent_id", f.PeerAgentID}, {"parent_chat_id", f.ParentChatID}, {"kind", f.Kind},
	} {
		if c.val != "" {
			where, args = append(where, c.col+" = ?"), append(args, c.val)
		}
	}
	if !f.IncludeArchived {
		where = append(where, "archived_at IS NULL")
	}
	q := "SELECT " + chatCols + " FROM chats"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY updated_at DESC, rowid DESC LIMIT ? OFFSET ?"
	args = append(args, clampLimit(f.Limit), max(f.Offset, 0))
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing chats: %w", err)
	}
	defer rows.Close()
	out := []Chat{}
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading chat row: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateChat(ctx context.Context, id string, p ChatPatch) (Chat, error) {
	now := time.Now().UTC()
	sets, args := []string{"updated_at = ?"}, []any{FormatTime(now)}
	if p.Title != nil {
		sets, args = append(sets, "title = ?"), append(args, *p.Title)
	}
	if p.Tags != nil {
		sets, args = append(sets, "tags = ?"), append(args, encodeTags(*p.Tags))
	}
	if p.Archived != nil {
		if *p.Archived {
			sets, args = append(sets, "archived_at = COALESCE(archived_at, ?)"), append(args, FormatTime(now))
		} else {
			sets = append(sets, "archived_at = NULL")
		}
	}
	if p.SetProfile {
		sets, args = append(sets, "profile_id = ?"), append(args, strArg(p.ProfileID))
	}
	if p.SetClient {
		sets, args = append(sets, "client_label = ?"), append(args, strArg(p.ClientLabel))
	}
	args = append(args, id)
	var out *Chat
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE chats SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("chat", id)
		}
		out, err = getChatTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return Chat{}, fmt.Errorf("store: updating chat %s: %w", id, err)
	}
	return *out, nil
}

func (s *SQLiteStore) DeleteChat(ctx context.Context, id string) error {
	res, err := s.write.ExecContext(ctx, "DELETE FROM chats WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: deleting chat %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("chat", id)
	}
	return nil
}

const messageCols = `id, chat_id, parent_id, role, content, tool_calls, tool_results, token_input,
	token_output, cost_micros, latency_ms, model, system_prompt, tools, finish_reason, run_id, sender, last_active_child_id, created_at`

// messageColsM is messageCols qualified for queries that join.
const messageColsM = `m.id, m.chat_id, m.parent_id, m.role, m.content, m.tool_calls, m.tool_results,
	m.token_input, m.token_output, m.cost_micros, m.latency_ms, m.model, m.system_prompt, m.tools,
	m.finish_reason, m.run_id, m.sender, m.last_active_child_id, m.created_at`

func scanMessage(sc scanner) (Message, error) {
	var (
		m                             Message
		parent, calls, results, model sql.NullString
		systemPrompt, tools           sql.NullString
		finish, run, lac, sender      sql.NullString
		content, created              string
	)
	err := sc.Scan(&m.ID, &m.ChatID, &parent, &m.Role, &content, &calls, &results, &m.TokenInput,
		&m.TokenOutput, &m.CostMicros, &m.LatencyMs, &model, &systemPrompt, &tools, &finish, &run, &sender, &lac, &created)
	if err != nil {
		return m, err
	}
	m.ParentID, m.FinishReason, m.RunID, m.LastActiveChildID = optional(parent), optional(finish), optional(run), optional(lac)
	m.Content, m.ToolCalls, m.ToolResults, m.Model = json.RawMessage(content), rawFrom(calls), rawFrom(results), rawFrom(model)
	m.SystemPrompt, m.Tools = optional(systemPrompt), rawFrom(tools)
	m.Sender = rawFrom(sender)
	m.CreatedAt = mustTime(created)
	return m, nil
}

// SearchText flattens a content array's text blocks (and a bare JSON string)
// into what the FTS index stores. Anything unparseable indexes as nothing.
func SearchText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// pathUpdateSQL points every ancestor's last_active_child_id at the next
// message down the path from ?1's message. up(id, child) holds each ancestor
// with its child on the path.
const pathUpdateSQL = `WITH RECURSIVE up(id, child) AS (
	SELECT parent_id, id FROM messages WHERE id = ? AND parent_id IS NOT NULL
	UNION ALL
	SELECT p.parent_id, p.id FROM messages p JOIN up ON p.id = up.id WHERE p.parent_id IS NOT NULL
)
UPDATE messages SET last_active_child_id = (SELECT child FROM up WHERE up.id = messages.id)
WHERE id IN (SELECT id FROM up)`

func appendMessageTx(ctx context.Context, tx *sql.Tx, m *Message) error {
	if m.ID == "" {
		m.ID = NewID()
	}
	m.CreatedAt = timeOrNow(m.CreatedAt)
	m.LastActiveChildID = nil
	if len(m.Content) == 0 {
		m.Content = json.RawMessage("[]")
	}

	chat, err := getChatTx(ctx, tx, m.ChatID)
	if err != nil {
		return err
	}
	if chat == nil {
		return notFound("chat", m.ChatID)
	}
	if m.ParentID != nil {
		var parentChat string
		err := tx.QueryRowContext(ctx, "SELECT chat_id FROM messages WHERE id = ?", *m.ParentID).Scan(&parentChat)
		if isNoRows(err) {
			return notFound("parent message", *m.ParentID)
		}
		if err != nil {
			return err
		}
		if parentChat != m.ChatID {
			return fmt.Errorf("%w: parent message %s belongs to another chat", ErrInvalid, *m.ParentID)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO messages (id, chat_id, parent_id, role, content, search_text, tool_calls, tool_results,
			token_input, token_output, cost_micros, latency_ms, model, system_prompt, tools, finish_reason, run_id, sender, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.ChatID, strArg(m.ParentID), m.Role, string(m.Content), SearchText(m.Content),
		rawArg(m.ToolCalls, ""), rawArg(m.ToolResults, ""), m.TokenInput, m.TokenOutput, m.CostMicros,
		m.LatencyMs, rawArg(m.Model, ""), strArg(m.SystemPrompt), rawArg(m.Tools, ""),
		strArg(m.FinishReason), strArg(m.RunID), rawArg(m.Sender, ""), FormatTime(m.CreatedAt)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE chats SET active_leaf_id = ?, updated_at = ?, token_total = token_total + ?,
			cost_total_micros = cost_total_micros + ? WHERE id = ?`,
		m.ID, FormatTime(m.CreatedAt), m.TokenInput+m.TokenOutput, m.CostMicros, m.ChatID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, pathUpdateSQL, m.ID)
	return err
}

func (s *SQLiteStore) AppendMessage(ctx context.Context, m Message) (Message, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error { return appendMessageTx(ctx, tx, &m) })
	if err != nil {
		return Message{}, fmt.Errorf("store: appending message: %w", err)
	}
	return m, nil
}

func (s *SQLiteStore) AppendMessageWithUsage(ctx context.Context, m Message) (Message, error) {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := appendMessageTx(ctx, tx, &m); err != nil {
			return err
		}
		var agentID string
		if err := tx.QueryRowContext(ctx, "SELECT agent_id FROM chats WHERE id = ?", m.ChatID).Scan(&agentID); err != nil {
			return err
		}
		return addUsageTx(ctx, tx, agentID, m.TokenInput+m.TokenOutput, m.CostMicros, m.CreatedAt)
	})
	if err != nil {
		return Message{}, fmt.Errorf("store: persisting message with usage: %w", err)
	}
	return m, nil
}

func (s *SQLiteStore) GetMessage(ctx context.Context, id string) (*Message, error) {
	m, err := scanMessage(s.read.QueryRowContext(ctx, "SELECT "+messageCols+" FROM messages WHERE id = ?", id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading message %s: %w", id, err)
	}
	return &m, nil
}

const pathSQL = `WITH RECURSIVE path(id, parent_id, n) AS (
	SELECT id, parent_id, 0 FROM messages WHERE id = %s
	UNION ALL
	SELECT m.id, m.parent_id, path.n + 1 FROM messages m JOIN path ON m.id = path.parent_id
)
SELECT ` + messageColsM + ` FROM path JOIN messages m ON m.id = path.id ORDER BY path.n DESC`

func (s *SQLiteStore) queryPath(ctx context.Context, start string, arg string) ([]Message, error) {
	rows, err := s.read.QueryContext(ctx, fmt.Sprintf(pathSQL, start), arg)
	if err != nil {
		return nil, fmt.Errorf("store: walking message path: %w", err)
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

func (s *SQLiteStore) ActivePath(ctx context.Context, chatID string) ([]Message, error) {
	return s.queryPath(ctx, "(SELECT active_leaf_id FROM chats WHERE id = ?)", chatID)
}

func (s *SQLiteStore) PathTo(ctx context.Context, messageID string) ([]Message, error) {
	return s.queryPath(ctx, "?", messageID)
}

func (s *SQLiteStore) Siblings(ctx context.Context, messageID string) (SiblingSet, error) {
	var chatID string
	var parent sql.NullString
	err := s.read.QueryRowContext(ctx, "SELECT chat_id, parent_id FROM messages WHERE id = ?", messageID).Scan(&chatID, &parent)
	if isNoRows(err) {
		return SiblingSet{}, notFound("message", messageID)
	}
	if err != nil {
		return SiblingSet{}, fmt.Errorf("store: siblings of %s: %w", messageID, err)
	}
	var ids []string
	if parent.Valid {
		ids, err = queryStrings(ctx, s.read, "SELECT id FROM messages WHERE parent_id = ? ORDER BY seq", parent.String)
	} else {
		ids, err = queryStrings(ctx, s.read,
			"SELECT id FROM messages WHERE chat_id = ? AND parent_id IS NULL ORDER BY seq", chatID)
	}
	if err != nil {
		return SiblingSet{}, fmt.Errorf("store: siblings of %s: %w", messageID, err)
	}
	set := SiblingSet{IDs: ids, Index: -1}
	for i, id := range ids {
		if id == messageID {
			set.Index = i
		}
	}
	return set, nil
}

// deepestSQL follows the remembered branch (last_active_child_id, or the newest
// child when nothing is remembered) from ? down to a leaf.
const deepestSQL = `WITH RECURSIVE d(id, n) AS (
	SELECT id, 0 FROM messages WHERE id = ?
	UNION ALL
	SELECT c.id, d.n + 1 FROM d
	JOIN messages m ON m.id = d.id
	JOIN messages c ON c.id = COALESCE(m.last_active_child_id,
		(SELECT id FROM messages k WHERE k.parent_id = m.id ORDER BY k.seq DESC LIMIT 1))
)
SELECT id FROM d ORDER BY n DESC LIMIT 1`

func (s *SQLiteStore) SelectSibling(ctx context.Context, messageID string) (string, error) {
	var leaf string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var chatID string
		err := tx.QueryRowContext(ctx, "SELECT chat_id FROM messages WHERE id = ?", messageID).Scan(&chatID)
		if isNoRows(err) {
			return notFound("message", messageID)
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, deepestSQL, messageID).Scan(&leaf); err != nil {
			return err
		}
		return setLeafTx(ctx, tx, chatID, leaf)
	})
	if err != nil {
		return "", fmt.Errorf("store: selecting %s: %w", messageID, err)
	}
	return leaf, nil
}

func setLeafTx(ctx context.Context, tx *sql.Tx, chatID, leafID string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE chats SET active_leaf_id = ?, updated_at = ? WHERE id = ?",
		leafID, FormatTime(time.Now()), chatID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, pathUpdateSQL, leafID)
	return err
}

func (s *SQLiteStore) SetActiveLeaf(ctx context.Context, chatID, messageID string) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRowContext(ctx, "SELECT chat_id FROM messages WHERE id = ?", messageID).Scan(&owner)
		if isNoRows(err) {
			return notFound("message", messageID)
		}
		if err != nil {
			return err
		}
		if owner != chatID {
			return fmt.Errorf("%w: message %s belongs to another chat", ErrInvalid, messageID)
		}
		return setLeafTx(ctx, tx, chatID, messageID)
	})
	if err != nil {
		return fmt.Errorf("store: setting active leaf: %w", err)
	}
	return nil
}

// ftsQuery turns free text into a safe FTS5 expression: every word quoted (so
// operators and punctuation are literal), all required, the last one a prefix
// so search-as-you-type works.
func ftsQuery(text string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"`
	}
	parts[len(parts)-1] += "*"
	return strings.Join(parts, " ")
}

func (s *SQLiteStore) SearchMessages(ctx context.Context, q SearchQuery) ([]SearchHit, error) {
	match := ftsQuery(q.Text)
	if match == "" {
		return []SearchHit{}, nil
	}
	query := `SELECT m.chat_id, c.title, c.agent_id, m.id, m.role,
			snippet(messages_fts, 0, '` + SnippetStart + `', '` + SnippetEnd + `', '…', 16)
		FROM messages_fts
		JOIN messages m ON m.seq = messages_fts.rowid
		JOIN chats c ON c.id = m.chat_id
		WHERE messages_fts MATCH ?`
	args := []any{match}
	if q.AgentID != "" {
		query += " AND c.agent_id = ?"
		args = append(args, q.AgentID)
	}
	query += " ORDER BY messages_fts.rank LIMIT ?"
	args = append(args, clampLimit(q.Limit))

	rows, err := s.read.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: searching messages: %w", err)
	}
	defer rows.Close()
	out := []SearchHit{}
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.ChatID, &h.ChatTitle, &h.AgentID, &h.MessageID, &h.Role, &h.Snippet); err != nil {
			return nil, fmt.Errorf("store: reading search hit: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
