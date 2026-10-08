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
func (m *Manager) buildCatalog(ctx context.Context, agent *store.Agent, chat *store.Chat) (*catalog, error) {
	grants, err := m.st.ListGrants(ctx, agent.ID)
	if err != nil {
		return nil, err
	}
	rules := agentRules(grants)
	_, pinned := soleClient(grants)
	cat := &catalog{byName: map[string]*catalogTool{}, byProv: map[string]*catalogTool{}}
	add := func(t *catalogTool) {
		t.Provider = providerName(t.Name)
		if t.sb != nil && t.sb.alias != "" {
			t.Provider = providerName(t.sb.alias)
		}
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
			if !ok || !toolAllowed(agent.ToolAllow, tool.Name) {
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
				Meta: upstreamMeta(conn, ch, tool),
			})
		}
	}
	for _, sb := range sbTools {
		if sb.shownTo(agent) {
			add(&catalogTool{Name: sb.name, Desc: sb.desc, Schema: sb.schema, Ann: regAnnotations(sb.ann), AnnMCP: sb.ann, sb: sb})
		}
	}
	if len(m.autoSkills(ctx)) > 0 {
		t := skillLoadTool
		add(&catalogTool{Name: t.name, Desc: t.desc, Schema: t.schema, Ann: regAnnotations(t.ann), AnnMCP: t.ann, sb: t})
	}
	for _, t := range m.extraTools() {
		if t.needChat != nil && !t.needChat(chat) {
			continue
		}
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
		switch {
		case name == skillLoadName:
			sb = skillLoadTool
		case m.extraToolByName(name) != nil:
			sb = m.extraToolByName(name)
		}
		if sb == nil || !sb.shownTo(agent) {
			return toolErr(fmt.Sprintf("unknown tool %q", name))
		}
		args = normalizeArgs(sb.schema, args)
		req := calls.LocalRequest{Label: "switchboard", Server: "orchestrator", Tool: name, ExposedName: name,
			Arguments: args, Source: store.SourceAgent, AgentID: aid, ChatID: cid, RunID: rid}
		if msg := validateArgs(sb.schema, args); msg != "" {
			return outcomeFromResult(m.disp.Local(ctx, req, func(context.Context) (any, error) { return nil, errors.New(msg) }))
		}
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
	var ann *registry.ToolAnnotations
	var schema map[string]any
	for _, t := range ch.Tools() {
		if t.Name == upstream {
			ann, schema = t.Annotations, t.InputSchema
		}
	}
	args = normalizeArgs(schema, args)
	req := calls.Request{Connection: conn, Channel: ch, Tool: upstream, ExposedName: name, Arguments: args,
		Source: store.SourceAgent, AgentID: aid, ChatID: cid, RunID: rid}
	if allowed, _ := Resolve(agentRules(grants), ref); !allowed {
		return outcomeFromResult(m.disp.Deny(ctx, req, "not permitted: "+ref.String()))
	}
	if !toolAllowed(agent.ToolAllow, upstream) {
		return outcomeFromResult(m.disp.Deny(ctx, req, fmt.Sprintf("not permitted: tool %q is not in this chat's allowed tools %v", upstream, agent.ToolAllow)))
	}
	if msg := validateArgs(schema, args); msg != "" {
		lreq := calls.LocalRequest{Label: conn.Label, Server: ch.Name, Tool: upstream, ExposedName: name,
			Arguments: args, Source: store.SourceAgent, AgentID: aid, ChatID: cid, RunID: rid}
		return outcomeFromResult(m.disp.Local(ctx, lreq, func(context.Context) (any, error) { return nil, errors.New(msg) }))
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
	// reason explains an irreversible gate (W1a) to whoever answers it.
	reason string
	// questions is set when this entry is switchboard.user.ask waiting for the
	// user rather than a call waiting for approval; it rides the same plumbing
	// (pending list, stream frame, waiting run status) and differs in the reply.
	questions []Question
}

// needsApproval is W1: never / always, or for "destructive" every tool that is
// not provably harmless. Annotations are advisory data from the upstream (W4),
// so a missing hint must not open the gate: per the MCP spec a tool that is not
// readOnly defaults to destructiveHint=true and openWorldHint=true. Approval is
// therefore skipped only when readOnlyHint is true, or when both destructiveHint
// and openWorldHint are explicitly false. Nil annotations require approval.
// upstreamMeta is the stable origin metadata the hub puts on every proxied
// upstream tool (H5a), plus the W10 pass-through: a tool that declared itself
// irreversible keeps that fact for downstream consumers of /mcp.
func upstreamMeta(conn *registry.Connection, ch *registry.ServerChannel, tool registry.ToolInfo) mcp.Meta {
	meta := mcp.Meta{"host": conn.Label, "project": ch.Project, "server": ch.Name, "upstreamName": tool.Name}
	if tool.Annotations != nil && tool.Annotations.Irreversible {
		meta["irreversible"] = true
	}
	return meta
}

func needsApproval(mode string, ann *registry.ToolAnnotations) bool {
	// W10: an irreversible action gates even under approval=never — that mode
	// is the user's decision about friction, not a licence to move a branch
	// on someone else's machine unasked. (The chat's auto-approver, checked
	// before this in gate, is the user's explicit answer to exactly that.)
	if ann != nil && ann.Irreversible {
		return true
	}
	switch mode {
	case store.ApprovalAlways:
		return true
	case store.ApprovalDestructive:
		if ann == nil {
			return true
		}
		if ann.ReadOnly {
			return false
		}
		explicitlyBenign := ann.Destructive != nil && !*ann.Destructive && ann.OpenWorld != nil && !*ann.OpenWorld
		return !explicitlyBenign
	}
	return false
}

// gate runs the approval step of 5.3/5.8. It returns ok=false with the reason
// to give the model when the call must not proceed. The chat's own preferences
// apply first (I1, I3): the auto-approver resolves every call without waiting
// — irreversible tools included, it being the user's explicit standing yes —
// and a chat-level approval mode overrides the owning agent's.
func (m *Manager) gate(ctx context.Context, agent *store.Agent, rs *runState, callID, tool string, args map[string]any, ann *registry.ToolAnnotations) (bool, string) {
	mode := agent.Approval
	if c := m.gateChat(ctx, rs); c != nil {
		if c.AutoApprove {
			return true, ""
		}
		mode = c.ApprovalMode(agent.Approval)
	}
	if !needsApproval(mode, ann) {
		return true, ""
	}
	if rs == nil {
		return false, fmt.Sprintf(
			"approval is required for this call (%s) but /mcp/agent has no run to pause; use the console (gate rule: %s)",
			tool, approvalRuleNote(mode, ann))
	}
	timeout := time.Duration(m.set.ApprovalTimeout) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	now := time.Now().UTC()
	pa := &pendingApproval{tool: tool, arguments: args, createdAt: now, expiresAt: now.Add(timeout), ch: make(chan approvalDecision, 1)}
	frame := map[string]any{"callId": callID, "name": tool, "arguments": args, "expiresAt": store.FormatTime(pa.expiresAt)}
	if ann != nil && ann.Irreversible && ann.Reason != "" {
		pa.reason = ann.Reason
		frame["reason"] = ann.Reason
	}
	m.mu.Lock()
	rs.approvals[callID] = pa
	m.mu.Unlock()

	m.enterWait(rs, true)
	m.frame(rs, "approval_required", frame)
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
		dec = approvalDecision{reason: fmt.Sprintf(
			"approval timed out after %s: the call was auto-denied; this is retryable — calling again will ask again",
			timeout.Round(time.Second))}
		outcome = "timeout"
	case <-ctx.Done():
		dec = approvalDecision{reason: approvalCancelReason(ctx, m.set.CallTimeout)}
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
	r := dec.reason
	if outcome == "denied" {
		r = "denied by the approver"
		if dec.reason != "" {
			r += ": " + dec.reason
		}
		r += "; calling again will ask the approver afresh"
	}
	return false, fmt.Sprintf("%s (pending call: %s; gate rule: %s)", r, tool, approvalRuleNote(mode, ann))
}

// gateChat reads the chat whose run is calling, for the approval preferences
// read at gate time (a toggle flips behaviour for the next call). Runs without
// a chat (an agent reached over /mcp with no run) have none.
func (m *Manager) gateChat(ctx context.Context, rs *runState) *store.Chat {
	if rs == nil || rs.chatID == "" {
		return nil
	}
	c, err := m.st.GetChat(ctx, rs.chatID)
	if err != nil || c == nil {
		return nil
	}
	return c
}

// approvalCancelReason turns a ctx cancellation during an approval wait into a
// reason the model can act on. A bare "cancelled" made the hub's own
// CALL_TIMEOUT indistinguishable from a human pressing deny (P1-B); ctx.Err()
// separates them: a deadline here is the hub's clock (nobody denied it),
// anything else is the run or its caller being stopped.
func approvalCancelReason(ctx context.Context, callTimeout float64) string {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf(
			"cancelled while waiting for approval: the hub's per-call deadline (CALL_TIMEOUT, %s) elapsed "+
				"while the approval was still pending — nobody denied it; this is retryable, calling again starts a new wait",
			formatSeconds(callTimeout))
	default:
		if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
			return fmt.Sprintf("cancelled while waiting for approval: %v (ctx: %v); the run or its caller was stopped; retryable once a run is active again", cause, ctx.Err())
		}
		return fmt.Sprintf("cancelled while waiting for approval: the run or its caller was cancelled (%v); retryable once a run is active again", ctx.Err())
	}
}

// approvalRuleNote states, from data the hub actually holds, which rule
// gated this call: the effective approval mode plus the annotation (or its
// absence) that made the call non-harmless. The model sees this in the tool
// error so a rejection is explainable, not a mystery "cancelled".
func approvalRuleNote(mode string, ann *registry.ToolAnnotations) string {
	if ann != nil && ann.Irreversible {
		note := "the tool declares this action irreversible (no undo from this machine), which gates it even under approval mode '" + mode + "'"
		if ann.Reason != "" {
			note += ": " + ann.Reason
		}
		return note
	}
	switch mode {
	case store.ApprovalAlways:
		return "approval mode 'always' gates every tool call"
	case store.ApprovalDestructive:
		switch {
		case ann == nil:
			return "approval mode 'destructive' and the tool sent no annotations (absent hints default to destructive)"
		case ann.Destructive == nil || *ann.Destructive:
			return "approval mode 'destructive' and destructiveHint is true or missing"
		case ann.OpenWorld == nil || *ann.OpenWorld:
			return "approval mode 'destructive' and openWorldHint is true or missing"
		}
	}
	return "the approval gate flagged this call"
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
	cat, err := m.buildCatalog(ctx, agent, nil) // the /mcp surface has no chat context: unlock tools stay hidden
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
