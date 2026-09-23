package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const agentCols = `id, parent_id, name, description, project, model, system_prompt, depth, status,
	budget, capabilities, approval, auto_wake, token_total, cost_total_micros,
	created_at, updated_at, last_activity_at, deleted_at, profile_id, origin, client_label`

func scanAgent(sc scanner) (Agent, error) {
	var (
		a                          Agent
		parent, project, deleted   sql.NullString
		profile, clientLabel       sql.NullString
		model, budget, caps        string
		autoWake                   int
		created, updated, activity string
	)
	err := sc.Scan(&a.ID, &parent, &a.Name, &a.Description, &project, &model, &a.SystemPrompt,
		&a.Depth, &a.Status, &budget, &caps, &a.Approval, &autoWake, &a.TokenTotal,
		&a.CostTotalMicros, &created, &updated, &activity, &deleted, &profile, &a.Origin, &clientLabel)
	if err != nil {
		return a, err
	}
	a.ParentID = optional(parent)
	a.Project = optional(project)
	a.Model = json.RawMessage(model)
	a.Budget = json.RawMessage(budget)
	var c struct {
		CanSpawn   bool `json:"can_spawn"`
		CanMessage bool `json:"can_message"`
	}
	_ = json.Unmarshal([]byte(caps), &c)
	a.Capabilities = Capabilities{CanSpawn: c.CanSpawn, CanMessage: c.CanMessage}
	a.AutoWake = autoWake != 0
	a.CreatedAt, a.UpdatedAt, a.LastActivityAt = mustTime(created), mustTime(updated), mustTime(activity)
	a.DeletedAt = timePtr(deleted)
	a.ProfileID = optional(profile)
	a.ClientLabel = optional(clientLabel)
	return a, nil
}

func encodeCaps(c Capabilities) string {
	b, _ := json.Marshal(map[string]bool{"can_spawn": c.CanSpawn, "can_message": c.CanMessage})
	return string(b)
}

func projectArg(p *string) any {
	if p == nil || *p == "" {
		return nil
	}
	return *p
}

func getAgentTx(ctx context.Context, q queryer, id string) (*Agent, error) {
	a, err := scanAgent(q.QueryRowContext(ctx, "SELECT "+agentCols+" FROM agents WHERE id = ?", id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading agent %s: %w", id, err)
	}
	return &a, nil
}

func (s *SQLiteStore) CreateAgent(ctx context.Context, a Agent, grants []Grant) (Agent, error) {
	now := time.Now().UTC()
	if a.ID == "" {
		a.ID = NewID()
	}
	if a.Status == "" {
		a.Status = AgentIdle
	}
	if a.Approval == "" {
		a.Approval = ApprovalDestructive
	}
	a.CreatedAt, a.UpdatedAt, a.LastActivityAt = now, now, now
	a.DeletedAt = nil
	if a.Origin == "" {
		a.Origin = OriginChat
		if a.ParentID != nil {
			a.Origin = OriginSpawn
		}
	}

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		a.Depth = 0
		if a.ParentID != nil {
			parent, err := getAgentTx(ctx, tx, *a.ParentID)
			if err != nil {
				return err
			}
			if parent == nil || parent.DeletedAt != nil {
				return notFound("parent agent", *a.ParentID)
			}
			a.Depth = parent.Depth + 1
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO agents (`+agentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?)`,
			a.ID, strArg(a.ParentID), a.Name, a.Description, projectArg(a.Project),
			rawArg(a.Model, "{}"), a.SystemPrompt, a.Depth, a.Status, rawArg(a.Budget, "{}"),
			encodeCaps(a.Capabilities), a.Approval, boolInt(a.AutoWake), a.TokenTotal,
			a.CostTotalMicros, FormatTime(now), FormatTime(now), FormatTime(now), strArg(a.ProfileID),
			a.Origin, strArg(a.ClientLabel))
		if isUnique(err) {
			return fmt.Errorf("%w: %q", ErrNameTaken, a.Name)
		}
		if err != nil {
			return err
		}
		if a.ParentID != nil {
			if err := setEdgeTx(ctx, tx, *a.ParentID, a.ID, true, now); err != nil {
				return err
			}
		}
		for _, g := range grants {
			g.AgentID = a.ID
			if _, err := upsertGrantTx(ctx, tx, g, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Agent{}, fmt.Errorf("store: creating agent %q: %w", a.Name, err)
	}
	if a.Project != nil && *a.Project == "" {
		a.Project = nil
	}
	a.Model, a.Budget = rawOr(a.Model, "{}"), rawOr(a.Budget, "{}")
	return a, nil
}

func (s *SQLiteStore) GetAgent(ctx context.Context, id string) (*Agent, error) {
	return getAgentTx(ctx, s.read, id)
}

func (s *SQLiteStore) GetAgentByName(ctx context.Context, parentID, name string) (*Agent, error) {
	a, err := scanAgent(s.read.QueryRowContext(ctx,
		"SELECT "+agentCols+" FROM agents WHERE COALESCE(parent_id, '') = ? AND name = ? AND deleted_at IS NULL",
		parentID, name))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: finding agent %q: %w", name, err)
	}
	return &a, nil
}

func (s *SQLiteStore) ListAgents(ctx context.Context, f AgentFilter) ([]Agent, error) {
	var where []string
	var args []any
	switch {
	case f.ParentID != "":
		where, args = append(where, "parent_id = ?"), append(args, f.ParentID)
	case f.RootsOnly:
		where = append(where, "parent_id IS NULL")
	}
	if f.Project != "" {
		where, args = append(where, "project = ?"), append(args, f.Project)
	}
	if !f.IncludeDeleted {
		where = append(where, "deleted_at IS NULL")
	}
	q := "SELECT " + agentCols + " FROM agents"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY created_at, rowid"
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing agents: %w", err)
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading agent row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) CountLiveChildren(ctx context.Context, parentID string) (int, error) {
	var n int
	err := s.read.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM agents WHERE parent_id = ? AND deleted_at IS NULL", parentID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: counting children: %w", err)
	}
	return n, nil
}

