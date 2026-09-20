package registry

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
)

func TestComposeToolName(t *testing.T) {
	cases := []struct {
		name    string
		scope   Scope
		label   string
		project string
		server  string
		tool    string
		want    string
		wantOK  bool
	}{
		{"all with project", ScopeAll, "box", "site", "fs", "read", "box__site__fs__read", true},
		{"all without project", ScopeAll, "box", "", "fs", "read", "box__fs__read", true},
		{"agent names as all", ScopeAgent, "box", "site", "fs", "read", "box__site__fs__read", true},
		{"agent without project", ScopeAgent, "box", "", "fs", "read", "box__fs__read", true},
		{"host with project", ScopeHost, "box", "site", "fs", "read", "site__fs__read", true},
		{"host without project", ScopeHost, "box", "", "fs", "read", "fs__read", true},
		{"project", ScopeProject, "box", "site", "fs", "read", "fs__read", true},
		{"project ignores project name", ScopeProject, "box", "", "fs", "read", "fs__read", true},
		{"server", ScopeServer, "box", "site", "fs", "read", "read", true},
		{"project_server", ScopeProjectServer, "box", "site", "fs", "read", "read", true},
		{"dots are legal", ScopeServer, "box", "", "fs", "switchboard.mcp.grant", "switchboard.mcp.grant", true},
		{"dashes and underscores", ScopeProject, "box", "", "fs-v2", "read_file", "fs-v2__read_file", true},
		{"space is illegal", ScopeAll, "box", "", "fs", "read file", "", false},
		{"slash is illegal", ScopeAll, "box", "", "fs", "read/file", "", false},
		// I4's charset allows "_", so a separator inside an upstream tool name
		// passes; only label, project and server are barred from containing one
		// (protocol.ValidateName), which is what keeps the prefix unambiguous.
		{"separator in tool is allowed", ScopeAll, "box", "", "fs", "read__file", "box__fs__read__file", true},
		{"empty tool is illegal", ScopeAll, "box", "", "fs", "", "", false},
		{"unicode is illegal", ScopeServer, "box", "", "fs", "olvasás", "", false},
		{"oversized composed name", ScopeAll, strings.Repeat("l", 60), strings.Repeat("p", 60), "fs", "read", "", false},
		{"oversized tool alone", ScopeServer, "box", "", "fs", strings.Repeat("t", 129), "", false},
		{"exactly at the limit", ScopeServer, "box", "", "fs", strings.Repeat("t", 128), strings.Repeat("t", 128), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ComposeToolName(tc.scope, tc.label, tc.project, tc.server, tc.tool)
			if ok != tc.wantOK || got != tc.want {
				t.Fatalf("ComposeToolName = %q, %v; want %q, %v", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestServerRefString(t *testing.T) {
	if got := (ServerRef{Label: "box", Project: "site", Server: "fs"}).String(); got != "box/site/fs" {
		t.Fatalf("String = %q", got)
	}
	if got := (ServerRef{Label: "box", Server: "fs"}).String(); got != "box/fs" {
		t.Fatalf("String = %q", got)
	}
}

func TestServerRefMatches(t *testing.T) {
	target := ServerRef{Label: "box", Project: "site", Server: "fs"}
	projectless := ServerRef{Label: "box", Server: "fs"}

	cases := []struct {
		name    string
		pattern ServerRef
		target  ServerRef
		want    bool
	}{
		{"exact", target, target, true},
		{"everything", ServerRef{"*", "*", "*"}, target, true},
		{"everything matches projectless", ServerRef{"*", "*", "*"}, projectless, true},
		{"label wildcard", ServerRef{"*", "site", "fs"}, target, true},
		{"project wildcard", ServerRef{"box", "*", "fs"}, target, true},
		{"server wildcard", ServerRef{"box", "site", "*"}, target, true},
		{"wrong label", ServerRef{"other", "*", "*"}, target, false},
		{"wrong project", ServerRef{"box", "other", "fs"}, target, false},
		{"wrong server", ServerRef{"box", "site", "git"}, target, false},
		{"empty project is a value, not a wildcard", ServerRef{"box", "", "fs"}, target, false},
		{"empty project matches projectless", ServerRef{"box", "", "fs"}, projectless, true},
		{"wildcard on the concrete side is a literal", ServerRef{"box", "site", "fs"}, ServerRef{"*", "*", "*"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.pattern.Matches(tc.target); got != tc.want {
				t.Fatalf("%v.Matches(%v) = %v; want %v", tc.pattern, tc.target, got, tc.want)
			}
		})
	}
}

func TestServerRefSpecificityOrders(t *testing.T) {
	refs := []ServerRef{
		{"*", "*", "*"},
		{"box", "*", "*"},
		{"box", "site", "*"},
		{"box", "site", "fs"},
	}
	for i, ref := range refs {
		if got := ref.Specificity(); got != i {
			t.Fatalf("%v.Specificity() = %d; want %d", ref, got, i)
		}
	}
	if got := (ServerRef{"box", "", "fs"}).Specificity(); got != 3 {
		t.Fatalf("an empty project is a concrete value: got %d, want 3", got)
	}
}

// -- registry -----------------------------------------------------------

func newTestRegistry(t *testing.T) (*Registry, *busTap) {
	t.Helper()
	bus := events.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, unsubscribe := bus.Subscribe(ctx)
	t.Cleanup(unsubscribe)
	return New(bus), &busTap{ch: ch}
}

// busTap is a tiny test helper around a subscription.
type busTap struct{ ch <-chan events.Event }

func (b *busTap) drain() []events.Event {
	var out []events.Event
	for {
		select {
		case ev := <-b.ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func testConnection(id, label string) *Connection {
	return NewConnection(id, label, protocol.ClientInfo{Name: "mcp-switchboard-client", Label: label},
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		func(context.Context, any) error { return nil })
}

func withServer(connection *Connection, name, project string, tools ...string) *ServerChannel {
	channel := NewServerChannel(connection.ID, connection.Label,
		protocol.ServerDecl{Name: name, Project: project, Command: name + " --stdio"})
	infos := make([]ToolInfo, 0, len(tools))
	for _, tool := range tools {
		infos = append(infos, ToolInfo{Name: tool, Description: "does " + tool})
	}
	channel.SetTools(infos)
	channel.SetState(protocol.StateRunning, "", nil)
	connection.AddServer(channel)
	return channel
}

func TestRegistryConnections(t *testing.T) {
	reg, bus := newTestRegistry(t)

	first := testConnection("c1-abcdefgh-rest", "box")
	reg.AddConnection(first)
	if got := reg.Get("c1-abcdefgh-rest"); got != first {
		t.Fatal("Get did not return the connection just added")
	}
	if !reg.LabelInUse("box") {
		t.Fatal("LabelInUse should see the label")
	}
	if reg.LabelInUse("other") {
		t.Fatal("LabelInUse matched a label nobody holds")
	}
	if len(reg.Connections()) != 1 {
		t.Fatalf("Connections = %d; want 1", len(reg.Connections()))
	}

	if got := reg.RemoveConnection("c1-abcdefgh-rest"); got != first {
		t.Fatal("RemoveConnection did not return the removed connection")
	}
	if got := reg.RemoveConnection("c1-abcdefgh-rest"); got != nil {
		t.Fatal("removing twice should return nil")
	}
	if reg.LabelInUse("box") {
		t.Fatal("label should be free again")
	}

	// One event for the add, one for the remove; the second removal changed
	// nothing and must not announce anything.
	got := bus.drain()
	if len(got) != 2 {
		t.Fatalf("published %d events; want 2 (%v)", len(got), got)
	}
	for _, ev := range got {
		if ev.Type != events.TypeConnections {
			t.Fatalf("unexpected event %+v", ev)
		}
	}
}

func TestRegistryIterServersAndFind(t *testing.T) {
	reg, _ := newTestRegistry(t)

	box := testConnection("c1", "box")
	withServer(box, "fs", "site", "read")
	withServer(box, "git", "", "log")
	reg.AddConnection(box)

	laptop := testConnection("c2", "laptop")
	withServer(laptop, "fs", "", "read")
	reg.AddConnection(laptop)

	if got := len(reg.IterServers()); got != 3 {
		t.Fatalf("IterServers = %d entries; want 3", got)
	}
	if got := reg.FindServer("c1", "git"); got == nil || got.Name != "git" {
		t.Fatalf("FindServer = %v", got)
	}
	if got := reg.FindServer("c1", "nope"); got != nil {
		t.Fatal("FindServer found a server that does not exist")
	}
	if got := reg.FindServer("nope", "git"); got != nil {
		t.Fatal("FindServer found a connection that does not exist")
	}

	connection, channel := reg.FindByLabel("laptop", "fs")
	if connection == nil || connection.ID != "c2" || channel.Project != "" {
		t.Fatalf("FindByLabel = %v, %v", connection, channel)
	}
	if connection, _ := reg.FindByLabel("laptop", "git"); connection != nil {
		t.Fatal("FindByLabel crossed connections")
	}

	if got := box.RemoveServer("git"); got == nil {
		t.Fatal("RemoveServer returned nothing")
	}
	if got := len(reg.IterServers()); got != 2 {
		t.Fatalf("IterServers = %d entries after removal; want 2", got)
	}
}

func TestRegistryResolveTool(t *testing.T) {
	reg, _ := newTestRegistry(t)
	box := testConnection("c1", "box")
	withServer(box, "fs", "site", "read", "write")
	withServer(box, "git", "", "log")
	reg.AddConnection(box)

	cases := []struct {
		name    string
		scope   Scope
		exposed string
		filter  Filter
		want    string // "server/tool", or "" for no match
	}{
		{"all", ScopeAll, "box__site__fs__read", Filter{}, "fs/read"},
		{"all, projectless server", ScopeAll, "box__git__log", Filter{}, "git/log"},
		{"agent resolves like all", ScopeAgent, "box__site__fs__write", Filter{}, "fs/write"},
		{"host", ScopeHost, "site__fs__read", Filter{Label: Only("box")}, "fs/read"},
		{"project", ScopeProject, "fs__read", Filter{Label: Only("box"), Project: Only("site")}, "fs/read"},
		{"server", ScopeServer, "log", Filter{Label: Only("box"), Server: Only("git")}, "git/log"},
		{"project_server", ScopeProjectServer, "read", Filter{Label: Only("box"), Project: Only("site"), Server: Only("fs")}, "fs/read"},
		{"empty project filter excludes a projected server", ScopeServer, "read", Filter{Project: Only("")}, ""},
		{"wrong label", ScopeAll, "box__site__fs__read", Filter{Label: Only("laptop")}, ""},
		{"unknown name", ScopeAll, "box__site__fs__delete", Filter{}, ""},
		{"right name, wrong scope", ScopeHost, "box__site__fs__read", Filter{}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connection, channel, tool, ok := reg.ResolveTool(tc.scope, tc.exposed, tc.filter)
			if tc.want == "" {
				if ok {
					t.Fatalf("resolved %q to %s/%s; want no match", tc.exposed, channel.Name, tool)
				}
				return
			}
			if !ok {
				t.Fatalf("did not resolve %q", tc.exposed)
			}
			if got := channel.Name + "/" + tool; got != tc.want {
				t.Fatalf("resolved to %s; want %s", got, tc.want)
			}
			if connection.Label != "box" {
				t.Fatalf("resolved through connection %q", connection.Label)
			}
		})
	}
}

// A reconnect gives the same server a new connection id. Re-composing names on
// every lookup is what stops the old entry from answering.
func TestResolveToolAfterReconnect(t *testing.T) {
	reg, _ := newTestRegistry(t)
	old := testConnection("c1", "box")
	withServer(old, "fs", "", "read")
	reg.AddConnection(old)
	reg.RemoveConnection("c1")

	fresh := testConnection("c2", "box")
	withServer(fresh, "fs", "", "read")
	reg.AddConnection(fresh)

	connection, _, _, ok := reg.ResolveTool(ScopeAll, "box__fs__read", Filter{})
	if !ok || connection.ID != "c2" {
		t.Fatalf("resolved through %v; want the reconnected c2", connection)
	}
}

func TestRegistryCounts(t *testing.T) {
	reg, _ := newTestRegistry(t)
	box := testConnection("c1", "box")
	withServer(box, "fs", "", "read")
	failed := withServer(box, "git", "", "log")
	exit := 1
	failed.SetState(protocol.StateFailed, "boom", &exit)
	reg.AddConnection(box)
	reg.AddConnection(testConnection("c2", "laptop"))

	connections, byState := reg.Counts()
	if connections != 2 {
		t.Fatalf("connections = %d; want 2", connections)
	}
	if byState[protocol.StateRunning] != 1 || byState[protocol.StateFailed] != 1 {
		t.Fatalf("byState = %v", byState)
	}
}

func TestSnapshotJSONShape(t *testing.T) {
	reg, _ := newTestRegistry(t)
	box := testConnection("c1", "box")
	channel := withServer(box, "fs", "site")
	channel.SetTools([]ToolInfo{
		{Name: "read", Title: "Read", Description: "read a file", InputSchema: map[string]any{"type": "object"}},
		{Name: "read file", Description: "dropped: illegal name"},
	})
	reg.AddConnection(box)

	data, err := json.Marshal(reg.Snapshot())
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Connections []struct {
			ID          string          `json:"id"`
			Label       string          `json:"label"`
			Client      json.RawMessage `json:"client"`
			ConnectedAt string          `json:"connectedAt"`
			Servers     []struct {
				Name      string `json:"name"`
				Project   string `json:"project"`
				Command   string `json:"command"`
				State     string `json:"state"`
				Error     string `json:"error"`
				ExitCode  *int   `json:"exitCode"`
				ToolCount int    `json:"toolCount"`
				Tools     []struct {
					Name        string          `json:"name"`
					ExposedName *string         `json:"exposedName"`
					Title       string          `json:"title"`
					Description string          `json:"description"`
					InputSchema json.RawMessage `json:"inputSchema"`
				} `json:"tools"`
			} `json:"servers"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	if len(decoded.Connections) != 1 {
		t.Fatalf("snapshot: %s", data)
	}
	connection := decoded.Connections[0]
	if connection.ID != "c1" || connection.Label != "box" || connection.ConnectedAt != "2026-01-01T00:00:00Z" {
		t.Fatalf("connection: %s", data)
	}
	if len(connection.Servers) != 1 {
		t.Fatalf("servers: %s", data)
	}
	server := connection.Servers[0]
	if server.Name != "fs" || server.Project != "site" || server.Command != "fs --stdio" {
		t.Fatalf("server: %s", data)
	}
	if server.State != protocol.StateRunning || server.Error != "" || server.ExitCode != nil {
		t.Fatalf("state: %s", data)
	}
	if server.ToolCount != 2 || len(server.Tools) != 2 {
		t.Fatalf("tools: %s", data)
	}
	if server.Tools[0].ExposedName == nil || *server.Tools[0].ExposedName != "box__site__fs__read" {
		t.Fatalf("exposedName: %s", data)
	}
	if server.Tools[0].Title != "Read" || string(server.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tool detail: %s", data)
	}
	// A tool whose name cannot be composed is still listed, with a null
	// exposedName, so the console can show that it exists but is unreachable.
	if server.Tools[1].ExposedName != nil {
		t.Fatalf("dropped tool should have a null exposedName: %s", data)
	}
	if string(server.Tools[1].InputSchema) != "{}" {
		t.Fatalf("a missing schema should render as an empty object: %s", data)
	}
}

// Reads must stay clean while connections come and go and servers change state
// underneath them (spec.md G1).
func TestRegistryConcurrentReadsAndWrites(t *testing.T) {
	reg, _ := newTestRegistry(t)
	stable := testConnection("stable", "box")
	channel := withServer(stable, "fs", "site", "read")
	reg.AddConnection(stable)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				reg.ResolveTool(ScopeAll, "box__site__fs__read", Filter{})
				reg.Snapshot()
				reg.Counts()
				reg.LabelInUse("box")
				reg.FindByLabel("box", "fs")
			}
		}()
	}

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for round := 0; round < 200; round++ {
				connection := testConnection(string(rune('a'+worker))+"-conn", "label")
				withServer(connection, "fs", "", "read")
				reg.AddConnection(connection)
				channel.SetState(protocol.StateRunning, "", nil)
				channel.SetTools([]ToolInfo{{Name: "read"}, {Name: "write"}})
				reg.RemoveConnection(connection.ID)
			}
		}(i)
	}

	// The readers run until the writers are done; the point is the race
	// detector, not a count.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	wg.Wait()
	cancel()
}
