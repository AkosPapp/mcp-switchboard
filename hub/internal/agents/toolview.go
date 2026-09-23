package agents

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func annView(a *mcp.ToolAnnotations) ToolAnnotations {
	var v ToolAnnotations
	if a == nil {
		return v
	}
	t := true
	if a.ReadOnlyHint {
		v.ReadOnlyHint = &t
	}
	if a.IdempotentHint {
		v.IdempotentHint = &t
	}
	v.DestructiveHint, v.OpenWorldHint = a.DestructiveHint, a.OpenWorldHint
	return v
}

// requiresOf derives M1's visibility rule from the tool's own predicate, so
// the API cannot disagree with what the catalog does.
func requiresOf(t *sbTool) string {
	spawn := t.visible(store.Capabilities{CanSpawn: true})
	msg := t.visible(store.Capabilities{CanMessage: true})
	switch {
	case t.visible(store.Capabilities{}):
		return "always"
	case spawn && msg:
		return "canSpawn or canMessage"
	case spawn:
		return "canSpawn"
	default:
		return "canMessage"
	}
}

// HubTools implements Service: the switchboard.* definitions the run loop offers.
func (m *Manager) HubTools() []HubTool {
	out := make([]HubTool, 0, len(sbTools))
	for _, t := range sbTools {
		out = append(out, HubTool{Name: t.name, Description: t.desc, InputSchema: t.schema, Annotations: annView(t.ann), Requires: requiresOf(t)})
	}
	return out
}

// ChatTools implements Service. It runs buildCatalog, the function the run
// loop calls each turn (5.4), so the two cannot drift. Names are the real
// exposed names; the provider-safe mapping stays internal.
func (m *Manager) ChatTools(ctx context.Context, chatID string) (*ChatToolsView, error) {
	chat, agent, err := m.liveAgentForChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	eff, prof := m.resolveAgent(ctx, agent)
	plan, err := m.planTurn(ctx, eff, prof)
	if err != nil {
		return nil, err
	}
	cat := plan.cat
	view := &ChatToolsView{ClientLabel: chat.ClientLabel, Tools: make([]ChatTool, 0, len(cat.tools))}
	if chat.ClientLabel != nil {
		for _, c := range m.reg.Connections() {
			if c.Label == *chat.ClientLabel {
				view.ClientConnected = true
				break
			}
		}
	}
	for _, t := range cat.tools {
		ann := annView(t.AnnMCP)
		ct := ChatTool{Name: t.Name, Description: t.Desc, Origin: "hub", InputSchema: t.Schema, Annotations: &ann}
		if t.sb == nil {
			ct.Origin = "mcp"
			ct.Server = &ToolServer{Label: t.Ref.Label, Project: t.Ref.Project, Server: t.Ref.Server}
		}
		view.Tools = append(view.Tools, ct)
	}
	return view, nil
}
