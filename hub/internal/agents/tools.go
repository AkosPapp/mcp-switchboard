package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/push"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// catalogTool is one tool an agent may see this turn.
type catalogTool struct {
	Name     string // composed by agentToolName, or switchboard.*; what MCP and storage use
	Provider string // providerName(Name): what the model sees
	Desc     string
	Schema   map[string]any
	Ann      *registry.ToolAnnotations
	AnnMCP   *mcp.ToolAnnotations
	Ref      registry.ServerRef // zero for switchboard tools
	Upstream string
	Meta     mcp.Meta
	sb       *sbTool
}

// catalog is the per-turn tool set (T1): stable order, stable names.
type catalog struct {
	tools  []*catalogTool
	byName map[string]*catalogTool
	byProv map[string]*catalogTool
}

func (c *catalog) llmTools() []llm.Tool {
	out := make([]llm.Tool, 0, len(c.tools))
	for _, t := range c.tools {
		out = append(out, llm.Tool{Name: t.Provider, Description: t.Desc, InputSchema: t.Schema})
	}
	return out
}

func agentRules(grants []store.Grant) []Rule {
	rules := make([]Rule, 0, len(grants))
	for _, g := range grants {
		rules = append(rules, Rule{Ref: registry.ServerRef{Label: g.Label, Project: g.Project, Server: g.Server}, Allowed: g.Allowed})
	}
	return rules
}

// buildCatalog is 5.4: live (connection, channel) pairs, kept when a grant
// allows them, named by agentToolName (client prefix dropped when the agent is
// pinned to one client), plus the switchboard.* tools the agent's
// capabilities allow (M1), sorted by name; names invalid or duplicated are
// dropped loudly.
func (m *Manager) buildCatalog(ctx context.Context, agent *store.Agent) (*catalog, error) {
	grants, err := m.st.ListGrants(ctx, agent.ID)
	if err != nil {
		return nil, err
	}
	rules := agentRules(grants)
	_, pinned := soleClient(grants)
	cat := &catalog{byName: map[string]*catalogTool{}, byProv: map[string]*catalogTool{}}
	add := func(t *catalogTool) {
		t.Provider = providerName(t.Name)
		if prev, dup := cat.byProv[t.Provider]; dup {
			m.log.Warn("dropping a duplicate tool name from an agent's catalog", "tool", t.Name, "clashes_with", prev.Name, "agent", agent.ID)
			return
		}
		cat.tools = append(cat.tools, t)
		cat.byName[t.Name] = t
		cat.byProv[t.Provider] = t
	}
	for _, entry := range m.reg.IterServers() {
		conn, ch := entry.Connection, entry.Channel
		if !ch.Ready() {
			continue
		}
		ref := ch.Ref()
		if ok, _ := Resolve(rules, ref); !ok {
			continue
		}
		for _, tool := range ch.Tools() {
			name, ok := agentToolName(pinned, conn.Label, ch.Project, ch.Name, tool.Name)
			if !ok {
				continue
			}
			schema := tool.InputSchema
			if len(schema) == 0 {
				schema = map[string]any{"type": "object"}
			}
			add(&catalogTool{
				Name: name, Desc: describeOrigin(tool.Description, conn.Label, ch.Project, ch.Name), Schema: schema,
				Ann: tool.Annotations, AnnMCP: mcpAnnotations(tool.Annotations), Ref: ref, Upstream: tool.Name,
				// H5a: only the stable origin fields; a connection id would change on reconnect.
				Meta: mcp.Meta{"host": conn.Label, "project": ch.Project, "server": ch.Name, "upstreamName": tool.Name},
			})
		}
	}
	for _, sb := range sbTools {
		if sb.visible(agent.Capabilities) {
			add(&catalogTool{Name: sb.name, Desc: sb.desc, Schema: sb.schema, Ann: regAnnotations(sb.ann), AnnMCP: sb.ann, sb: sb})
		}
	}
	if len(m.autoSkills(ctx)) > 0 {
		t := skillLoadTool
		add(&catalogTool{Name: t.name, Desc: t.desc, Schema: t.schema, Ann: regAnnotations(t.ann), AnnMCP: t.ann, sb: t})
	}
	sort.Slice(cat.tools, func(i, j int) bool { return cat.tools[i].Name < cat.tools[j].Name })
	return cat, nil
}

func describeOrigin(desc, label, project, server string) string {
	where := label + " · " + server
	if project != "" {
		where = label + " · " + project + " · " + server
	}
	if desc == "" {
		return "[" + where + "]"
	}
	return "[" + where + "] " + desc
}

