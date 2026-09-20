// Package registry holds the hub's live state: which clients are connected,
// what servers they carry, what tools those expose.
//
// It also owns tool-name composition, because how a tool is named is the main
// way a consumer (n8n, an agent) can tell which machine it lives on. Everything
// here is rebuilt from scratch on every hub start; nothing durable is keyed on
// anything in this package (spec.md I3).
package registry

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
)

// SEP-986. The low-level MCP server does NOT enforce this, so we do (spec.md I4).
var toolNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// MaxToolName is the ceiling on a composed name, also from I4.
const MaxToolName = 128

// Wildcard is the literal a grant uses to mean "any value in this field" (A11).
const Wildcard = "*"

// Scope says how much of the hub a given MCP endpoint exposes, and how it names
// tools. A server's project is optional, so ScopeAll and ScopeHost omit that
// component entirely for a project-less server rather than leaving a stray
// separator.
type Scope string

const (
	ScopeAll           Scope = "all"            // {label}__[{project}__]{server}__{tool}
	ScopeHost          Scope = "host"           // [{project}__]{server}__{tool}
	ScopeProject       Scope = "project"        // {server}__{tool}
	ScopeServer        Scope = "server"         // {tool}  (label+server fixed, any/no project)
	ScopeProjectServer Scope = "project_server" // {tool}  (label+project+server fixed)

	// ScopeAgent names tools exactly as ScopeAll does. It is a scope of its own
	// only so callers can say "this endpoint is an agent's"; what an agent may
	// reach is decided by its grants, not by naming.
	ScopeAgent Scope = "agent"
)

// ComposeToolName builds the name a consumer sees. ok is false when no valid
// name can be made, which keeps one malformed upstream tool from taking out a
// whole tools/list response instead of erroring the caller out.
func ComposeToolName(scope Scope, label, project, server, tool string) (string, bool) {
	origin := ServerRef{Label: label, Project: project, Server: server}.String()
	if !toolNameRE.MatchString(tool) {
		slog.Warn("dropping tool: name has characters MCP does not allow", "tool", tool, "server", origin)
		return "", false
	}

	var parts []string
	switch scope {
	case ScopeServer, ScopeProjectServer:
		parts = []string{tool}
	case ScopeProject:
		parts = []string{server, tool}
	case ScopeHost:
		parts = append(parts, optional(project)...)
		parts = append(parts, server, tool)
	default:
		parts = append(parts, label)
		parts = append(parts, optional(project)...)
		parts = append(parts, server, tool)
	}
	name := strings.Join(parts, protocol.NameSeparator)

	if len(name) > MaxToolName {
		slog.Warn("dropping tool: composed name is over the limit",
			"tool", tool, "server", origin, "length", len(name), "limit", MaxToolName)
		return "", false
	}
	return name, true
}

func optional(project string) []string {
	if project == "" {
		return nil
	}
	return []string{project}
}

// ServerRef is a server's stable identity: the triple (label, project, server)
// of spec.md I1, all three of which come from the client's configuration and so
// survive client and hub restarts. A connection id is never an identity (I2).
//
// In a grant, any field may be the literal "*" (A11).
type ServerRef struct {
	Label   string `json:"label"`
	Project string `json:"project"`
	Server  string `json:"server"`
}

// String renders the triple the way prose and logs do: label/project/server,
// with the middle segment left out when the server has no project.
func (r ServerRef) String() string {
	if r.Project == "" {
		return r.Label + "/" + r.Server
	}
	return r.Label + "/" + r.Project + "/" + r.Server
}

// Matches reports whether r, read as a pattern, covers other. Only r's fields
// are read as wildcards: a "*" on the concrete side is a server literally named
// "*", not a licence to match everything.
func (r ServerRef) Matches(other ServerRef) bool {
	return fieldMatches(r.Label, other.Label) &&
		fieldMatches(r.Project, other.Project) &&
		fieldMatches(r.Server, other.Server)
}

func fieldMatches(pattern, value string) bool {
	return pattern == Wildcard || pattern == value
}

// Specificity counts the fields that are not wildcards. Grant resolution orders
// by it so that the narrowest rule wins over a broader one.
func (r ServerRef) Specificity() int {
	n := 0
	for _, field := range []string{r.Label, r.Project, r.Server} {
		if field != Wildcard {
			n++
		}
	}
	return n
}

// ToolInfo is one tool as the upstream server described it.
type ToolInfo struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any
}

