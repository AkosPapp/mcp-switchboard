package agents

import (
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// builtinServer is the first-party harness server (spec.md §9). Its tools are
// the ones an agent uses constantly, so a single-client agent sees them bare.
const builtinServer = "harness"

// soleClient reports the one client an agent is pinned to: every allowed grant
// names the same concrete label. An agent created from a chat is always like
// this (A16); one made through the API with wildcard or multi-client grants is
// not, and keeps the fully qualified names.
//
// The answer depends only on the agent's stored grants, never on what is
// connected, so an agent's tool names do not change when an unrelated machine
// dials in (T1, Q5).
func soleClient(grants []store.Grant) (string, bool) {
	label := ""
	for _, g := range grants {
		if !g.Allowed {
			continue
		}
		if g.Label == registry.Wildcard || g.Label == "" {
			return "", false
		}
		if label == "" {
			label = g.Label
		} else if label != g.Label {
			return "", false
		}
	}
	return label, label != ""
}

// agentToolName composes the name an agent sees for a client-server tool.
//
//   - Pinned to one client: the client label is redundant, so it is dropped,
//     and the harness (the server every client carries) drops its own prefix
//     too, leaving `run_command`. Other servers keep `[project__]server__tool`
//     so tools from different servers cannot collide.
//   - Otherwise: `label__[project__]server__tool`, as at /mcp.
func agentToolName(pinned bool, label, project, server, tool string) (string, bool) {
	if !pinned {
		return registry.ComposeToolName(registry.ScopeAll, label, project, server, tool)
	}
	if server == builtinServer && project == "" {
		return registry.ComposeToolName(registry.ScopeServer, label, project, server, tool)
	}
	return registry.ComposeToolName(registry.ScopeHost, label, project, server, tool)
}

// resolveAgentTool maps a name the agent used back to a live server, by
// re-composing candidate names (never a cached map, so a reconnected client can
// not be resolved through a stale entry). When the agent is pinned, only that
// client's servers are candidates.
func (m *Manager) resolveAgentTool(grants []store.Grant, name string) (*registry.Connection, *registry.ServerChannel, string, bool) {
	client, pinned := soleClient(grants)
	for _, entry := range m.reg.IterServers() {
		if pinned && entry.Connection.Label != client {
			continue
		}
		for _, tool := range entry.Channel.Tools() {
			composed, ok := agentToolName(pinned, entry.Connection.Label, entry.Channel.Project, entry.Channel.Name, tool.Name)
			if ok && composed == name {
				return entry.Connection, entry.Channel, tool.Name, true
			}
		}
	}
	return nil, nil, "", false
}
