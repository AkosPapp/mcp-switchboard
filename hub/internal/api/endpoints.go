package api

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
)

// The console's "Endpoints" panel: the /mcp URL reference table and the
// copyable client-install command (spec.md H7).
//
// Kept as a pure function of (Settings, a registry snapshot) rather than a
// method on the handler, so it is trivial to test with a snapshot literal and
// no live hub.

// InstallScriptURL is where the client install one-liner fetches its script.
const InstallScriptURL = "https://akospapp.github.io/mcp-switchboard/install.sh"

// EndpointRow is one reachable scope: the path to point a client at, and an
// example of what a tool is called there. The example is what makes the table
// useful - the path alone does not tell anyone how names change per scope.
type EndpointRow struct {
	Path        string `json:"path"`
	Scope       string `json:"scope"`
	Description string `json:"description"`
	Example     string `json:"example"`
}

// EndpointsInfo is the body of GET /api/endpoints. PublicURL and InstallCommand
// are null rather than empty when unset, because "not configured" and
// "configured to nothing" are the same thing to the panel and null is what the
// console already tests for.
type EndpointsInfo struct {
	LocalBaseURL   string        `json:"localBaseUrl"`
	PublicURL      *string       `json:"publicUrl"`
	InstallCommand *string       `json:"installCommand"`
	Rows           []EndpointRow `json:"rows"`
}

// placeholderExample is the example tool name shown when nothing real is
// connected yet.
func placeholderExample(scope registry.Scope) string {
	switch scope {
	case registry.ScopeHost:
		return "git__git_status"
	case registry.ScopeProject:
		return "server__tool"
	case registry.ScopeServer:
		return "git_status"
	case registry.ScopeProjectServer:
		return "tool"
	default:
		return "legion5__git__git_status"
	}
}

// firstTool returns a server's first tool, or nil when it has none - a server
// that is starting, or failed, still gets a row and falls back to a placeholder.
func firstTool(server registry.ServerJSON) *registry.ToolJSON {
	if len(server.Tools) == 0 {
		return nil
	}
	return &server.Tools[0]
}

// BuildEndpointRows returns one row per meaningful scope, using real connected
// tools as examples where possible and falling back to a placeholder otherwise.
func BuildEndpointRows(snapshot registry.SnapshotJSON) []EndpointRow {
	exampleAll := ""
	for _, connection := range snapshot.Connections {
		for _, server := range connection.Servers {
			if tool := firstTool(server); tool != nil && tool.ExposedName != nil && *tool.ExposedName != "" {
				exampleAll = *tool.ExposedName
				break
			}
		}
		if exampleAll != "" {
			break
		}
	}
	if exampleAll == "" {
		exampleAll = placeholderExample(registry.ScopeAll)
	}

	rows := []EndpointRow{{
		Path:        "/mcp",
		Scope:       string(registry.ScopeAll),
		Description: "everything, every machine",
		Example:     exampleAll,
	}}

	if len(snapshot.Connections) == 0 {
		rows = append(rows, EndpointRow{
			Path:        "/mcp/host/<host>",
			Scope:       string(registry.ScopeHost),
			Description: "everything on one machine",
			Example:     placeholderExample(registry.ScopeHost),
		})
		return rows
	}

	seenHosts := map[string]bool{}
	seenProjects := map[[2]string]bool{}

	for _, connection := range snapshot.Connections {
		label := connection.Label
		servers := connection.Servers

		if !seenHosts[label] {
			seenHosts[label] = true
			example := placeholderExample(registry.ScopeHost)
			for _, server := range servers {
				if tool := firstTool(server); tool != nil {
					// The host scope drops the label from the front of the
					// name, which is exactly what stripping one separator does.
					example = lastAfterSeparator(tool)
					break
				}
			}
			rows = append(rows, EndpointRow{
				Path:        "/mcp/host/" + label,
				Scope:       string(registry.ScopeHost),
				Description: "everything on " + label,
				Example:     example,
			})
		}

		for _, project := range projectsOf(servers) {
			key := [2]string{label, project}
			if seenProjects[key] {
				continue
			}
			seenProjects[key] = true
			example := placeholderExample(registry.ScopeProject)
			for _, server := range servers {
				if projectOf(server) != project {
					continue
				}
				if tool := firstTool(server); tool != nil {
					example = server.Name + protocol.NameSeparator + tool.Name
					break
				}
			}
			rows = append(rows, EndpointRow{
				Path:        "/mcp/host/" + label + "/project/" + project,
				Scope:       string(registry.ScopeProject),
				Description: fmt.Sprintf("the '%s' project on %s", project, label),
				Example:     example,
			})
		}

		for _, server := range servers {
			example := placeholderExample(registry.ScopeServer)
			if tool := firstTool(server); tool != nil {
				example = tool.Name
			}
			if project := projectOf(server); project != "" {
				rows = append(rows, EndpointRow{
					Path:        "/mcp/host/" + label + "/project/" + project + "/server/" + server.Name,
					Scope:       string(registry.ScopeProjectServer),
					Description: fmt.Sprintf("just %s (in %s on %s)", server.Name, project, label),
					Example:     example,
				})
				continue
			}
			rows = append(rows, EndpointRow{
				Path:        "/mcp/host/" + label + "/server/" + server.Name,
				Scope:       string(registry.ScopeServer),
				Description: fmt.Sprintf("just %s on %s", server.Name, label),
				Example:     example,
			})
		}
	}

	return rows
}

