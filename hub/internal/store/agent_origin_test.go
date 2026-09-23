package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestMigration004BackfillsOriginAndClient(t *testing.T) {
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
	for _, m := range all[:3] {
		if err := applyMigration(ctx, raw, m); err != nil {
			t.Fatalf("applying %d: %v", m.version, err)
		}
	}
	now := FormatTime(nowUTC())
	ts := "'" + now + "','" + now + "','" + now + "'"
	for _, q := range []string{
		// root with a chat that recorded a client
		`INSERT INTO agents (id, name, created_at, updated_at, last_activity_at) VALUES ('root','root',` + ts + `)`,
		`INSERT INTO chats (id, agent_id, title, created_at, updated_at, client_label) VALUES ('c1','root','t','` + now + `','` + now + `','laptop')`,
		// child spawn: label from its single concrete grant
		`INSERT INTO agents (id, parent_id, name, depth, created_at, updated_at, last_activity_at) VALUES ('kid','root','kid',1,` + ts + `)`,
		`INSERT INTO grants (agent_id, label, project, server, allowed, source, created_at, updated_at) VALUES ('kid','laptop','*','*',1,'inherited','` + now + `','` + now + `')`,
		// root with only a wildcard grant: no client
		`INSERT INTO agents (id, name, created_at, updated_at, last_activity_at) VALUES ('wild','wild',` + ts + `)`,
		`INSERT INTO grants (agent_id, label, project, server, allowed, source, created_at, updated_at) VALUES ('wild','*','*','*',1,'explicit','` + now + `','` + now + `')`,
		// root with two labels: no client
		`INSERT INTO agents (id, name, created_at, updated_at, last_activity_at) VALUES ('two','two',` + ts + `)`,
		`INSERT INTO grants (agent_id, label, project, server, allowed, source, created_at, updated_at) VALUES ('two','a','*','*',1,'explicit','` + now + `','` + now + `')`,
		`INSERT INTO grants (agent_id, label, project, server, allowed, source, created_at, updated_at) VALUES ('two','b','*','*',1,'explicit','` + now + `','` + now + `')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("seeding v3 data: %v\n%s", err, q)
		}
	}
	raw.Close()

	s := openStore(t, path)
	want := map[string]struct {
		origin string
		client string // "" = nil
	}{
		"root": {"chat", "laptop"}, "kid": {"spawn", "laptop"}, "wild": {"chat", ""}, "two": {"chat", ""},
	}
	for id, w := range want {
		a, err := s.GetAgent(ctx, id)
		if err != nil || a == nil {
			t.Fatalf("%s: %v", id, err)
		}
		got := ""
		if a.ClientLabel != nil {
			got = *a.ClientLabel
		}
		if a.Origin != w.origin || got != w.client {
			t.Errorf("%s: origin=%q client=%q, want %q %q", id, a.Origin, got, w.origin, w.client)
		}
	}
}

func TestSetAgentClientRewritesGrantsAndNarrowsDescendants(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	lab := func(v string) *string { return &v }
	root, err := s.CreateAgent(ctx, Agent{Name: "root", Origin: OriginManual, ClientLabel: lab("a")},
		[]Grant{{Label: "a", Project: "*", Server: "*", Allowed: true, Source: GrantExplicit}})
	if err != nil {
		t.Fatal(err)
	}
	kid, err := s.CreateAgent(ctx, Agent{Name: "kid", ParentID: &root.ID, ClientLabel: lab("a")},
		[]Grant{{Label: "a", Project: "*", Server: "*", Allowed: true, Source: GrantInherited}})
	if err != nil {
		t.Fatal(err)
	}
	if kid.Origin != OriginSpawn || root.Origin != OriginManual {
		t.Fatalf("origins: %q %q", root.Origin, kid.Origin)
	}
	rev, err := s.SetAgentClient(ctx, root.ID, lab("b"))
	if err != nil || len(rev) != 1 {
		t.Fatalf("revoked = %+v %v", rev, err)
	}
	rg, _ := s.ListGrants(ctx, root.ID)
	if len(rg) != 1 || rg[0].Label != "b" || rg[0].Source != GrantExplicit || rg[0].Project != "*" || rg[0].Server != "*" {
		t.Fatalf("root grants = %+v", rg)
	}
	if kg, _ := s.ListGrants(ctx, kid.ID); len(kg) != 0 {
		t.Fatalf("kid grants = %+v", kg)
	}
	if a, _ := s.GetAgent(ctx, root.ID); a.ClientLabel == nil || *a.ClientLabel != "b" {
		t.Fatalf("root client = %v", a.ClientLabel)
	}
	if a, _ := s.GetAgent(ctx, kid.ID); a.ClientLabel != nil {
		t.Fatalf("kid client = %v", *a.ClientLabel)
	}
	if _, err := s.SetAgentClient(ctx, root.ID, nil); err != nil {
		t.Fatal(err)
	}
	if rg, _ := s.ListGrants(ctx, root.ID); len(rg) != 0 {
		t.Fatalf("root grants after none = %+v", rg)
	}
}