func (s *SQLiteStore) UpdateAgent(ctx context.Context, id string, p AgentPatch) (Agent, error) {
	sets := []string{"updated_at = ?"}
	args := []any{FormatTime(time.Now())}
	add := func(col string, v any) {
		sets, args = append(sets, col+" = ?"), append(args, v)
	}
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.Description != nil {
		add("description", *p.Description)
	}
	if p.Project != nil {
		add("project", projectArg(p.Project))
	}
	if len(p.Model) > 0 {
		add("model", string(p.Model))
	}
	if p.SystemPrompt != nil {
		add("system_prompt", *p.SystemPrompt)
	}
	if len(p.Budget) > 0 {
		add("budget", string(p.Budget))
	}
	if p.Capabilities != nil {
		add("capabilities", encodeCaps(*p.Capabilities))
	}
	if p.Approval != nil {
		add("approval", *p.Approval)
	}
	if p.AutoWake != nil {
		add("auto_wake", boolInt(*p.AutoWake))
	}
	if p.SetProfile {
		add("profile_id", strArg(p.ProfileID))
	}
	args = append(args, id)

	var out *Agent
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE agents SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if isUnique(err) {
			return fmt.Errorf("%w: %q", ErrNameTaken, *p.Name)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("agent", id)
		}
		if p.SetClient {
			if _, err := setAgentClientTx(ctx, tx, id, p.ClientLabel); err != nil {
				return err
			}
		}
		out, err = getAgentTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return Agent{}, fmt.Errorf("store: updating agent %s: %w", id, err)
	}
	return *out, nil
}

func (s *SQLiteStore) SetAgentClient(ctx context.Context, id string, label *string) ([]Grant, error) {
	var revoked []Grant
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var err error
		revoked, err = setAgentClientTx(ctx, tx, id, label)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: setting agent %s client: %w", id, err)
	}
	return revoked, nil
}

func setAgentClientTx(ctx context.Context, tx *sql.Tx, id string, label *string) ([]Grant, error) {
	revoked := []Grant{}
	a, err := getAgentTx(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if a == nil || a.DeletedAt != nil {
		return nil, notFound("live agent", id)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, "DELETE FROM grants WHERE agent_id = ?", id); err != nil {
		return nil, err
	}
	newLabel := ""
	if label != nil {
		newLabel = *label
		if _, err := upsertGrantTx(ctx, tx, Grant{AgentID: id, Label: newLabel, Project: Wildcard,
			Server: Wildcard, Allowed: true, Source: GrantExplicit}, now); err != nil {
			return nil, err
		}
	}
	// A14: what descendants inherited beyond the new client is revoked.
	rows, err := tx.QueryContext(ctx, descendantsCTE+`
		DELETE FROM grants
		WHERE agent_id IN (SELECT id FROM d) AND source = 'inherited' AND allowed = 1 AND label <> ?
		RETURNING `+grantCols, id, newLabel)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		revoked = append(revoked, g)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, descendantsCTE+`
		UPDATE agents SET client_label = NULL, updated_at = ?
		WHERE id IN (SELECT id FROM d) AND client_label IS NOT NULL AND client_label <> ?`,
		id, FormatTime(now), newLabel); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE agents SET client_label = ?, updated_at = ? WHERE id = ?",
		strArg(label), FormatTime(now), id)
	return revoked, err
}

