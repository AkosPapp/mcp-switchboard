package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const profileCols = `id, name, description, system_prompt, model, capabilities, approval, budget,
	is_default, created_at, updated_at`

func scanProfile(sc scanner) (Profile, error) {
	var (
		p                Profile
		model            sql.NullString
		caps, budget     string
		isDefault        int
		created, updated string
	)
	err := sc.Scan(&p.ID, &p.Name, &p.Description, &p.SystemPrompt, &model, &caps, &p.Approval,
		&budget, &isDefault, &created, &updated)
	if err != nil {
		return p, err
	}
	p.Model = rawFrom(model)
	p.Budget = json.RawMessage(budget)
	var c struct {
		CanSpawn   bool `json:"can_spawn"`
		CanMessage bool `json:"can_message"`
	}
	_ = json.Unmarshal([]byte(caps), &c)
	p.Capabilities = Capabilities{CanSpawn: c.CanSpawn, CanMessage: c.CanMessage}
	p.IsDefault = isDefault != 0
	p.CreatedAt, p.UpdatedAt = mustTime(created), mustTime(updated)
	return p, nil
}

func getProfileTx(ctx context.Context, q queryer, id string) (*Profile, error) {
	p, err := scanProfile(q.QueryRowContext(ctx, "SELECT "+profileCols+" FROM profiles WHERE id = ?", id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading profile %s: %w", id, err)
	}
	return &p, nil
}

func (s *SQLiteStore) CreateProfile(ctx context.Context, p Profile) (Profile, error) {
	if p.ID == "" {
		p.ID = NewID()
	}
	if p.Approval == "" {
		p.Approval = ApprovalDestructive
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if p.IsDefault {
			if _, err := tx.ExecContext(ctx, "UPDATE profiles SET is_default = 0 WHERE is_default = 1"); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO profiles (`+profileCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, p.Name, p.Description, p.SystemPrompt, rawArg(p.Model, ""), encodeCaps(p.Capabilities),
			p.Approval, rawArg(p.Budget, "{}"), boolInt(p.IsDefault), FormatTime(now), FormatTime(now))
		return err
	})
	if isUnique(err) {
		return Profile{}, fmt.Errorf("%w: profile %q", ErrNameTaken, p.Name)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("store: creating profile %q: %w", p.Name, err)
	}
	p.Budget = rawOr(p.Budget, "{}")
	if string(p.Model) == "null" {
		p.Model = nil
	}
	return p, nil
}

func (s *SQLiteStore) GetProfile(ctx context.Context, id string) (*Profile, error) {
	return getProfileTx(ctx, s.read, id)
}

func (s *SQLiteStore) GetDefaultProfile(ctx context.Context) (*Profile, error) {
	p, err := scanProfile(s.read.QueryRowContext(ctx, "SELECT "+profileCols+" FROM profiles WHERE is_default = 1"))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading default profile: %w", err)
	}
	return &p, nil
}

func (s *SQLiteStore) ListProfiles(ctx context.Context) ([]Profile, error) {
	rows, err := s.read.QueryContext(ctx,
		"SELECT "+profileCols+" FROM profiles ORDER BY is_default DESC, name COLLATE NOCASE, name")
	if err != nil {
		return nil, fmt.Errorf("store: listing profiles: %w", err)
	}
	defer rows.Close()
	out := []Profile{}
	for rows.Next() {
		p, err := scanProfile(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading profile row: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateProfile(ctx context.Context, id string, p ProfilePatch) (Profile, error) {
	sets, args := []string{"updated_at = ?"}, []any{FormatTime(time.Now())}
	add := func(col string, v any) { sets, args = append(sets, col+" = ?"), append(args, v) }
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.Description != nil {
		add("description", *p.Description)
	}
	if p.SystemPrompt != nil {
		add("system_prompt", *p.SystemPrompt)
	}
	switch {
	case p.ClearModel:
		add("model", nil)
	case len(p.Model) > 0:
		add("model", string(p.Model))
	}
	if p.Capabilities != nil {
		add("capabilities", encodeCaps(*p.Capabilities))
	}
	if p.Approval != nil {
		add("approval", *p.Approval)
	}
	if len(p.Budget) > 0 {
		add("budget", string(p.Budget))
	}
	args = append(args, id)
	var out *Profile
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE profiles SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("profile", id)
		}
		out, err = getProfileTx(ctx, tx, id)
		return err
	})
	if isUnique(err) {
		return Profile{}, fmt.Errorf("%w: profile name", ErrNameTaken)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("store: updating profile %s: %w", id, err)
	}
	return *out, nil
}

func (s *SQLiteStore) SetDefaultProfile(ctx context.Context, id string) (Profile, error) {
	var out *Profile
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if p, err := getProfileTx(ctx, tx, id); err != nil {
			return err
		} else if p == nil {
			return notFound("profile", id)
		}
		// Clear first: the partial unique index allows only one row at a time.
		if _, err := tx.ExecContext(ctx, "UPDATE profiles SET is_default = 0 WHERE is_default = 1 AND id <> ?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE profiles SET is_default = 1, updated_at = ? WHERE id = ?",
			FormatTime(time.Now()), id); err != nil {
			return err
		}
		var err error
		out, err = getProfileTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return Profile{}, fmt.Errorf("store: setting default profile %s: %w", id, err)
	}
	return *out, nil
}

func (s *SQLiteStore) DeleteProfile(ctx context.Context, id string) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// The foreign keys would do this too; explicit keeps it independent of the pragma.
		for _, q := range []string{
			"UPDATE chats SET profile_id = NULL WHERE profile_id = ?",
			"UPDATE agents SET profile_id = NULL WHERE profile_id = ?",
		} {
			if _, err := tx.ExecContext(ctx, q, id); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM profiles WHERE id = ?", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("profile", id)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: deleting profile %s: %w", id, err)
	}
	return nil
}

func (s *SQLiteStore) CountProfiles(ctx context.Context) (int, error) {
	var n int
	if err := s.read.QueryRowContext(ctx, "SELECT COUNT(*) FROM profiles").Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting profiles: %w", err)
	}
	return n, nil
}
