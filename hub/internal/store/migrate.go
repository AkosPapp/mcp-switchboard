package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const migrationDir = "migrations"

// schemaMigrations is created by the runner rather than by 001, because the
// runner has to read it before it can apply anything.
const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     INTEGER PRIMARY KEY,
    name        TEXT NOT NULL,
    applied_at  TEXT NOT NULL
)`

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads and validates the embedded set. Numbering must start at
// 1 and be contiguous: a gap means a file was lost in a rebase or never got
// committed, and applying what is left would produce a schema nobody designed.
func loadMigrations(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("reading migrations: %w", err)
	}

	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		numStr, rest, ok := strings.Cut(strings.TrimSuffix(e.Name(), ".sql"), "_")
		if !ok || rest == "" {
			return nil, fmt.Errorf("migration %q is not named NNN_description.sql", e.Name())
		}
		version, err := strconv.Atoi(numStr)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("migration %q has no usable version number", e.Name())
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading migration %q: %w", e.Name(), err)
		}
		out = append(out, migration{version: version, name: rest, sql: string(body)})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration numbering is not contiguous: expected %03d, found %03d_%s", i+1, m.version, m.name)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no migrations found in %s", dir)
	}
	return out, nil
}

// migrate brings db up to the newest known version, forward only. It runs on
// the single write handle, before anything else is allowed to touch the file.
func migrate(ctx context.Context, db *sql.DB, dbPath string, migrations []migration) error {
	haveCalls, err := tableExists(ctx, db, "calls")
	if err != nil {
		return err
	}
	haveMigrations, err := tableExists(ctx, db, "schema_migrations")
	if err != nil {
		return err
	}

	// D7. A database written by the Python hub has a calls table and no version
	// record. It is refused rather than adopted: it is missing every column 001
	// declares, so adopting it would turn a clear startup failure into an
	// obscure failure at the first insert. There is deliberately no upgrade
	// path - no deployment's call log is worth keeping, and a compatibility
	// path would be written once, run once, and maintained forever.
	if haveCalls && !haveMigrations {
		return fmt.Errorf(
			"%s predates the migration system (it has a calls table but no schema_migrations); "+
				"there is no upgrade path for it - delete the file and let the hub recreate it", dbPath)
	}

	if _, err := db.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	var current int
	if err := db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_migrations").Scan(&current); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}

	// D8. Refusing beats guessing: a newer schema may have dropped a column
	// this binary still writes to, and we would only find out one row at a time.
	newest := migrations[len(migrations)-1].version
	if current > newest {
		return fmt.Errorf(
			"%s is at schema version %d but this build only knows version %d; "+
				"it was written by a newer hub - run that one, or restore an older database",
			dbPath, current, newest)
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return fmt.Errorf("migration %03d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

// applyMigration runs one file and records it in the same transaction, so a
// half-applied schema cannot outlive a crash (D5).
func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
		m.version, m.name, FormatTime(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRowContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&found)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("inspecting schema for %q: %w", name, err)
	}
	return true, nil
}
