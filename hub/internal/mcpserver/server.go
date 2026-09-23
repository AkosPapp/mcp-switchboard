// Package mcpserver is the consumer-facing MCP server: one aggregated view of
// every tunnelled tool, served over Streamable HTTP.
//
// This is what n8n (or any MCP client) connects to. It is served at several
// scopes, so a consumer can take everything or narrow to one machine, one
// project on that machine, or one server (spec.md H6) - and the scope decides
// how tools are named, because a consumer that only ever talks to one server
// should not have to type that server's name into every call.
//
// The catalog is live: servers appear and vanish as tunnels come and go. The
// SDK's Server, by contrast, holds a static tool set built with AddTool. So
// tools/list and tools/call are answered from the registry by a receiving
// middleware instead of being registered as features - see handle.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// The two methods this endpoint answers itself. The SDK keeps its own copies
// unexported, and hardcoding the wire strings is better than reaching for them:
// they are protocol constants, and they are never going to change.
const (
	methodListTools = "tools/list"
	methodCallTool  = "tools/call"
)

// Endpoint serves the consumer-facing scopes.
type Endpoint struct {
	registry   *registry.Registry
	dispatcher *calls.Dispatcher
	version    string
	log        *slog.Logger
	// agents backs /mcp/agent/{id}; nil when the orchestrator is disabled,
	// which makes that whole scope a 404 (spec.md E7).
	agents AgentBackend

	// One SDK server per distinct target, created on first use.
	//
	// H6 asks for a single server instance behind all scopes, which the Python
	// hub could honour because it read the scope off the live request on every
	// handler call. The Go SDK hands a request's scope to getServer and nowhere
	// else, so the scope has to be bound to something longer-lived, and the
	// server is that something. It is still one server per *scope*, not per
	// request or per consumer: sessions stay put and the session manager is
	// shared, which is the property H6 is protecting.
	mu      sync.Mutex
	servers map[string]*mcp.Server

	handler http.Handler
}

// Options configure an Endpoint. Everything but the registry and dispatcher is
// optional.
type Options struct {
	Version string
	Logger  *slog.Logger
	// Agents, when set, enables the /mcp/agent/{id} scope and its switchboard.*
	// tools (spec.md 5.5). Leave nil when AGENTS_ENABLED is false.
	Agents AgentBackend
}

// AgentBackend is what the agent scope needs from the orchestrator. The agent
// is identified by the URL alone (X8); nothing in a tool call's arguments can
// change who is calling.
type AgentBackend interface {
	// HasAgent reports whether a live agent has this id.
	HasAgent(ctx context.Context, agentID string) bool
	// AgentTools is the agent's catalog: granted registry tools composed at
	// ScopeAll plus the switchboard.* tools its capabilities allow (5.4, M1).
	AgentTools(ctx context.Context, agentID string) ([]*mcp.Tool, error)
	// CallAgentTool authorises and dispatches one call as the agent.
	CallAgentTool(ctx context.Context, agentID, name string, args map[string]any) (*mcp.CallToolResult, error)
}

// New returns an endpoint serving reg's tools, dispatching calls through d.
func New(reg *registry.Registry, d *calls.Dispatcher, opts Options) *Endpoint {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	version := opts.Version
	if version == "" {
		version = "0.0.0+dev"
	}

	endpoint := &Endpoint{
		registry:   reg,
		dispatcher: d,
		version:    version,
		log:        logger,
		agents:     opts.Agents,
		servers:    make(map[string]*mcp.Server),
	}
	streamable := mcp.NewStreamableHTTPHandler(endpoint.serverFor, &mcp.StreamableHTTPOptions{
		// A consumer that hangs up must cancel the upstream call it was
		// waiting on, rather than leaving it to run out the dispatch timeout
		// against a socket nobody is reading (spec.md G2).
		PropagateRequestCancellation: true,
	})
	endpoint.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, ok := targetForRequest(r)
		if ok && target.Scope == registry.ScopeAgent &&
			(endpoint.agents == nil || !endpoint.agents.HasAgent(r.Context(), target.AgentID)) {
			http.NotFound(w, r)
			return
		}
		if !ok {
			// Answered here rather than by returning nil from serverFor, which
			// the SDK reports as 400: a path that names no scope is a wrong
			// URL, and 404 is what tells the operator that.
			http.NotFound(w, r)
			return
		}
		streamable.ServeHTTP(w, r)
	})
	return endpoint
}

