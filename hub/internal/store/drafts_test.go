package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestDraftRoundTripAndEmptyDeletes(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	c := mkChat(t, s, mkAgent(t, s, nil, "a").ID)

	if d, err := s.GetDraft(ctx, c.ID); err != nil || d.Content != "" || d.UpdatedAt != nil {
		t.Fatalf("empty draft = %+v %v", d, err)
	}
	if _, err := s.SetDraft(ctx, c.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDraft(ctx, c.ID)
	if err != nil || d.Content != "hello" || d.UpdatedAt == nil {
		t.Fatalf("draft = %+v %v", d, err)
	}
	if _, err := s.SetDraft(ctx, c.ID, "hello 2"); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDraft(ctx, c.ID); d.Content != "hello 2" {
		t.Fatalf("overwrite: %+v", d)
	}
	if _, err := s.SetDraft(ctx, c.ID, " \n\t"); err != nil {
		t.Fatal(err)
	}
	var n int
	s.read.QueryRow("SELECT COUNT(*) FROM chat_drafts").Scan(&n)
	if n != 0 {
		t.Fatalf("whitespace draft left %d rows", n)
	}
	if _, err := s.SetDraft(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown chat: %v", err)
	}
}

func TestDraftCascadesOnChatDelete(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	c := mkChat(t, s, mkAgent(t, s, nil, "a").ID)
	if _, err := s.SetDraft(ctx, c.ID, "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteChat(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	s.read.QueryRow("SELECT COUNT(*) FROM chat_drafts").Scan(&n)
	if n != 0 {
		t.Fatalf("draft survived chat delete: %d", n)
	}
}

func TestAppendMessageClearingDraft(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	c := mkChat(t, s, mkAgent(t, s, nil, "a").ID)
	if _, err := s.SetDraft(ctx, c.ID, "x"); err != nil {
		t.Fatal(err)
	}
	first, err := s.AppendMessageClearingDraft(ctx, Message{ChatID: c.ID, Role: RoleUser, Content: text("hi")})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDraft(ctx, c.ID); d.UpdatedAt != nil {
		t.Fatalf("draft not cleared: %+v", d)
	}
	// A failing draft delete must not lose the message.
	if _, err := s.write.Exec("DROP TABLE chat_drafts"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendMessageClearingDraft(ctx, Message{ChatID: c.ID, ParentID: &first.ID, Role: RoleUser, Content: text("again")}); err != nil {
		t.Fatalf("message lost over a draft failure: %v", err)
	}
	if p, _ := s.ActivePath(ctx, c.ID); len(p) != 2 {
		t.Fatalf("path len = %d, want 2", len(p))
	}
}

func TestUpgradeFromVersion2PreservesData(t *testing.T) {
	ctx := context.Background()
	path := tempDB(t)
	raw, err := sql.Open(driverName, dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	all, err := loadMigrations(migrationFS, migrationDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(schemaMigrationsDDL); err != nil {
		t.Fatal(err)
	}
	for _, m := range all[:2] {
		if err := applyMigration(ctx, raw, m); err != nil {
			t.Fatalf("applying %d: %v", m.version, err)
		}
	}
	now := FormatTime(nowUTC())
	for _, q := range []string{
		`INSERT INTO agents (id, name, created_at, updated_at, last_activity_at) VALUES ('a1','old','` + now + `','` + now + `','` + now + `')`,
		`INSERT INTO chats (id, agent_id, title, created_at, updated_at) VALUES ('c1','a1','legacy','` + now + `','` + now + `')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	s := openStore(t, path)
	var version int
	if err := s.read.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 9 {
		t.Fatalf("version = %d (%v)", version, err)
	}
	if c, err := s.GetChat(ctx, "c1"); err != nil || c == nil || c.Title != "legacy" {
		t.Fatalf("chat after upgrade: %+v %v", c, err)
	}
	if _, err := s.SetDraft(ctx, "c1", "d"); err != nil {
		t.Fatal(err)
	}
}