// ServerChannel is one local MCP server, reached through one connection.
//
// The identity fields are fixed at construction; everything a server's
// lifecycle changes sits behind mu, because the tunnel's read loop writes it
// while tools/list readers are in flight, and neither side should have to take
// the registry's lock to touch one server.
type ServerChannel struct {
	Name         string
	ConnectionID string
	Label        string
	Project      string
	Command      string

	mu       sync.RWMutex
	state    string
	errMsg   string
	exitCode *int
	tools    []ToolInfo
	session  any // the MCP ClientSession once initialize() has completed
}

// NewServerChannel returns a channel in the starting state, as the client
// declared it in hello.
func NewServerChannel(connectionID, label string, decl protocol.ServerDecl) *ServerChannel {
	return &ServerChannel{
		Name:         decl.Name,
		ConnectionID: connectionID,
		Label:        label,
		Project:      decl.Project,
		Command:      decl.Command,
		state:        protocol.StateStarting,
	}
}

// Ref is this server's stable identity.
func (c *ServerChannel) Ref() ServerRef {
	return ServerRef{Label: c.Label, Project: c.Project, Server: c.Name}
}

// State returns the lifecycle state, the last error and the last exit code
// together, since a consumer that reads one of them always wants the others in
// the same breath.
func (c *ServerChannel) State() (state, errMsg string, exitCode *int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state, c.errMsg, c.exitCode
}

// SetState records a server_state frame.
func (c *ServerChannel) SetState(state, errMsg string, exitCode *int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state, c.errMsg, c.exitCode = state, errMsg, exitCode
}

// Tools returns a copy, so a caller can walk the catalog while the server
// restarts and republishes it underneath.
func (c *ServerChannel) Tools() []ToolInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]ToolInfo(nil), c.tools...)
}

// SetTools replaces the catalog wholesale; a server that restarted may expose a
// different set, and merging would keep tools that no longer exist.
func (c *ServerChannel) SetTools(tools []ToolInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tools = append([]ToolInfo(nil), tools...)
}

// Session is the live MCP session, or nil before initialize() completed.
func (c *ServerChannel) Session() any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.session
}

// SetSession attaches (or, with nil, drops) the initialised session.
func (c *ServerChannel) SetSession(session any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = session
}

// Ready reports whether this server can take a tool call.
func (c *ServerChannel) Ready() bool { return c.Session() != nil }

// Connection is one connected client process, carrying any number of servers.
type Connection struct {
	ID          string
	Label       string
	Client      protocol.ClientInfo
	ConnectedAt time.Time

	// Send writes one frame to this client. It takes a context because a tool
	// call that is cancelled must not leave a writer blocked on a dead socket.
	Send func(ctx context.Context, frame any) error

	mu      sync.RWMutex
	servers map[string]*ServerChannel
}

// NewConnection returns a connection with no servers yet.
func NewConnection(id, label string, client protocol.ClientInfo, connectedAt time.Time, send func(context.Context, any) error) *Connection {
	return &Connection{
		ID:          id,
		Label:       label,
		Client:      client,
		ConnectedAt: connectedAt,
		Send:        send,
		servers:     make(map[string]*ServerChannel),
	}
}

// AddServer registers a server channel under its name, replacing any previous
// one of that name.
func (c *Connection) AddServer(channel *ServerChannel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.servers == nil {
		c.servers = make(map[string]*ServerChannel)
	}
	c.servers[channel.Name] = channel
}

// RemoveServer drops a server channel, returning it if it was there.
func (c *Connection) RemoveServer(name string) *ServerChannel {
	c.mu.Lock()
	defer c.mu.Unlock()
	channel := c.servers[name]
	delete(c.servers, name)
	return channel
}

// Server looks one server up by name.
func (c *Connection) Server(name string) *ServerChannel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.servers[name]
}

// Servers snapshots this connection's channels.
func (c *Connection) Servers() []*ServerChannel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*ServerChannel, 0, len(c.servers))
	for _, channel := range c.servers {
		out = append(out, channel)
	}
	return out
}

// ServerEntry pairs a server with the connection it is reached through, which
// is how every consumer wants them: the channel says what to call, the
// connection says where to send it.
type ServerEntry struct {
	Connection *Connection
	Channel    *ServerChannel
}

// Filter narrows a lookup. A nil field means "don't care"; an empty string is a
// real value, since a server with no project has project "".
type Filter struct {
	Label   *string
	Project *string
	Server  *string
}

// Only is shorthand for a filter field: Only("") filters for "no project".
func Only(value string) *string { return &value }

func (f Filter) matches(connection *Connection, channel *ServerChannel) bool {
	if f.Label != nil && connection.Label != *f.Label {
		return false
	}
	if f.Project != nil && channel.Project != *f.Project {
		return false
	}
	if f.Server != nil && channel.Name != *f.Server {
		return false
	}
	return true
}