// Handler serves every scope. Mount it at both "/mcp" and "/mcp/": the scope
// comes from the full request path, so one handler covers all of them, and a
// path that names no scope is a 404.
func (e *Endpoint) Handler() http.Handler { return e.handler }

// ServeHTTP lets an Endpoint be mounted directly.
func (e *Endpoint) ServeHTTP(w http.ResponseWriter, r *http.Request) { e.handler.ServeHTTP(w, r) }

// serverFor returns the server backing this request's scope, creating it once.
func (e *Endpoint) serverFor(r *http.Request) *mcp.Server {
	target, ok := targetForRequest(r)
	if !ok || (target.Scope == registry.ScopeAgent && e.agents == nil) {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if server, ok := e.servers[target.key()]; ok {
		return server
	}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-switchboard",
		Version: e.version,
	}, &mcp.ServerOptions{
		// Declared by hand because the SDK infers capabilities from registered
		// features and this server has none - every tool it serves is answered
		// by the middleware below. listChanged in particular must be on: the
		// whole tool list changes as tunnels come and go.
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{ListChanged: true},
		},
		Logger: e.log,
	})
	server.AddReceivingMiddleware(e.handle(target))
	e.servers[target.key()] = server
	return server
}

// handle answers tools/list and tools/call from the live registry, and lets
// everything else (initialize, ping, …) fall through to the SDK.
func (e *Endpoint) handle(target Target) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			switch method {
			case methodListTools:
				if target.Scope == registry.ScopeAgent {
					tools, err := e.agents.AgentTools(ctx, target.AgentID)
					if err != nil {
						return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()}
					}
					return &mcp.ListToolsResult{Tools: tools}, nil
				}
				return e.listTools(target), nil
			case methodCallTool:
				params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
				if !ok {
					return next(ctx, method, req)
				}
				return e.callTool(ctx, target, params)
			default:
				return next(ctx, method, req)
			}
		}
	}
}

// listTools walks the registry and composes the name each tool is exposed under
// at this scope.
//
// Pagination is not implemented, as in the Python hub: the catalog is one
// machine's worth of tools held in memory, and a cursor would have to be stable
// across a list that changes whenever a tunnel moves.
func (e *Endpoint) listTools(target Target) *mcp.ListToolsResult {
	tools := make([]*mcp.Tool, 0)
	// exposed name -> the server it came from, for the duplicate warning.
	seen := make(map[string]string)

	for _, entry := range e.registry.IterServers() {
		connection, channel := entry.Connection, entry.Channel
		if !target.matches(connection, channel) || !channel.Ready() {
			continue
		}

		for _, tool := range channel.Tools() {
			exposed, ok := registry.ComposeToolName(target.Scope, connection.Label, channel.Project, channel.Name, tool.Name)
			if !ok {
				// ComposeToolName already warned about why. Dropping one tool
				// beats failing the whole listing (spec.md I4).
				continue
			}

			origin := channel.Ref().String()
			if previous, clash := seen[exposed]; clash {
				// Two tools with one name in a list is undefined behaviour -
				// the consumer just picks one. Drop the later one loudly.
				e.log.Warn("hiding a tool whose exposed name is already taken",
					"tool", exposed, "server", origin, "taken_by", previous,
					"hint", "use a per-host endpoint (/mcp/host/<label>) to disambiguate")
				continue
			}
			seen[exposed] = origin

			tools = append(tools, &mcp.Tool{
				Name:        exposed,
				Title:       title(tool.Name, connection.Label, channel.Project, channel.Name),
				Description: describe(tool.Description, connection.Label, channel.Project, channel.Name),
				InputSchema: inputSchema(tool.InputSchema),
				Meta:        toolMeta(target.Scope, connection, channel, tool.Name),
			})
		}
	}

	// Sorted so a consumer that diffs two listings sees only real changes, not
	// the map iteration order underneath IterServers.
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return &mcp.ListToolsResult{Tools: tools}
}

