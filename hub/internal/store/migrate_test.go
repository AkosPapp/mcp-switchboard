package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "calls.db")
}

func openStore(t *testing.T, path string) *SQLiteStore {
	t.Helper()
	s, err := Open(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrationsApplyToEmptyFileAndAreIdempotent(t *testing.T) {
	path := tempDB(t)

	s := openStore(t, path)
	var version int
	if err := s.read.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatalf("reading version: %v", err)
	}
	if version != 9 {
		t.Fatalf("version = %d, want 9", version)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Opening again must apply nothing and leave exactly the recorded versions.
	again := openStore(t, path)
	var rows int
	if err := again.read.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&rows); err != nil {
		t.Fatalf("counting migrations: %v", err)
	}
	if rows != 9 {
		t.Fatalf("schema_migrations has %d rows after reopen, want 9", rows)
	}
	if _, err := again.ListCalls(context.Background(), CallFilter{}); err != nil {
		t.Fatalf("store unusable after reopen: %v", err)
	}
}

func TestPreMigrationsDatabaseIsRefused(t *testing.T) {
	path := tempDB(t)

	raw, err := sql.Open(driverName, dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec("CREATE TABLE calls (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("creating legacy table: %v", err)
	}
	raw.Close()

	_, err = Open(context.Background(), Options{Path: path})
	if err == nil {
		t.Fatal("Open accepted a pre-migrations database")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "delete") {
		t.Errorf("error does not say to delete the file: %v", err)
	}
}

func TestFutureSchemaVersionIsRefused(t *testing.T) {
	path := tempDB(t)
	openStore(t, path).Close()

	raw, err := sql.Open(driverName, dsn(path))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(
		"INSERT INTO schema_migrations (version, name, applied_at) VALUES (99, 'from_the_future', '')"); err != nil {
		t.Fatalf("faking a future version: %v", err)
	}
	raw.Close()

	_, err = Open(context.Background(), Options{Path: path})
	if err == nil {
		t.Fatal("Open accepted a database newer than the binary")
	}
	if !strings.Contains(err.Error(), "99") || !strings.Contains(err.Error(), path) {
		t.Errorf("error should name the file and the version: %v", err)
	}
}

func TestMigrationNumberingMustBeContiguous(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"gap": {
			"m/001_initial.sql": {Data: []byte("SELECT 1;")},
			"m/003_later.sql":   {Data: []byte("SELECT 1;")},
		},
		"does not start at one": {
			"m/002_second.sql": {Data: []byte("SELECT 1;")},
		},
		"unnumbered": {
			"m/initial.sql": {Data: []byte("SELECT 1;")},
		},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadMigrations(fsys, "m"); err == nil {
				t.Fatal("loadMigrations accepted a broken migration set")
			}
		})
	}

	ok := fstest.MapFS{
		"m/001_initial.sql": {Data: []byte("SELECT 1;")},
		"m/002_second.sql":  {Data: []byte("SELECT 1;")},
		"m/notes.txt":       {Data: []byte("ignored")},
	}
	got, err := loadMigrations(ok, "m")
	if err != nil {
		t.Fatalf("loadMigrations rejected a valid set: %v", err)
	}
	if len(got) != 2 || got[0].version != 1 || got[1].name != "second" {
		t.Fatalf("unexpected migrations: %+v", got)
	}
}

// FTS5 is not used by 001, but the chat search of U6 will need it and E1 pins
// the driver by name because pure-Go drivers differ on shipping it. A failure
// here invalidates a spec decision rather than just this test.
func TestDriverSupportsFTS5(t *testing.T) {
	s := openStore(t, tempDB(t))
	ctx := context.Background()

	if _, err := s.write.ExecContext(ctx, "CREATE VIRTUAL TABLE t USING fts5(x)"); err != nil {
		t.Fatalf("driver %q has no FTS5: %v", driverName, err)
	}
	if _, err := s.write.ExecContext(ctx,
		"INSERT INTO t (x) VALUES ('the quick brown fox'), ('a slow green turtle')"); err != nil {
		t.Fatalf("inserting into FTS5 table: %v", err)
	}

	var got string
	if err := s.read.QueryRowContext(ctx, "SELECT x FROM t WHERE t MATCH 'brown'").Scan(&got); err != nil {
		t.Fatalf("FTS5 MATCH query failed: %v", err)
	}
	if got != "the quick brown fox" {
		t.Fatalf("MATCH returned %q", got)
	}
}
