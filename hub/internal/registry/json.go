package registry

import (
	"sort"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
)

// timeLayout is the shape the console parses connectedAt in: RFC 3339 with
// nanoseconds, the nearest thing Go has to Python's datetime.isoformat().
const timeLayout = time.RFC3339Nano

// The snapshot shapes below are consumed by the console and by the REST API's
// tests, so the field names are part of the hub's contract and match the Python
// hub's to_json() exactly. They are structs rather than maps for the same
// reason: a typo in a map key is a runtime surprise, a typo in a tag is a diff.

// ToolJSON is one tool as the console shows it. ExposedName is null when the
// tool was dropped from the catalog under I4, which is how a reader tells "this
// tool exists upstream but nobody can call it" from "this tool is callable".
//
// The optional fields are pointers so that "absent" renders as JSON null rather
// than as an empty string. Go has no None and the temptation is to collapse the
// two, but the console and the end-to-end suite both read these as the Python
// hub wrote them - `server.project is None` is an assertion in tests/ - and a
// consumer distinguishing "no project" from "a project named empty" has every
// right to.
type ToolJSON struct {
	Name        string         `json:"name"`
	ExposedName *string        `json:"exposedName"`
	Title       *string        `json:"title"`
	Description *string        `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// ServerJSON is one server channel.
type ServerJSON struct {
	Name      string     `json:"name"`
	Project   *string    `json:"project"`
	Command   string     `json:"command"`
	State     string     `json:"state"`
	Error     *string    `json:"error"`
	ExitCode  *int       `json:"exitCode"`
	ToolCount int        `json:"toolCount"`
	Tools     []ToolJSON `json:"tools"`
}

// nullable renders an empty string as JSON null, which is what the Python hub
// this replaces put on the wire for every one of these fields.
func nullable(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// ConnectionJSON is one connected client and everything it carries.
type ConnectionJSON struct {
	ID          string              `json:"id"`
	Label       string              `json:"label"`
	Client      protocol.ClientInfo `json:"client"`
	ConnectedAt string              `json:"connectedAt"`
	Servers     []ServerJSON        `json:"servers"`
}

// SnapshotJSON is the whole live tree, the body of GET /api/connections.
type SnapshotJSON struct {
	Connections []ConnectionJSON `json:"connections"`
}

// ToJSON renders one server, naming its tools as the given scope would.
func (c *ServerChannel) ToJSON(scope Scope) ServerJSON {
	state, errMsg, exitCode := c.State()
	tools := c.Tools()

	out := make([]ToolJSON, 0, len(tools))
	for _, tool := range tools {
		entry := ToolJSON{
			Name:        tool.Name,
			Title:       nullable(tool.Title),
			Description: nullable(tool.Description),
			InputSchema: tool.InputSchema,
		}
		if entry.InputSchema == nil {
			entry.InputSchema = map[string]any{}
		}
		if exposed, ok := ComposeToolName(scope, c.Label, c.Project, c.Name, tool.Name); ok {
			entry.ExposedName = &exposed
		}
		out = append(out, entry)
	}

	return ServerJSON{
		Name:      c.Name,
		Project:   nullable(c.Project),
		Command:   c.Command,
		State:     state,
		Error:     nullable(errMsg),
		ExitCode:  exitCode,
		ToolCount: len(tools),
		Tools:     out,
	}
}

// ToJSON renders one connection and its servers under ScopeAll, the naming the
// console displays.
func (c *Connection) ToJSON() ConnectionJSON {
	// Sorted, because the underlying map has no order and a console list that
	// reshuffles itself on every poll is unreadable.
	servers := c.Servers()
	sort.Slice(servers, func(i, j int) bool { return servers[i].Name < servers[j].Name })
	out := make([]ServerJSON, 0, len(servers))
	for _, channel := range servers {
		out = append(out, channel.ToJSON(ScopeAll))
	}
	return ConnectionJSON{
		ID:          c.ID,
		Label:       c.Label,
		Client:      c.Client,
		ConnectedAt: c.ConnectedAt.UTC().Format(timeLayout),
		Servers:     out,
	}
}

// Snapshot renders the whole registry.
func (r *Registry) Snapshot() SnapshotJSON {
	connections := r.Connections()
	sort.Slice(connections, func(i, j int) bool {
		if !connections[i].ConnectedAt.Equal(connections[j].ConnectedAt) {
			return connections[i].ConnectedAt.Before(connections[j].ConnectedAt)
		}
		return connections[i].ID < connections[j].ID
	})
	out := make([]ConnectionJSON, 0, len(connections))
	for _, connection := range connections {
		out = append(out, connection.ToJSON())
	}
	return SnapshotJSON{Connections: out}
}