// callTool resolves an exposed name back to a server and dispatches it.
//
// Resolution happens at the same scope the name was listed in, so a name that
// only exists at /mcp cannot be called at /mcp/host/x (H6).
func (e *Endpoint) callTool(ctx context.Context, target Target, params *mcp.CallToolParamsRaw) (*mcp.CallToolResult, error) {
	arguments, err := decodeArguments(params.Arguments)
	if err != nil {
		return nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInvalidParams,
			Message: fmt.Sprintf("arguments for %q are not a JSON object: %v", params.Name, err),
		}
	}
	if target.Scope == registry.ScopeAgent {
		// Identity is the URL's, never the arguments' (X8).
		return e.agents.CallAgentTool(ctx, target.AgentID, params.Name, arguments)
	}

	connection, channel, upstream, ok := e.registry.ResolveTool(target.Scope, params.Name, target.filter())
	if !ok {
		// An unknown name is a protocol error, not a tool that failed: the
		// consumer asked for something this endpoint never offered, and N1's
		// distinction only applies to tools that actually ran.
		return nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInvalidParams,
			Message: fmt.Sprintf("no tool named %q is connected", params.Name),
		}
	}

	result, err := e.dispatcher.Call(ctx, calls.Request{
		Connection:  connection,
		Channel:     channel,
		Tool:        upstream,
		ExposedName: params.Name,
		Arguments:   arguments,
		Source:      store.SourceMCP,
	})
	if err != nil {
		// The server went away between the listing and the call. Like an
		// unknown name, nothing ran, so it is an error rather than a result.
		return nil, &jsonrpc.Error{
			Code:    jsonrpc.CodeInternalError,
			Message: fmt.Sprintf("%s: %v", channel.Ref(), err),
		}
	}

	// A tool that answered with an error is a successful call that returned an
	// error (spec.md N1): it comes back as a result with IsError set, so the
	// model can see what went wrong and adapt, instead of the consumer seeing
	// a transport failure.
	if result.Result != nil {
		return result.Result, nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: result.Error}},
		IsError: result.IsError,
	}, nil
}

// ToolsChanged tells every live consumer to re-list, and is what makes a
// dynamic catalog visible to a session that is already open.
//
// The SDK only emits that notification from its own feature mutations, so this
// adds and removes a tool that nothing can ever see: tools/list is answered by
// the middleware and never consults the server's feature set. A round trip
// through the public API is worth more than a copy of the notification
// plumbing that would then have to track the SDK's debouncing.
func (e *Endpoint) ToolsChanged() {
	e.mu.Lock()
	servers := make([]*mcp.Server, 0, len(e.servers))
	for _, server := range e.servers {
		servers = append(servers, server)
	}
	e.mu.Unlock()

	for _, server := range servers {
		server.AddTool(&mcp.Tool{Name: catalogChangedTool, InputSchema: emptySchema()}, nil)
		server.RemoveTools(catalogChangedTool)
	}
}

// catalogChangedTool is never listed and never callable; see ToolsChanged.
const catalogChangedTool = "switchboard.internal.catalog_changed"

// toolMeta is the origin block of spec.md H5, which is how a consumer that
// reads _meta (rather than parsing the name) finds out where a tool lives.
func toolMeta(scope registry.Scope, connection *registry.Connection, channel *registry.ServerChannel, upstream string) mcp.Meta {
	meta := mcp.Meta{
		"host":         connection.Label,
		"project":      channel.Project,
		"server":       channel.Name,
		"connectionId": connection.ID,
		"upstreamName": upstream,
	}
	if scope == registry.ScopeAgent {
		// The agent-facing catalog carries only the stable fields (H5a): a
		// connection id is regenerated on every reconnect (I2), and a tool
		// definition that changes on reconnect invalidates the provider's
		// prompt cache for no reason.
		delete(meta, "connectionId")
	}
	return meta
}

func title(tool, label, project, server string) string {
	where := server
	if project != "" {
		where = project + "/" + server
	}
	return fmt.Sprintf("%s · %s @ %s", tool, where, label)
}

// describe tags the description with the tool's origin.
//
// This is the field an LLM actually reads when choosing a tool - including our
// own agents - so the origin belongs here and not only in the name (H5).
func describe(description, label, project, server string) string {
	where := label + " · " + server
	if project != "" {
		where = label + " · " + project + " · " + server
	}
	prefix := "[" + where + "]"
	if description == "" {
		return prefix
	}
	return prefix + " " + description
}

// inputSchema passes the upstream schema through untouched. A server that
// declared none still needs a valid one, or a strict consumer rejects the tool.
func inputSchema(schema map[string]any) any {
	if len(schema) == 0 {
		return emptySchema()
	}
	return schema
}

func emptySchema() map[string]any {
	return map[string]any{"type": "object"}
}

// decodeArguments turns the raw arguments into the map the dispatcher logs and
// forwards. Absent arguments are an empty object, not an error.
func decodeArguments(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]any{}, nil
	}
	var arguments map[string]any
	if err := json.Unmarshal(raw, &arguments); err != nil {
		return nil, err
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	return arguments, nil
}