func mcpAnnotations(a *registry.ToolAnnotations) *mcp.ToolAnnotations {
	if a == nil {
		return nil
	}
	return &mcp.ToolAnnotations{ReadOnlyHint: a.ReadOnly, DestructiveHint: a.Destructive, OpenWorldHint: a.OpenWorld}
}

func regAnnotations(a *mcp.ToolAnnotations) *registry.ToolAnnotations {
	if a == nil {
		return nil
	}
	return &registry.ToolAnnotations{ReadOnly: a.ReadOnlyHint, Destructive: a.DestructiveHint, OpenWorld: a.OpenWorldHint}
}

// ---------------------------------------------------------------- tool outcomes

// toolOutcome is what one tool call yielded, in the shapes the transcript needs.
type toolOutcome struct {
	CallID  string
	Result  map[string]any // MCP result as a map
	Blocks  []llm.Block
	IsError bool
}

func (o toolOutcome) Text() string { return blocksText(o.Blocks) }

func toolErr(msg string) toolOutcome {
	return toolOutcome{IsError: true, Blocks: textBlocks(msg),
		Result: map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": msg}}}}
}

func outcomeFromResult(r *calls.Result) toolOutcome {
	out := toolOutcome{CallID: r.CallID, IsError: r.IsError || r.Error != ""}
	if r.Result != nil {
		if raw, err := json.Marshal(r.Result); err == nil {
			_ = json.Unmarshal(raw, &out.Result)
		}
	}
	out.Blocks = blocksFromResult(out.Result)
	if len(out.Blocks) == 0 && r.Error != "" {
		out.Blocks = textBlocks(r.Error)
		out.Result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": r.Error}}}
	}
	return out
}

func (o toolOutcome) mcpResult() *mcp.CallToolResult {
	res := &mcp.CallToolResult{IsError: o.IsError}
	for _, b := range o.Blocks {
		switch b.Type {
		case llm.BlockImage:
			res.Content = append(res.Content, &mcp.ImageContent{Data: []byte(b.Data), MIMEType: b.MediaType})
		default:
			res.Content = append(res.Content, &mcp.TextContent{Text: b.Text})
		}
	}
	return res
}

// ---------------------------------------------------------------- dispatching

func (rs *runState) ids(agent *store.Agent) (a, c, r *string) {
	a = &agent.ID
	if rs != nil {
		c, r = &rs.chatID, &rs.id
	}
	return
}

// execTool authorises and performs one tool call as agent (R1, R2, W1). rs is
// nil for a call made through /mcp/agent/{id} with no run behind it.
func (m *Manager) execTool(ctx context.Context, agent *store.Agent, rs *runState, name string, args map[string]any, toolCallID string) toolOutcome {
	if args == nil {
		args = map[string]any{}
	}
	aid, cid, rid := rs.ids(agent)

	if strings.HasPrefix(name, "switchboard.") {
		sb := sbByName[name]
		if name == skillLoadName {
			sb = skillLoadTool
		}
		if sb == nil || !sb.visible(agent.Capabilities) {
			return toolErr(fmt.Sprintf("unknown tool %q", name))
		}
		req := calls.LocalRequest{Label: "switchboard", Server: "orchestrator", Tool: name, ExposedName: name,
			Arguments: args, Source: store.SourceAgent, AgentID: aid, ChatID: cid, RunID: rid}
		var gateErr error
		if ok, reason := m.gate(ctx, agent, rs, toolCallID, name, args, regAnnotations(sb.ann)); !ok {
			gateErr = fmt.Errorf("%w: %s", calls.ErrDenied, reason)
		}
		res := m.disp.Local(ctx, req, func(ctx context.Context) (any, error) {
			if gateErr != nil {
				return nil, gateErr
			}
			return sb.run(m, ctx, &callCtx{agent: agent, rs: rs, callID: toolCallID}, args)
		})
		return outcomeFromResult(res)
	}

	// The agent's own stored grants are authoritative at call time (A12), and
	// they also decide how its tool names are composed (soleClient).
	grants, err := m.st.ListGrants(ctx, agent.ID)
	if err != nil {
		return toolErr("could not read grants: " + err.Error())
	}
	conn, ch, upstream, ok := m.resolveAgentTool(grants, name)
	if !ok {
		return toolErr(fmt.Sprintf("unknown tool %q: it is not offered by any connected server", name))
	}
	ref := ch.Ref()
	req := calls.Request{Connection: conn, Channel: ch, Tool: upstream, ExposedName: name, Arguments: args,
		Source: store.SourceAgent, AgentID: aid, ChatID: cid, RunID: rid}
	if allowed, _ := Resolve(agentRules(grants), ref); !allowed {
		return outcomeFromResult(m.disp.Deny(ctx, req, "not permitted: "+ref.String()))
	}
	var ann *registry.ToolAnnotations
	for _, t := range ch.Tools() {
		if t.Name == upstream {
			ann = t.Annotations
		}
	}
	if ok, reason := m.gate(ctx, agent, rs, toolCallID, name, args, ann); !ok {
		return outcomeFromResult(m.disp.Deny(ctx, req, reason))
	}
	res, err := m.disp.Call(ctx, req)
	if err != nil {
		if errors.Is(err, calls.ErrNotConnected) {
			// A15: a normal tool error, distinct from a permission error.
			return toolErr(fmt.Sprintf("server not connected: %s", ref))
		}
		return toolErr(err.Error())
	}
	return outcomeFromResult(res)
}

// ------------------------------------------------------------------ approvals

type approvalDecision struct {
	approved bool
	reason   string
	answers  []QuestionAnswer // set when a question was answered (questions.go)
}

type pendingApproval struct {
	tool      string
	arguments map[string]any
	expiresAt time.Time
	createdAt time.Time
	ch        chan approvalDecision
	// questions is set when this entry is switchboard.user.ask waiting for the
	// user rather than a call waiting for approval; it rides the same plumbing
	// (pending list, stream frame, waiting run status) and differs in the reply.
	questions []Question
}

// needsApproval is W1: never / always, or for "destructive" any tool whose
// annotations say destructiveHint or openWorldHint true. Annotations are
// advisory data from the upstream (W4): a missing annotation does not pause.
func needsApproval(agent *store.Agent, ann *registry.ToolAnnotations) bool {
	switch agent.Approval {
	case store.ApprovalAlways:
		return true
	case store.ApprovalDestructive:
		return ann != nil && ((ann.Destructive != nil && *ann.Destructive) || (ann.OpenWorld != nil && *ann.OpenWorld))
	}
	return false
}

// gate runs the approval step of 5.3/5.8. It returns ok=false with the reason
// to give the model when the call must not proceed.
func (m *Manager) gate(ctx context.Context, agent *store.Agent, rs *runState, callID, tool string, args map[string]any, ann *registry.ToolAnnotations) (bool, string) {
	if !needsApproval(agent, ann) {
		return true, ""
	}
	if rs == nil {
		return false, "approval is required for this call but /mcp/agent has no run to pause; use the console"
	}
	timeout := time.Duration(m.set.ApprovalTimeout) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	now := time.Now().UTC()
	pa := &pendingApproval{tool: tool, arguments: args, createdAt: now, expiresAt: now.Add(timeout), ch: make(chan approvalDecision, 1)}
	m.mu.Lock()
	rs.approvals[callID] = pa
	m.mu.Unlock()

	m.enterWait(rs, true)
	m.frame(rs, "approval_required", map[string]any{"callId": callID, "name": tool, "arguments": args, "expiresAt": store.FormatTime(pa.expiresAt)})
	m.publish(eventChat(rs))
	m.publish(events.Event{Type: events.TypeAgent, AgentID: rs.agentID}) // /api/approvals changed
	m.notifyPush(rs, push.KindApprovalRequired, fmt.Sprintf("wants to run %s", tool))
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	var dec approvalDecision
	outcome := "denied"
	select {
	case dec = <-pa.ch:
		if dec.approved {
			outcome = "approved"
		}
	case <-timer.C:
		dec = approvalDecision{reason: fmt.Sprintf("approval timed out after %s: the call was auto-denied", timeout.Round(time.Second))}
		outcome = "timeout"
	case <-ctx.Done():
		dec = approvalDecision{reason: "cancelled while waiting for approval"}
		outcome = ""
	}
	m.mu.Lock()
	delete(rs.approvals, callID)
	m.mu.Unlock()
	m.publish(events.Event{Type: events.TypeAgent, AgentID: rs.agentID}) // /api/approvals changed
	m.leaveWait(rs, true)
	if outcome != "" {
		if m.met != nil {
			m.met.CountApproval(outcome)
		}
		m.emit("approval", map[string]any{"agentId": agent.ID, "runId": rs.id, "tool": tool, "outcome": outcome})
	}
	if dec.approved {
		return true, ""
	}
	if outcome == "denied" {
		r := "denied by the approver"
		if dec.reason != "" {
			r += ": " + dec.reason
		}
		return false, r
	}
	return false, dec.reason
}

// Approve answers a pending approval (W2).
func (m *Manager) Approve(_ context.Context, runID, callID string, approved bool, reason string) error {
	m.mu.Lock()
	rs := m.runs[runID]
	var pa *pendingApproval
	if rs != nil {
		pa = rs.approvals[callID]
	}
	m.mu.Unlock()
	if pa == nil {
		return ErrNotPending
	}
	select {
	case pa.ch <- approvalDecision{approved: approved, reason: reason}:
		return nil
	default:
		return ErrNotPending // already answered
	}
}

// PendingApprovals lists what a run is blocked on.
func (m *Manager) PendingApprovals(runID string) []PendingApproval {
	m.mu.Lock()
	defer m.mu.Unlock()
	rs := m.runs[runID]
	if rs == nil {
		return []PendingApproval{}
	}
	out := make([]PendingApproval, 0, len(rs.approvals))
	for id, pa := range rs.approvals {
		out = append(out, pa.view(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CallID < out[j].CallID })
	return out
}

// AllPendingApprovals lists every approval pending in any run, oldest first,
// with the agent name and chat title joined from the store.
func (m *Manager) AllPendingApprovals(ctx context.Context) ([]PendingApprovalDetail, error) {
	type row struct {
		d PendingApprovalDetail
		t time.Time
	}
	var rows []row
	m.mu.Lock()
	for _, rs := range m.runs {
		for id, pa := range rs.approvals {
			rows = append(rows, row{PendingApprovalDetail{
				RunID: rs.id, ChatID: rs.chatID, AgentID: rs.agentID,
				PendingApproval: pa.view(id),
			}, pa.createdAt})
		}
	}
	m.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].t.Equal(rows[j].t) {
			return rows[i].t.Before(rows[j].t)
		}
		return rows[i].d.CallID < rows[j].d.CallID
	})
	out := make([]PendingApprovalDetail, 0, len(rows))
	for _, r := range rows {
		if a, err := m.st.GetAgent(ctx, r.d.AgentID); err == nil && a != nil {
			r.d.AgentName = a.Name
		}
		if r.d.ChatID != "" {
			if c, err := m.st.GetChat(ctx, r.d.ChatID); err == nil && c != nil {
				r.d.ChatTitle = c.Title
			}
		}
		out = append(out, r.d)
	}
	return out, nil
}

