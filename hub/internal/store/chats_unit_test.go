package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

func TestMigration005BackfillsParentChatAndKeepsData(t *testing.T) {
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
	for _, m := range all[:4] {
		if err := applyMigration(ctx, raw, m); err != nil {
			t.Fatalf("applying %d: %v", m.version, err)
		}
	}
	now := FormatTime(nowUTC())
	ts := "'" + now + "','" + now + "','" + now + "'"
	for _, q := range []string{
		`INSERT INTO agents (id, name, created_at, updated_at, last_activity_at) VALUES ('root','root',` + ts + `)`,
		`INSERT INTO agents (id, parent_id, name, depth, created_at, updated_at, last_activity_at) VALUES ('kid','root','kid',1,` + ts + `)`,
		// root has an older and a newer human chat: the older one is the parent.
		`INSERT INTO chats (id, agent_id, title, kind, created_at, updated_at) VALUES ('h1','root','first','human','2026-01-01T00:00:00+00:00','` + now + `')`,
		`INSERT INTO chats (id, agent_id, title, kind, created_at, updated_at) VALUES ('h2','root','second','human','2026-02-01T00:00:00+00:00','` + now + `')`,
		`INSERT INTO chats (id, agent_id, peer_agent_id, title, kind, created_at, updated_at) VALUES ('sp','kid','root','Spawned by root','spawn','` + now + `','` + now + `')`,
		// a legacy peer chat and an unrelated chat stay unlinked
		`INSERT INTO chats (id, agent_id, peer_agent_id, title, kind, created_at, updated_at) VALUES ('pc','root','kid','Conversation','agent','` + now + `','` + now + `')`,
		`INSERT INTO messages (id, chat_id, role, content, search_text, created_at) VALUES ('m1','h1','user','[{"type":"text","text":"hello"}]','hello','` + now + `')`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("seeding v4 data: %v\n%s", err, q)
		}
	}
	raw.Close()

	s := openStore(t, path)
	want := map[string]string{"sp": "h1", "h1": "", "h2": "", "pc": ""}
	for id, parent := range want {
		c, err := s.GetChat(ctx, id)
		if err != nil || c == nil {
			t.Fatalf("%s: %v", id, err)
		}
		got := ""
		if c.ParentChatID != nil {
			got = *c.ParentChatID
		}
		if got != parent {
			t.Errorf("chat %s parent = %q, want %q", id, got, parent)
		}
	}
	m, err := s.GetMessage(ctx, "m1")
	if err != nil || m == nil || m.Sender != nil || SearchText(m.Content) != "hello" {
		t.Fatalf("existing message changed: %+v %v", m, err)
	}
	// The new columns work on the upgraded database.
	sender := json.RawMessage(`{"chatId":"h1","chatTitle":"first","kind":"message"}`)
	got, err := s.AppendMessage(ctx, Message{ChatID: "h2", Role: RoleUser, Content: json.RawMessage(`[{"type":"text","text":"x"}]`), Sender: sender})
	if err != nil {
		t.Fatal(err)
	}
	back, _ := s.GetMessage(ctx, got.ID)
	if string(back.Sender) != string(sender) {
		t.Errorf("sender = %s", back.Sender)
	}
	// Losing the parent clears the link instead of leaving it dangling.
	if err := s.DeleteChat(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.GetChat(ctx, "sp"); c.ParentChatID != nil {
		t.Errorf("dangling parent %v", *c.ParentChatID)
	}
}

func TestMessageAndChatJSONCarrySenderAndParent(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	a, _ := s.CreateAgent(ctx, Agent{Name: "a"}, nil)
	parent, _ := s.CreateChat(ctx, Chat{AgentID: a.ID, Title: "p"})
	child, _ := s.CreateChat(ctx, Chat{AgentID: a.ID, Title: "c", ParentChatID: &parent.ID})
	b, _ := json.Marshal(child)
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	if obj["parentChatId"] != parent.ID {
		t.Fatalf("chat json = %s", b)
	}
	b, _ = json.Marshal(parent)
	_ = json.Unmarshal(b, &obj)
	if v, ok := obj["parentChatId"]; !ok || v != nil {
		t.Fatalf("top-level chat json = %s", b)
	}
	m, _ := s.AppendMessage(ctx, Message{ChatID: parent.ID, Role: RoleUser})
	b, _ = json.Marshal(m)
	_ = json.Unmarshal(b, &obj)
	if v, ok := obj["sender"]; !ok || v != nil {
		t.Fatalf("message json = %s", b)
	}
	if l, _ := s.ListChats(ctx, ChatFilter{ParentChatID: parent.ID}); len(l) != 1 || l[0].ID != child.ID {
		t.Fatalf("ListChats by parent = %+v", l)
	}
}