// lastAfterSeparator strips the label component off an exposed name. It reads
// the composed name rather than rebuilding one, so the host-scope example can
// never disagree with what /mcp/host/{label} actually lists.
func lastAfterSeparator(tool *registry.ToolJSON) string {
	if tool.ExposedName == nil {
		return tool.Name
	}
	_, rest, found := strings.Cut(*tool.ExposedName, protocol.NameSeparator)
	if !found {
		return *tool.ExposedName
	}
	return rest
}

// projectOf reads a server's project, which is a pointer because the JSON
// contract renders "no project" as null rather than as an empty string.
func projectOf(server registry.ServerJSON) string {
	if server.Project == nil {
		return ""
	}
	return *server.Project
}

// projectsOf returns the distinct, sorted projects a connection's servers
// declare. Sorted because the panel is read top to bottom and a table that
// reshuffles between polls is unreadable.
func projectsOf(servers []registry.ServerJSON) []string {
	seen := map[string]bool{}
	var out []string
	for _, server := range servers {
		project := projectOf(server)
		if project == "" || seen[project] {
			continue
		}
		seen[project] = true
		out = append(out, project)
	}
	sort.Strings(out)
	return out
}

// BuildInstallCommand returns the client install one-liner, or nil when
// MCP_SWITCHBOARD_PUBLIC_URL is unset - there is no way to derive the tunnel's
// externally reachable address from how the hub binds locally.
//
// It includes the real tunnel token. This only ever renders on the private
// listener, which is not meant to be exposed (spec.md 10.1): equivalent
// exposure to reading the same token out of the hub's own settings file.
func BuildInstallCommand(settings config.Settings) *string {
	if settings.PublicURL == "" {
		return nil
	}
	command := fmt.Sprintf(
		"curl -fsSL %s | sh -s -- --hub-url %s --token %s",
		InstallScriptURL, settings.PublicURL, settings.TunnelToken,
	)
	return &command
}

// BuildEndpointsInfo assembles the whole panel.
func BuildEndpointsInfo(settings config.Settings, snapshot registry.SnapshotJSON) EndpointsInfo {
	var publicURL *string
	if settings.PublicURL != "" {
		value := settings.PublicURL
		publicURL = &value
	}
	return EndpointsInfo{
		LocalBaseURL:   settings.EffectiveLocalBaseURL(),
		PublicURL:      publicURL,
		InstallCommand: BuildInstallCommand(settings),
		Rows:           BuildEndpointRows(snapshot),
	}
}
