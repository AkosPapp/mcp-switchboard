package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const skillCols = `id, name, description, body, auto, created_at, updated_at`

func scanSkill(sc scanner) (Skill, error) {
	var (
		s                Skill
		auto             int
		created, updated string
	)
	if err := sc.Scan(&s.ID, &s.Name, &s.Description, &s.Body, &auto, &created, &updated); err != nil {
		return s, err
	}
	s.Auto = auto != 0
	s.CreatedAt, s.UpdatedAt = mustTime(created), mustTime(updated)
	return s, nil
}

func getSkillTx(ctx context.Context, q queryer, id string) (*Skill, error) {
	s, err := scanSkill(q.QueryRowContext(ctx, "SELECT "+skillCols+" FROM skills WHERE id = ?", id))
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading skill %s: %w", id, err)
	}
	return &s, nil
}

func (s *SQLiteStore) CreateSkill(ctx context.Context, k Skill) (Skill, error) {
	if k.ID == "" {
		k.ID = NewID()
	}
	now := time.Now().UTC()
	k.CreatedAt, k.UpdatedAt = now, now
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO skills (`+skillCols+`) VALUES (?,?,?,?,?,?,?)`,
			k.ID, k.Name, k.Description, k.Body, boolInt(k.Auto), FormatTime(now), FormatTime(now))
		return err
	})
	if isUnique(err) {
		return Skill{}, fmt.Errorf("%w: skill %q", ErrNameTaken, k.Name)
	}
	if err != nil {
		return Skill{}, fmt.Errorf("store: creating skill %q: %w", k.Name, err)
	}
	return k, nil
}

func (s *SQLiteStore) GetSkill(ctx context.Context, id string) (*Skill, error) {
	return getSkillTx(ctx, s.read, id)
}

func (s *SQLiteStore) ListSkills(ctx context.Context) ([]Skill, error) {
	rows, err := s.read.QueryContext(ctx, "SELECT "+skillCols+" FROM skills ORDER BY name COLLATE NOCASE, name")
	if err != nil {
		return nil, fmt.Errorf("store: listing skills: %w", err)
	}
	defer rows.Close()
	out := []Skill{}
	for rows.Next() {
		k, err := scanSkill(rows)
		if err != nil {
			return nil, fmt.Errorf("store: reading skill row: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateSkill(ctx context.Context, id string, p SkillPatch) (Skill, error) {
	sets, args := []string{"updated_at = ?"}, []any{FormatTime(time.Now())}
	add := func(col string, v any) { sets, args = append(sets, col+" = ?"), append(args, v) }
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.Description != nil {
		add("description", *p.Description)
	}
	if p.Body != nil {
		add("body", *p.Body)
	}
	if p.Auto != nil {
		add("auto", boolInt(*p.Auto))
	}
	args = append(args, id)
	var out *Skill
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE skills SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("skill", id)
		}
		out, err = getSkillTx(ctx, tx, id)
		return err
	})
	if isUnique(err) {
		return Skill{}, fmt.Errorf("%w: skill name", ErrNameTaken)
	}
	if err != nil {
		return Skill{}, fmt.Errorf("store: updating skill %s: %w", id, err)
	}
	return *out, nil
}

func (s *SQLiteStore) DeleteSkill(ctx context.Context, id string) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM skills WHERE id = ?", id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("skill", id)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: deleting skill %s: %w", id, err)
	}
	return nil
}
