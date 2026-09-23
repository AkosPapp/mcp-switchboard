package mcpserver

import (
	"net/http"
	"strings"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
)

// A Target is one endpoint's view of the catalog: the scope it names tools in,
// plus the path parameters that narrow it.
//
// It is a value, not a pointer, because it is derived from a URL once and then
// only read - two requests to the same path produce equal Targets, which is
// what makes it usable as a cache key.
type Target struct {
	Scope   registry.Scope
	Label   string
	Project string
	Server  string
	// AgentID is set for ScopeAgent only, and comes from the URL (spec.md X8).
	AgentID string
}

// parseTarget maps a request path to the scope it serves (spec.md H6).
//
//	/mcp                                                 all
//	/mcp/host/{label}                                    host
//	/mcp/host/{label}/project/{project}                  project
//	/mcp/host/{label}/server/{server}                    server
//	/mcp/host/{label}/project/{project}/server/{server}  project + server
//
//	/mcp/agent/{id}                                      agent (only when agents are enabled)
//
// ok is false for anything else. parseTarget is pure: whether the agent scope
// exists at all is the Endpoint's decision (E7: with the orchestrator off,
// /mcp/agent/* is a 404).
func parseTarget(path string) (Target, bool) {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) == 0 || segments[0] != "mcp" {
		return Target{}, false
	}

	rest := segments[1:]
	if len(rest) == 0 {
		return Target{Scope: registry.ScopeAll}, true
	}
	if rest[0] == "agent" {
		if len(rest) != 2 || rest[1] == "" {
			return Target{}, false
		}
		return Target{Scope: registry.ScopeAgent, AgentID: rest[1]}, true
	}
	if rest[0] != "host" || len(rest) < 2 || rest[1] == "" {
		return Target{}, false
	}

	target := Target{Scope: registry.ScopeHost, Label: rest[1]}
	rest = rest[2:]

	// The remaining segments are {keyword, value} pairs, and each one narrows
	// the scope further. Reading them as pairs rather than matching whole path
	// shapes keeps project-then-server the only legal order without a case per
	// combination.
	for len(rest) >= 2 && rest[1] != "" {
		switch rest[0] {
		case "project":
			if target.Project != "" || target.Server != "" {
				return Target{}, false
			}
			target.Project, target.Scope = rest[1], registry.ScopeProject
		case "server":
			if target.Server != "" {
				return Target{}, false
			}
			target.Server = rest[1]
			if target.Project == "" {
				target.Scope = registry.ScopeServer
			} else {
				target.Scope = registry.ScopeProjectServer
			}
		default:
			return Target{}, false
		}
		rest = rest[2:]
	}
	if len(rest) != 0 {
		return Target{}, false
	}
	return target, true
}

// filter turns the path parameters into a registry lookup, which is how
// ResolveTool is narrowed to this scope.
//
// A nil field means "don't care" while an empty string is a real value, so the
// project is only constrained when the path actually named one - a /mcp/host
// endpoint must still show servers that have no project at all.
func (t Target) filter() registry.Filter {
	filter := registry.Filter{}
	if t.Label != "" {
		filter.Label = registry.Only(t.Label)
	}
	if t.Project != "" {
		filter.Project = registry.Only(t.Project)
	}
	if t.Server != "" {
		filter.Server = registry.Only(t.Server)
	}
	return filter
}

// matches reports whether one connected server belongs in this scope. It is
// the same test filter() asks ResolveTool for, spelled out because listing
// walks the registry itself and the registry applies a Filter only internally.
func (t Target) matches(connection *registry.Connection, channel *registry.ServerChannel) bool {
	if t.Label != "" && connection.Label != t.Label {
		return false
	}
	if t.Project != "" && channel.Project != t.Project {
		return false
	}
	if t.Server != "" && channel.Name != t.Server {
		return false
	}
	return true
}

// key identifies the server instance that backs this target. The separator is
// a NUL because it cannot occur in a path segment, so two different targets can
// never collide on one key.
func (t Target) key() string {
	return strings.Join([]string{string(t.Scope), t.Label, t.Project, t.Server, t.AgentID}, "\x00")
}

func targetForRequest(r *http.Request) (Target, bool) {
	return parseTarget(r.URL.Path)
}