func (s *SQLiteStore) SetAgentStatus(ctx context.Context, id, status string) error {
	now := FormatTime(time.Now())
	res, err := s.write.ExecContext(ctx,
		"UPDATE agents SET status = ?, updated_at = ?, last_activity_at = ? WHERE id = ?", status, now, now, id)
	if err != nil {
		return fmt.Errorf("store: setting agent status: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("agent", id)
	}
	return nil
}

const ancestorsCTE = `WITH RECURSIVE anc(id, parent_id, n) AS (
	SELECT id, parent_id, 0 FROM agents WHERE id = ?
	UNION ALL
	SELECT a.id, a.parent_id, anc.n + 1 FROM agents a JOIN anc ON a.id = anc.parent_id
)`

const descendantsCTE = `WITH RECURSIVE d(id) AS (
	SELECT id FROM agents WHERE parent_id = ?
	UNION ALL
	SELECT a.id FROM agents a JOIN d ON a.parent_id = d.id
)`

func (s *SQLiteStore) Ancestors(ctx context.Context, id string) ([]string, error) {
	out, err := queryStrings(ctx, s.read, ancestorsCTE+" SELECT id FROM anc WHERE n > 0 ORDER BY n", id)
	if err != nil {
		return nil, fmt.Errorf("store: ancestors of %s: %w", id, err)
	}
	return out, nil
}

func (s *SQLiteStore) Descendants(ctx context.Context, id string) ([]string, error) {
	out, err := queryStrings(ctx, s.read, descendantsCTE+" SELECT id FROM d", id)
	if err != nil {
		return nil, fmt.Errorf("store: descendants of %s: %w", id, err)
	}
	return out, nil
}

// addUsageTx is the single recursive-CTE UPDATE of B5. It touches the agent
// itself and every ancestor; a missing agent updates nothing, which is reported.
func addUsageTx(ctx context.Context, tx *sql.Tx, agentID string, tokens, cost int64, now time.Time) error {
	ts := FormatTime(now)
	res, err := tx.ExecContext(ctx, ancestorsCTE+`
		UPDATE agents SET token_total = token_total + ?, cost_total_micros = cost_total_micros + ?,
			updated_at = ?, last_activity_at = ?
		WHERE id IN (SELECT id FROM anc)`, agentID, tokens, cost, ts, ts)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("agent", agentID)
	}
	return nil
}

func (s *SQLiteStore) AddUsage(ctx context.Context, agentID string, tokens, costMicros int64) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		return addUsageTx(ctx, tx, agentID, tokens, costMicros, time.Now().UTC())
	})
	if err != nil {
		return fmt.Errorf("store: adding usage: %w", err)
	}
	return nil
}

func (s *SQLiteStore) LifetimeExceeded(ctx context.Context, agentID string, limitMicros int64) (string, bool, error) {
	if limitMicros <= 0 {
		return "", false, nil
	}
	var id string
	err := s.read.QueryRowContext(ctx, ancestorsCTE+`
		SELECT a.id FROM agents a WHERE a.id IN (SELECT id FROM anc) AND a.cost_total_micros >= ?
		ORDER BY a.depth DESC LIMIT 1`, agentID, limitMicros).Scan(&id)
	if isNoRows(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: lifetime cost check: %w", err)
	}
	return id, true, nil
}

func (s *SQLiteStore) SoftDeleteAgent(ctx context.Context, id string) ([]string, error) {
	var newly []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		a, err := getAgentTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if a == nil || a.DeletedAt != nil {
			return notFound("live agent", id)
		}
		newly, err = queryStrings(ctx, tx,
			descendantsCTE+" SELECT a.id FROM agents a WHERE a.id IN (SELECT id FROM d) AND a.deleted_at IS NULL", id)
		if err != nil {
			return err
		}
		all := append([]string{id}, newly...)
		list, args := inList(all)
		now := time.Now().UTC()
		ts := FormatTime(now)
		if _, err := tx.ExecContext(ctx,
			"UPDATE agents SET deleted_at = ?, status = 'idle', updated_at = ? WHERE deleted_at IS NULL AND id IN ("+list+")",
			append([]any{ts, ts}, args...)...); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE runs SET status = 'cancelled', finished_at = ?, finished_at_epoch = ?,
			     error = COALESCE(error, 'the agent was deleted')
			 WHERE status IN ('queued','running','waiting') AND agent_id IN (`+list+")",
			append([]any{ts, epoch(now)}, args...)...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: deleting agent %s: %w", id, err)
	}
	return newly, nil
}

func (s *SQLiteStore) HardDeleteAgent(ctx context.Context, id string) ([]string, error) {
	var removed []string
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		a, err := getAgentTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if a == nil {
			return notFound("agent", id)
		}
		removed, err = hardDeleteAgentTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: hard-deleting agent %s: %w", id, err)
	}
	return removed, nil
}

// hardDeleteAgentTx removes the agent, its descendants and everything they own,
// returning every agent id removed (id first).
func hardDeleteAgentTx(ctx context.Context, tx *sql.Tx, id string) ([]string, error) {
	desc, err := queryStrings(ctx, tx, descendantsCTE+" SELECT id FROM d", id)
	if err != nil {
		return nil, err
	}
	removed := append([]string{id}, desc...)
	list, args := inList(removed)
	// The call log carries no foreign key (it is pruned on its own
	// schedule), so a GDPR-shaped delete has to reach it explicitly.
	if _, err := tx.ExecContext(ctx, "DELETE FROM calls WHERE agent_id IN ("+list+")", args...); err != nil {
		return nil, err
	}
	// Cascades take the descendants, chats, messages, runs, edges, grants, inbox.
	if _, err := tx.ExecContext(ctx, "DELETE FROM agents WHERE id = ?", id); err != nil {
		return nil, err
	}
	return removed, nil
}
