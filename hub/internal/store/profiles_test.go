package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

// A database migrated only to 001 (the deployed dev database) upgrades to 002
// with its rows intact and the new columns empty.
func TestUpgradeFromVersion1PreservesData(t *testing.T) {
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
	if err := applyMigration(ctx, raw, all[0]); err != nil {
		t.Fatalf("applying 001: %v", err)
	}
	now := FormatTime(nowUTC())
	for _, q := range []string{
		`INSERT INTO agents (id, name, created_at, updated_at, last_activity_at) VALUES ('a1','old','` + now + `','` + now + `','` + now + `')`,
		`INSERT INTO chats (id, agent_id, title, created_at, updated_at) VALUES ('c1','a1','legacy','` + now + `','` + now + `')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("seeding v1 data: %v", err)
		}
	}
	raw.Close()

	s := openStore(t, path)
	var version int
	if err := s.read.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 8 {
		t.Fatalf("version = %d (%v), want 8", version, err)
	}
	a, err := s.GetAgent(ctx, "a1")
	if err != nil || a == nil || a.Name != "old" || a.ProfileID != nil {
		t.Fatalf("agent after upgrade: %+v %v", a, err)
	}
	c, err := s.GetChat(ctx, "c1")
	if err != nil || c == nil || c.Title != "legacy" || c.ProfileID != nil || c.ClientLabel != nil {
		t.Fatalf("chat after upgrade: %+v %v", c, err)
	}
	if n, err := s.CountProfiles(ctx); err != nil || n != 0 {
		t.Fatalf("profiles after upgrade = %d %v", n, err)
	}
}

func TestProfilesCRUDDefaultAndDelete(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))

	a, err := s.CreateProfile(ctx, Profile{Name: "A", SystemPrompt: "x", IsDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateProfile(ctx, Profile{Name: "B", SystemPrompt: "y", Capabilities: Capabilities{CanSpawn: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProfile(ctx, Profile{Name: "A"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("duplicate name: %v", err)
	}
	// Creating with IsDefault moves the default atomically.
	c, err := s.CreateProfile(ctx, Profile{Name: "C", IsDefault: true})
	if err != nil {
		t.Fatal(err)
	}
	def, _ := s.GetDefaultProfile(ctx)
	if def == nil || def.ID != c.ID {
		t.Fatalf("default = %+v", def)
	}
	if _, err := s.SetDefaultProfile(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListProfiles(ctx)
	if len(list) != 3 || list[0].ID != b.ID || !list[0].IsDefault || list[1].Name != "A" {
		t.Fatalf("list order/default wrong: %+v", list)
	}
	defaults := 0
	for _, p := range list {
		if p.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Fatalf("%d defaults", defaults)
	}
	// The schema itself refuses a second default.
	if _, err := s.write.Exec("UPDATE profiles SET is_default = 1 WHERE id = ?", a.ID); err == nil {
		t.Fatal("two defaults allowed")
	}
	if _, err := s.SetDefaultProfile(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetDefault unknown: %v", err)
	}

	name := "A2"
	p, err := s.UpdateProfile(ctx, a.ID, ProfilePatch{Name: &name, Model: []byte(`{"provider":"p","model":"m"}`)})
	if err != nil || p.Name != "A2" || string(p.Model) != `{"provider":"p","model":"m"}` {
		t.Fatalf("update: %+v %v", p, err)
	}
	p, err = s.UpdateProfile(ctx, a.ID, ProfilePatch{ClearModel: true})
	if err != nil || p.Model != nil {
		t.Fatalf("clear model: %+v %v", p, err)
	}
	dup := "B"
	if _, err := s.UpdateProfile(ctx, a.ID, ProfilePatch{Name: &dup}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("rename onto taken: %v", err)
	}

	// Deleting nulls references on chats and agents.
	ag, err := s.CreateAgent(ctx, Agent{Name: "inst", ProfileID: &a.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.CreateChat(ctx, Chat{AgentID: ag.ID, ProfileID: &a.ID, ClientLabel: sp("laptop")})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetChat(ctx, ch.ID)
	if got.ProfileID == nil || *got.ProfileID != a.ID || got.ClientLabel == nil || *got.ClientLabel != "laptop" {
		t.Fatalf("chat columns not round-tripped: %+v", got)
	}
	if err := s.DeleteProfile(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetChat(ctx, ch.ID)
	gotA, _ := s.GetAgent(ctx, ag.ID)
	if got.ProfileID != nil || gotA.ProfileID != nil || got.ClientLabel == nil {
		t.Fatalf("references not nulled: chat %+v agent %+v", got, gotA)
	}
	if err := s.DeleteProfile(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if n, _ := s.CountProfiles(ctx); n != 2 {
		t.Fatalf("count = %d", n)
	}
}

func nowUTC() time.Time { return time.Now().UTC() }