// Registry is the live tree of connections and their servers.
//
// Its mutex guards the connection map only, and is never held across an I/O
// call (spec.md G1): the lock is taken, a slice of pointers is copied out, and
// released before anything is sent anywhere.
type Registry struct {
	mu          sync.RWMutex
	connections map[string]*Connection

	// bus is where "something changed" goes. The registry does not own
	// subscribers: keeping the change feed a separate component means the
	// registry stays a data structure and the bus stays reusable.
	bus *events.Bus
}

// New returns an empty registry publishing to bus, which may be nil.
func New(bus *events.Bus) *Registry {
	return &Registry{connections: make(map[string]*Connection), bus: bus}
}

// AddConnection registers a connection that has completed its hello.
func (r *Registry) AddConnection(connection *Connection) {
	r.mu.Lock()
	r.connections[connection.ID] = connection
	r.mu.Unlock()

	slog.Info("connection up", "label", connection.Label, "connection", shortID(connection.ID))
	r.PublishChange()
}

// RemoveConnection drops a connection, returning it if it was registered.
func (r *Registry) RemoveConnection(connectionID string) *Connection {
	r.mu.Lock()
	connection := r.connections[connectionID]
	delete(r.connections, connectionID)
	r.mu.Unlock()

	if connection == nil {
		return nil
	}
	slog.Info("connection down", "label", connection.Label, "connection", shortID(connectionID))
	r.PublishChange()
	return connection
}

// Get looks a connection up by its per-session id.
func (r *Registry) Get(connectionID string) *Connection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.connections[connectionID]
}

// Connections snapshots the connected clients.
func (r *Registry) Connections() []*Connection {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Connection, 0, len(r.connections))
	for _, connection := range r.connections {
		out = append(out, connection)
	}
	return out
}

// LabelInUse reports whether a client is already connected under this label.
// Labels namespace every exposed tool name, so two connections may not share one.
func (r *Registry) LabelInUse(label string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, connection := range r.connections {
		if connection.Label == label {
			return true
		}
	}
	return false
}

// IterServers returns every (connection, server) pair as a snapshot, so callers
// walk a stable list with no lock held.
func (r *Registry) IterServers() []ServerEntry {
	var out []ServerEntry
	for _, connection := range r.Connections() {
		for _, channel := range connection.Servers() {
			out = append(out, ServerEntry{Connection: connection, Channel: channel})
		}
	}
	return out
}

// FindServer looks a server up within one open session.
func (r *Registry) FindServer(connectionID, server string) *ServerChannel {
	connection := r.Get(connectionID)
	if connection == nil {
		return nil
	}
	return connection.Server(server)
}

// FindByLabel finds a server by the identity a consumer knows it under, rather
// than by the connection id, which changes on every reconnect.
func (r *Registry) FindByLabel(label, server string) (*Connection, *ServerChannel) {
	for _, entry := range r.IterServers() {
		if entry.Connection.Label == label && entry.Channel.Name == server {
			return entry.Connection, entry.Channel
		}
	}
	return nil, nil
}

// ResolveTool maps a name a consumer used back to a concrete server and tool.
//
// Resolved by re-composing candidate names rather than consulting a cached map,
// so a tunnel that reconnected mid-session can never be resolved through a
// stale entry.
func (r *Registry) ResolveTool(scope Scope, exposedName string, filter Filter) (*Connection, *ServerChannel, string, bool) {
	for _, entry := range r.IterServers() {
		if !filter.matches(entry.Connection, entry.Channel) {
			continue
		}
		for _, tool := range entry.Channel.Tools() {
			composed, ok := ComposeToolName(scope, entry.Connection.Label, entry.Channel.Project, entry.Channel.Name, tool.Name)
			if ok && composed == exposedName {
				return entry.Connection, entry.Channel, tool.Name, true
			}
		}
	}
	return nil, nil, "", false
}

// PublishChange tells the console the live tree moved. It carries no detail:
// subscribers refetch, which is what makes a dropped event harmless (7.3).
func (r *Registry) PublishChange() {
	r.bus.Publish(events.Event{Type: events.TypeConnections})
}

// Counts returns the number of connections and a per-state server tally, for
// /api/stats and the metrics collectors.
func (r *Registry) Counts() (int, map[string]int) {
	byState := make(map[string]int)
	for _, entry := range r.IterServers() {
		state, _, _ := entry.Channel.State()
		byState[state]++
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.connections), byState
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
