package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentToolAllowRoundTripsAndNullMeansNone(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, tempDB(t))
	with, err := s.CreateAgent(ctx, Agent{Name: "with", ToolAllow: []string{"file_read", "git_*"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	without, err := s.CreateAgent(ctx, Agent{Name: "without"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetAgent(ctx, with.ID)
	if strings.Join(got.ToolAllow, ",") != "file_read,git_*" {
		t.Fatalf("ToolAllow = %v", got.ToolAllow)
	}
	got, _ = s.GetAgent(ctx, without.ID)
	if got.ToolAllow != nil {
		t.Fatalf("ToolAllow = %v, want nil", got.ToolAllow)
	}
	var raw string
	if err := s.read.QueryRow("SELECT COALESCE(tool_allow,'NULL') FROM agents WHERE id = ?", without.ID).Scan(&raw); err != nil || raw != "NULL" {
		t.Fatalf("column = %q, %v", raw, err)
	}
	b, _ := json.Marshal(with)
	if !strings.Contains(string(b), `"toolAllow":["file_read","git_*"]`) {
		t.Errorf("json = %s", b)
	}
	list, _ := s.ListAgents(ctx, AgentFilter{})
	found := false
	for _, a := range list {
		if a.ID == with.ID && len(a.ToolAllow) == 2 {
			found = true
		}
	}
	if !found {
		t.Error("ListAgents lost ToolAllow")
	}
}
