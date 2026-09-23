package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

const grantCols = "agent_id, label, project, server, allowed, source, created_at, updated_at"

func scanGrant(sc scanner) (Grant, error) {
	var g Grant
	var allowed int
	var created, updated string
	if err := sc.Scan(&g.AgentID, &g.Label, &g.Project, &g.Server, &allowed, &g.Source, &created, &updated); err != nil {
		return g, err
	}
	g.Allowed = allowed != 0
	g.CreatedAt, g.UpdatedAt = mustTime(created), mustTime(updated)
	return g, nil
}

func upsertGrantTx(ctx context.Context, tx *sql.Tx, g Grant, now time.Time) (Grant, error) {
	if g.Source == "" {
		g.Source = GrantExplicit
	}
	ts := FormatTime(now)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO grants (`+grantCols+`) VALUES (?,?,?,?,?,?,?,?)
		 ON CONFLICT (agent_id, label, project, server)
		 DO UPDATE SET allowed = excluded.allowed, source = excluded.source, updated_at = excluded.updated_at`,
		g.AgentID, g.Label, g.Project, g.Server, boolInt(g.Allowed), g.Source, ts, ts)
	if err != nil {
		return g, err
	}
	g.UpdatedAt = now
	err = tx.QueryRowContext(ctx,
		"SELECT created_at FROM grants WHERE agent_id = ? AND label = ? AND project = ? AND server = ?",
		g.AgentID, g.Label, g.Project, g.Server).Scan(&ts)
	g.CreatedAt = mustTime(ts)
	return g, err
}

// revokeBelowTx is A14: delete every descendant's inherited allow row that the
// revoked pattern covers. Descendants are found by recursive CTE, so the
// propagation is transitive without a walk. Human and explicit rows are not
// touched, and no rows are ever added.
func revokeBelowTx(ctx context.Context, tx *sql.Tx, agentID, label, project, server string) ([]Grant, error) {
	rows, err := tx.QueryContext(ctx, descendantsCTE+`
		DELETE FROM grants
		WHERE agent_id IN (SELECT id FROM d) AND source = 'inherited' AND allowed = 1
		  AND (? = '*' OR label = ?) AND (? = '*' OR project = ?) AND (? = '*' OR server = ?)
		RETURNING `+grantCols,
		agentID, label, label, project, project, server, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListGrants(ctx context.Context, agentID string) ([]Grant, error) {
	rows, err := s.read.QueryContext(ctx,
		"SELECT "+grantCols+" FROM grants WHERE agent_id = ? ORDER BY label, project, server", agentID)
	if err != nil {
		return nil, fmt.Errorf("store: listing grants: %w", err)
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading grant row: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SetGrant(ctx context.Context, g Grant) ([]Grant, error) {
	revoked := []Grant{}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := upsertGrantTx(ctx, tx, g, time.Now().UTC()); err != nil {
			return err
		}
		if g.Allowed {
			return nil
		}
		var err error
		revoked, err = revokeBelowTx(ctx, tx, g.AgentID, g.Label, g.Project, g.Server)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: setting grant: %w", err)
	}
	return revoked, nil
}

func (s *SQLiteStore) DeleteGrant(ctx context.Context, agentID, label, project, server string) ([]Grant, error) {
	revoked := []Grant{}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM grants WHERE agent_id = ? AND label = ? AND project = ? AND server = ?",
			agentID, label, project, server); err != nil {
			return err
		}
		var err error
		revoked, err = revokeBelowTx(ctx, tx, agentID, label, project, server)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: deleting grant: %w", err)
	}
	return revoked, nil
}

// canonEdge orders a pair with the lexicographically smaller id first (D14):
// edges are symmetric, one row per unordered pair, so every read and write
// goes through this to land on the same row regardless of which side asked.
func canonEdge(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}

func setEdgeTx(ctx context.Context, tx *sql.Tx, from, to string, allowed bool, now time.Time) error {
	a, b := canonEdge(from, to)
	ts := FormatTime(now)
	_, err := tx.ExecContext(ctx,
		`INSERT INTO edges (agent_a_id, agent_b_id, allowed, created_at, updated_at) VALUES (?,?,?,?,?)
		 ON CONFLICT (agent_a_id, agent_b_id) DO UPDATE SET allowed = excluded.allowed, updated_at = excluded.updated_at`,
		a, b, boolInt(allowed), ts, ts)
	return err
}

func scanEdge(sc scanner) (Edge, error) {
	var e Edge
	var allowed int
	var created, updated string
	if err := sc.Scan(&e.From, &e.To, &allowed, &created, &updated); err != nil {
		return e, err
	}
	e.Allowed = allowed != 0
	e.CreatedAt, e.UpdatedAt = mustTime(created), mustTime(updated)
	return e, nil
}

// ListEdges lists edges. Since edges are symmetric (D14), a filter naming
// only From or only To means "touching this id", and each returned row is
// normalised so the named id comes back as From and the other side as To.
// Naming both means the exact (unordered) pair.
func (s *SQLiteStore) ListEdges(ctx context.Context, f EdgeFilter) ([]Edge, error) {
	q := "SELECT agent_a_id, agent_b_id, allowed, created_at, updated_at FROM edges WHERE 1=1"
	var args []any
	touching := ""
	switch {
	case f.From != "" && f.To != "":
		a, b := canonEdge(f.From, f.To)
		q += " AND agent_a_id = ? AND agent_b_id = ?"
		args = append(args, a, b)
	case f.From != "":
		q += " AND (agent_a_id = ? OR agent_b_id = ?)"
		args = append(args, f.From, f.From)
		touching = f.From
	case f.To != "":
		q += " AND (agent_a_id = ? OR agent_b_id = ?)"
		args = append(args, f.To, f.To)
		touching = f.To
	}
	rows, err := s.read.QueryContext(ctx, q+" ORDER BY agent_a_id, agent_b_id", args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing edges: %w", err)
	}
	defer rows.Close()
	out := []Edge{}
	for rows.Next() {
		e, err := scanEdge(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading edge row: %w", err)
		}
		if touching != "" && e.From != touching {
			e.From, e.To = e.To, e.From
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) SetEdge(ctx context.Context, from, to string, allowed bool) (Edge, error) {
	a, b := canonEdge(from, to)
	var e Edge
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := setEdgeTx(ctx, tx, from, to, allowed, time.Now().UTC()); err != nil {
			return err
		}
		var err error
		e, err = scanEdge(tx.QueryRowContext(ctx,
			"SELECT agent_a_id, agent_b_id, allowed, created_at, updated_at FROM edges WHERE agent_a_id = ? AND agent_b_id = ?",
			a, b))
		return err
	})
	if err != nil {
		return Edge{}, fmt.Errorf("store: setting edge: %w", err)
	}
	// Report back in the caller's own from/to order (harmless: the edge is
	// symmetric either way) rather than the canonical a/b order.
	e.From, e.To = from, to
	return e, nil
}

func (s *SQLiteStore) DeleteEdge(ctx context.Context, from, to string) error {
	a, b := canonEdge(from, to)
	if _, err := s.write.ExecContext(ctx,
		"DELETE FROM edges WHERE agent_a_id = ? AND agent_b_id = ?", a, b); err != nil {
		return fmt.Errorf("store: deleting edge: %w", err)
	}
	return nil
}

// EdgeAllowed is symmetric by construction (D14): both directions read the
// same canonical row, so there is nothing for a caller to get wrong by
// checking only one direction.
func (s *SQLiteStore) EdgeAllowed(ctx context.Context, from, to string) (bool, error) {
	a, b := canonEdge(from, to)
	var allowed int
	err := s.read.QueryRowContext(ctx,
		"SELECT allowed FROM edges WHERE agent_a_id = ? AND agent_b_id = ?", a, b).Scan(&allowed)
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: reading edge: %w", err)
	}
	return allowed != 0, nil
}
