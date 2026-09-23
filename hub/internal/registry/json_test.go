package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
)

// The Python hub put null on the wire for every absent optional field, and both
// the console and tests/test_end_to_end.py read them that way ("project is
// None"). Go has no None, so this is the test that stops the pointers from
// being "simplified" into plain strings.
func TestAbsentFieldsRenderAsNull(t *testing.T) {
	channel := NewServerChannel("cid", "box", protocol.ServerDecl{Name: "demo"})
	channel.SetTools([]ToolInfo{{Name: "echo"}})

	raw, err := json.Marshal(channel.ToJSON(ScopeAll))
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"project", "error", "exitCode"} {
		if value, present := got[field]; !present || value != nil {
			t.Errorf("%s = %#v, want null", field, value)
		}
	}

	tool := got["tools"].([]any)[0].(map[string]any)
	for _, field := range []string{"title", "description"} {
		if value, present := tool[field]; !present || value != nil {
			t.Errorf("tool %s = %#v, want null", field, value)
		}
	}
	if tool["exposedName"] != "box__demo__echo" {
		t.Errorf("exposedName = %#v", tool["exposedName"])
	}
}

func TestPresentFieldsRenderAsValues(t *testing.T) {
	channel := NewServerChannel("cid", "box", protocol.ServerDecl{Name: "lsp", Project: "nix"})
	code := 3
	channel.SetState(protocol.StateFailed, "it broke", &code)
	channel.SetTools([]ToolInfo{{Name: "echo", Title: "Echo", Description: "says it back"}})

	out := channel.ToJSON(ScopeAll)
	if out.Project == nil || *out.Project != "nix" {
		t.Errorf("project = %#v", out.Project)
	}
	if out.Error == nil || *out.Error != "it broke" {
		t.Errorf("error = %#v", out.Error)
	}
	if out.ExitCode == nil || *out.ExitCode != 3 {
		t.Errorf("exitCode = %#v", out.ExitCode)
	}
	if out.Tools[0].Title == nil || *out.Tools[0].Title != "Echo" {
		t.Errorf("title = %#v", out.Tools[0].Title)
	}
	if *out.Tools[0].ExposedName != "box__nix__lsp__echo" {
		t.Errorf("exposedName = %q", *out.Tools[0].ExposedName)
	}
}

func TestSnapshotIsOrdered(t *testing.T) {
	reg := New(nil)
	base := time.Now().UTC()
	for i, label := range []string{"c", "a", "b"} {
		conn := NewConnection(label+"-id", label, protocol.ClientInfo{Label: label},
			base.Add(time.Duration(i)*time.Second), nil)
		conn.AddServer(NewServerChannel(label+"-id", label, protocol.ServerDecl{Name: "z"}))
		conn.AddServer(NewServerChannel(label+"-id", label, protocol.ServerDecl{Name: "a"}))
		reg.AddConnection(conn)
	}

	snapshot := reg.Snapshot()
	if len(snapshot.Connections) != 3 {
		t.Fatalf("got %d connections", len(snapshot.Connections))
	}
	// Oldest first, and servers by name: a console list that reshuffles on every
	// poll is unreadable.
	if snapshot.Connections[0].Label != "c" {
		t.Errorf("connections are not ordered by connectedAt: %q first", snapshot.Connections[0].Label)
	}
	if snapshot.Connections[0].Servers[0].Name != "a" {
		t.Errorf("servers are not sorted by name: %q first", snapshot.Connections[0].Servers[0].Name)
	}
}

func TestConnectionJSONCarriesEnvironmentVerbatim(t *testing.T) {
	env := &protocol.ClientEnvironment{
		Kinds: []string{"devcontainer", "direnv"}, Project: "proj", Workspace: "/w",
		Details: map[string]string{"direnvDir": "/w"},
	}
	reg := New(nil)
	reg.AddConnection(NewConnection("a-id", "a", protocol.ClientInfo{Label: "a", Environment: env}, time.Now().UTC(), nil))
	reg.AddConnection(NewConnection("b-id", "b", protocol.ClientInfo{Label: "b"}, time.Now().UTC(), nil))

	raw, err := json.Marshal(reg.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Connections []struct {
			Label  string `json:"label"`
			Client map[string]any
		} `json:"connections"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Connections {
		e, has := c.Client["environment"]
		if c.Label == "b" {
			if has {
				t.Errorf("old client must have no environment, got %v", e)
			}
			continue
		}
		m := e.(map[string]any)
		if m["project"] != "proj" || m["workspace"] != "/w" || len(m["kinds"].([]any)) != 2 ||
			m["details"].(map[string]any)["direnvDir"] != "/w" {
			t.Errorf("environment = %v", m)
		}
	}
}