func TestDeleteChatCascade(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	mk := func(name string, parent *Agent) Agent {
		in := Agent{Name: name}
		if parent != nil {
			in.ParentID = &parent.ID
		}
		a, err := s.CreateAgent(ctx, in, nil)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	ra := mk("r", nil)
	ka := mk("k", &ra)
	ga := mk("g", &ka)
	shared := mk("shared", nil)
	root, _ := s.CreateChat(ctx, Chat{AgentID: ra.ID, Title: "root"})
	kid, _ := s.CreateChat(ctx, Chat{AgentID: ka.ID, Title: "kid", ParentChatID: &root.ID})
	grand, _ := s.CreateChat(ctx, Chat{AgentID: ga.ID, Title: "grand", ParentChatID: &kid.ID})
	s1, _ := s.CreateChat(ctx, Chat{AgentID: shared.ID, Title: "s1"})
	s2, _ := s.CreateChat(ctx, Chat{AgentID: shared.ID, Title: "s2"})
	for _, c := range []Chat{root, kid, grand, s1} {
		if _, err := s.AppendMessage(ctx, Message{ChatID: c.ID, Role: RoleUser}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateRun(ctx, Run{AgentID: ga.ID, ChatID: grand.ID, Trigger: TriggerHuman, Status: RunRunning}); err != nil {
		t.Fatal(err)
	}

	// A chat whose record another chat still uses keeps the record.
	del, err := s.DeleteChatCascade(ctx, s1.ID)
	if err != nil || len(del.ChatIDs) != 1 || len(del.AgentIDs) != 0 {
		t.Fatalf("shared delete = %+v %v", del, err)
	}
	if a, _ := s.GetAgent(ctx, shared.ID); a == nil {
		t.Fatal("record deleted while another chat uses it")
	}
	if del, err = s.DeleteChatCascade(ctx, s2.ID); err != nil || len(del.AgentIDs) != 1 {
		t.Fatalf("last chat delete = %+v %v", del, err)
	}
	if a, _ := s.GetAgent(ctx, shared.ID); a != nil {
		t.Fatal("unused record survived")
	}

	// The tree goes with its root: chats, records, runs, messages.
	del, err = s.DeleteChatCascade(ctx, root.ID)
	if err != nil || len(del.ChatIDs) != 3 {
		t.Fatalf("tree delete = %+v %v", del, err)
	}
	for _, id := range []string{ra.ID, ka.ID, ga.ID} {
		if a, _ := s.GetAgent(ctx, id); a != nil {
			t.Errorf("record %s survived", id)
		}
	}
	if runs, _ := s.ListRuns(ctx, RunFilter{}); len(runs) != 0 {
		t.Errorf("runs survived: %+v", runs)
	}
	if _, err := s.DeleteChatCascade(ctx, root.ID); err == nil {
		t.Error("second delete succeeded")
	}
}

func TestDeleteChatCascadeKeepsRecordWithUnlinkedChildren(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	ra, _ := s.CreateAgent(ctx, Agent{Name: "r"}, nil)
	ka, _ := s.CreateAgent(ctx, Agent{Name: "k", ParentID: &ra.ID}, nil)
	root, _ := s.CreateChat(ctx, Chat{AgentID: ra.ID})
	kidChat, _ := s.CreateChat(ctx, Chat{AgentID: ka.ID}) // legacy: not linked to its parent chat
	del, err := s.DeleteChatCascade(ctx, root.ID)
	if err != nil || len(del.AgentIDs) != 0 {
		t.Fatalf("delete = %+v %v", del, err)
	}
	if a, _ := s.GetAgent(ctx, ka.ID); a == nil {
		t.Fatal("child record was swept away")
	}
	if c, _ := s.GetChat(ctx, kidChat.ID); c == nil {
		t.Fatal("child chat deleted")
	}
}