// ------------------------------------------------------- /mcp/agent/{id} backend

// HasAgent implements mcpserver.AgentBackend.
func (m *Manager) HasAgent(ctx context.Context, agentID string) bool {
	a, err := m.st.GetAgent(ctx, agentID)
	return err == nil && a != nil && a.DeletedAt == nil
}

// AgentTools implements mcpserver.AgentBackend: the agent's catalog as MCP tools.
func (m *Manager) AgentTools(ctx context.Context, agentID string) ([]*mcp.Tool, error) {
	agent, err := m.st.GetAgent(ctx, agentID)
	if err != nil || agent == nil || agent.DeletedAt != nil {
		return nil, fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	agent, _ = m.resolveAgent(ctx, agent)
	cat, err := m.buildCatalog(ctx, agent)
	if err != nil {
		return nil, err
	}
	out := make([]*mcp.Tool, 0, len(cat.tools))
	for _, t := range cat.tools {
		out = append(out, &mcp.Tool{Name: t.Name, Description: t.Desc, InputSchema: t.Schema, Annotations: t.AnnMCP, Meta: t.Meta})
	}
	return out, nil
}

// CallAgentTool implements mcpserver.AgentBackend. The agent is the one the
// URL named (X8); there is no run, so approvals cannot be asked for.
func (m *Manager) CallAgentTool(ctx context.Context, agentID, name string, args map[string]any) (*mcp.CallToolResult, error) {
	agent, err := m.st.GetAgent(ctx, agentID)
	if err != nil || agent == nil || agent.DeletedAt != nil {
		return nil, fmt.Errorf("%w: agent %s", ErrNotFound, agentID)
	}
	agent, _ = m.resolveAgent(ctx, agent)
	return m.execTool(ctx, agent, nil, name, args, "").mcpResult(), nil
}
