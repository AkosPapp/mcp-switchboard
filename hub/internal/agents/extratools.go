package agents

// Optional hub tools that live outside sbTools: each is offered to every chat
// (like skill.load, no capability needed) but only while its enabled predicate
// holds - typically a setting being present. A tool file registers itself from
// init():
//
//	func init() { registerExtraTool(webSearchTool, func(m *Manager) bool { return m.set.SearxngURL != "" }) }
//
// buildCatalog, execTool and HubTools read this one list, so adding a tool
// never means touching those functions.

type extraTool struct {
	tool    *sbTool
	enabled func(*Manager) bool // nil means always
}

var extraSbTools []extraTool

func registerExtraTool(t *sbTool, enabled func(*Manager) bool) {
	extraSbTools = append(extraSbTools, extraTool{tool: t, enabled: enabled})
}

// extraTools is every extra tool currently enabled, in registration order.
func (m *Manager) extraTools() []*sbTool {
	out := make([]*sbTool, 0, len(extraSbTools))
	for _, e := range extraSbTools {
		if e.enabled == nil || e.enabled(m) {
			out = append(out, e.tool)
		}
	}
	return out
}

// extraToolByName is the enabled extra tool with this name, or nil.
func (m *Manager) extraToolByName(name string) *sbTool {
	for _, t := range m.extraTools() {
		if t.name == name {
			return t
		}
	}
	return nil
}
