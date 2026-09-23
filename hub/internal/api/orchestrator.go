package api

// The orchestrator's HTTP surface (spec.md 7.2 - 7.4). Registered only when
// Options.Agents is set, so with AGENTS_ENABLED=false every route here 404s (E7).
//
// Reads come straight from the store through OrchestratorReader; every write
// that has orchestration semantics (agents, grants, edges, posting, branching,
// cancelling, approvals) goes through agents.Service, which is the only thing
// allowed to publish to the bus and the chat streams. Chat metadata edits
// (title, tags, archive, active leaf) are plain store writes; creating, rebinding
// and deleting a chat go through the Service (they touch its execution record).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/agents"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// OrchestratorReader is the slice of the store the orchestrator routes use.
// SQLiteStore satisfies it. It includes the few chat-metadata writes the
// Service has no method for.
type OrchestratorReader interface {
	GetAgent(ctx context.Context, id string) (*store.Agent, error)
	ListAgents(ctx context.Context, f store.AgentFilter) ([]store.Agent, error)

	ListGrants(ctx context.Context, agentID string) ([]store.Grant, error)

	CreateChat(ctx context.Context, c store.Chat) (store.Chat, error)
	GetChat(ctx context.Context, id string) (*store.Chat, error)
	ListChats(ctx context.Context, f store.ChatFilter) ([]store.Chat, error)
	UpdateChat(ctx context.Context, id string, p store.ChatPatch) (store.Chat, error)
	DeleteChat(ctx context.Context, id string) error
	SetActiveLeaf(ctx context.Context, chatID, messageID string) error
	SelectSibling(ctx context.Context, messageID string) (string, error)

	GetMessage(ctx context.Context, id string) (*store.Message, error)
	ActivePath(ctx context.Context, chatID string) ([]store.Message, error)
	PathTo(ctx context.Context, messageID string) ([]store.Message, error)
	Siblings(ctx context.Context, messageID string) (store.SiblingSet, error)
	ListMessages(ctx context.Context, chatID string) ([]store.Message, error)
	SearchMessages(ctx context.Context, q store.SearchQuery) ([]store.SearchHit, error)

	GetDraft(ctx context.Context, chatID string) (store.Draft, error)
	SetDraft(ctx context.Context, chatID, content string) (store.Draft, error)

	GetRun(ctx context.Context, id string) (*store.Run, error)
	ListRuns(ctx context.Context, f store.RunFilter) ([]store.Run, error)
}

const maxBody = 8 << 20

func (h *Handler) registerOrchestrator() {
	if h.opts.Agents == nil {
		return
	}
	if h.rd == nil {
		h.log.Warn("agents are enabled but the store cannot serve orchestrator reads; routes not registered")
		return
	}
	m := h.mux
	m.HandleFunc("GET /api/agents", h.listAgents)
	m.HandleFunc("POST /api/agents", h.createAgent)
	m.HandleFunc("GET /api/agents/{id}", h.getAgent)
	m.HandleFunc("PATCH /api/agents/{id}", h.patchAgent)
	m.HandleFunc("DELETE /api/agents/{id}", h.deleteAgent)
	m.HandleFunc("GET /api/agents/{id}/grants", h.getGrants)
	m.HandleFunc("PUT /api/agents/{id}/grants", h.putGrants)
	m.HandleFunc("GET /api/graph", h.graph)
	m.HandleFunc("PUT /api/graph/edges/{from}/{to}", h.putEdge)

	m.HandleFunc("GET /api/chats", h.listChats)
	m.HandleFunc("POST /api/chats", h.createChat)
	m.HandleFunc("GET /api/chats/{id}", h.getChat)
	m.HandleFunc("PATCH /api/chats/{id}", h.patchChat)
	m.HandleFunc("DELETE /api/chats/{id}", h.deleteChat)
	m.HandleFunc("GET /api/chats/{id}/messages", h.getMessages)
	m.HandleFunc("POST /api/chats/{id}/messages", h.postMessage)
	m.HandleFunc("POST /api/chats/{id}/branch", h.branch)
	m.HandleFunc("GET /api/chats/{id}/export", h.exportChat)
	m.HandleFunc("GET /api/chats/{id}/draft", h.getDraft)
	m.HandleFunc("PUT /api/chats/{id}/draft", h.putDraft)
	if h.opts.Streams != nil {
		m.HandleFunc("GET /api/chats/{id}/stream", h.chatStream)
	}

	m.HandleFunc("GET /api/runs/{id}", h.getRun)
	m.HandleFunc("POST /api/runs/{id}/cancel", h.cancelRun)
	m.HandleFunc("POST /api/runs/{id}/approvals/{callId}", h.approve)
	m.HandleFunc("GET /api/approvals", h.listApprovals)
	m.HandleFunc("GET /api/models", h.models)

	m.HandleFunc("GET /api/profiles", h.listProfiles)
	m.HandleFunc("POST /api/profiles", h.createProfile)
	m.HandleFunc("GET /api/profiles/{id}", h.getProfile)
	m.HandleFunc("PATCH /api/profiles/{id}", h.patchProfile)
	m.HandleFunc("DELETE /api/profiles/{id}", h.deleteProfile)
	m.HandleFunc("GET /api/hub-tools", h.hubTools)
	m.HandleFunc("GET /api/chats/{id}/tools", h.chatTools)
	m.HandleFunc("GET /api/chats/{id}/system-prompt", h.chatSystemPrompt)
}

// writeErr maps the store's sentinels (which Service passes through, wrapped)
// onto status codes: 404 / 409 / 400, anything else 500 with the detail hidden.
func (h *Handler) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrNameTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, agents.ErrNotPending), errors.Is(err, agents.ErrLimit), errors.Is(err, agents.ErrConflict),
		strings.Contains(err.Error(), "not pending"):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, agents.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	default:
		h.log.Error("orchestrator request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, into any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := decodeBody(r, into); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// --------------------------------------------------------------------------
// agents
// --------------------------------------------------------------------------

func (h *Handler) listAgents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := h.rd.ListAgents(r.Context(), store.AgentFilter{
		ParentID:       clean(q.Get("parentId")),
		RootsOnly:      truthy(q.Get("roots")),
		Project:        clean(q.Get("project")),
		IncludeDeleted: truthy(q.Get("includeDeleted")),
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if list == nil {
		list = []store.Agent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": list})
}

type createAgentBody struct {
	ParentID     string             `json:"parentId"`
	Name         string             `json:"name"`
	Description  string             `json:"description"`
	Project      string             `json:"project"`
	Model        json.RawMessage    `json:"model"`
	SystemPrompt string             `json:"systemPrompt"`
	Budget       json.RawMessage    `json:"budget"`
	Capabilities store.Capabilities `json:"capabilities"`
	Approval     string             `json:"approval"`
	AutoWake     *bool              `json:"autoWake"`
	Grants       []store.Grant      `json:"grants"`
	// docs/AGENT_MODEL_API.md. ClientLabel is a RawMessage so a present key
	// (even null) can be told from an absent one.
	ProfileID   *string         `json:"profileId"`
	ClientLabel json.RawMessage `json:"clientLabel"`
}

// clientField decodes a "string or null" JSON key; present says whether the key was sent.
func clientField(raw json.RawMessage) (label *string, present bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if err := json.Unmarshal(raw, &label); err != nil {
		return nil, true, errors.New("clientLabel must be a string or null")
	}
	return label, true, nil
}

func validApproval(s string) bool {
	return s == "" || s == store.ApprovalNever || s == store.ApprovalDestructive || s == store.ApprovalAlways
}

func (h *Handler) createAgent(w http.ResponseWriter, r *http.Request) {
	var b createAgentBody
	if !h.readBody(w, r, &b) {
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if !validApproval(b.Approval) {
		writeError(w, http.StatusBadRequest, "approval must be never, destructive or always")
		return
	}
	label, hasClient, err := clientField(b.ClientLabel)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if label != nil && (strings.TrimSpace(*label) == "" || *label == store.Wildcard) {
		writeError(w, http.StatusBadRequest, "clientLabel must name exactly one client (not empty, not *)")
		return
	}
	in := agents.CreateAgentInput{
		ParentID: b.ParentID, Name: b.Name, Description: b.Description, Project: b.Project,
		Model: b.Model, SystemPrompt: b.SystemPrompt, Budget: b.Budget, Capabilities: b.Capabilities,
		Approval: b.Approval, AutoWake: b.AutoWake, Grants: b.Grants,
		ClientSet: hasClient, ClientLabel: label,
	}
	if b.ProfileID != nil {
		in.ProfileID = strings.TrimSpace(*b.ProfileID)
	}
	agent, err := h.opts.Agents.CreateAgent(r.Context(), in)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, agent)
}

func (h *Handler) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.rd.GetAgent(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if a == nil {
		writeError(w, http.StatusNotFound, "unknown agent")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

type patchAgentBody struct {
	Name         *string             `json:"name"`
	Description  *string             `json:"description"`
	Project      json.RawMessage     `json:"project"` // string, or null to clear
	Model        json.RawMessage     `json:"model"`
	SystemPrompt *string             `json:"systemPrompt"`
	Budget       json.RawMessage     `json:"budget"`
	Capabilities *store.Capabilities `json:"capabilities"`
	Approval     *string             `json:"approval"`
	AutoWake     *bool               `json:"autoWake"`
	ProfileID    json.RawMessage     `json:"profileId"`   // string, or null to detach
	ClientLabel  json.RawMessage     `json:"clientLabel"` // string, or null for none
}

func (h *Handler) patchAgent(w http.ResponseWriter, r *http.Request) {
	var b patchAgentBody
	if !h.readBody(w, r, &b) {
		return
	}
	patch := store.AgentPatch{
		Name: b.Name, Description: b.Description, Model: b.Model, SystemPrompt: b.SystemPrompt,
		Budget: b.Budget, Capabilities: b.Capabilities, Approval: b.Approval, AutoWake: b.AutoWake,
	}
	if patch.Name != nil {
		n := strings.TrimSpace(*patch.Name)
		if n == "" {
			writeError(w, http.StatusBadRequest, "name must not be empty")
			return
		}
		patch.Name = &n
	}
	if patch.Approval != nil && (*patch.Approval == "" || !validApproval(*patch.Approval)) {
		writeError(w, http.StatusBadRequest, "approval must be never, destructive or always")
		return
	}
	if len(b.Project) > 0 {
		var p *string
		if err := json.Unmarshal(b.Project, &p); err != nil {
			writeError(w, http.StatusBadRequest, "project must be a string or null")
			return
		}
		empty := ""
		if p == nil {
			p = &empty
		}
		patch.Project = p
	}
	if len(b.ProfileID) > 0 {
		var p *string
		if err := json.Unmarshal(b.ProfileID, &p); err != nil {
			writeError(w, http.StatusBadRequest, "profileId must be a string or null")
			return
		}
		if p != nil {
			t := strings.TrimSpace(*p)
			if t == "" {
				writeError(w, http.StatusBadRequest, "profileId must not be empty")
				return
			}
			p = &t
		}
		patch.SetProfile, patch.ProfileID = true, p
	}
	if label, present, err := clientField(b.ClientLabel); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if present {
		if label != nil && (strings.TrimSpace(*label) == "" || *label == store.Wildcard) {
			writeError(w, http.StatusBadRequest, "clientLabel must name exactly one client (not empty, not *)")
			return
		}
		patch.SetClient, patch.ClientLabel = true, label
	}
	a, err := h.opts.Agents.UpdateAgent(r.Context(), r.PathValue("id"), patch)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (h *Handler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := h.opts.Agents.DeleteAgent(r.Context(), r.PathValue("id"), truthy(r.URL.Query().Get("hard"))); err != nil {
		h.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// grants, graph
// --------------------------------------------------------------------------

func (h *Handler) requireAgent(w http.ResponseWriter, r *http.Request, id string) bool {
	a, err := h.rd.GetAgent(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return false
	}
	if a == nil {
		writeError(w, http.StatusNotFound, "unknown agent")
		return false
	}
	return true
}

func (h *Handler) getGrants(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.requireAgent(w, r, id) {
		return
	}
	grants, err := h.rd.ListGrants(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if grants == nil {
		grants = []store.Grant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants})
}

// putGrants body: either one grant {label, project, server, allowed} or
// {grants: [...]}. Each is *upserted* as a human edit (A13) through
// Service.SetGrant; grants absent from the list are left alone (to withdraw
// access, send allowed:false). The response is the agent's resulting set plus
// the descendant grants the narrowing revoked (A14).
func (h *Handler) putGrants(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.requireAgent(w, r, id) {
		return
	}
	var raw struct {
		Grants  *[]store.Grant `json:"grants"`
		Label   *string        `json:"label"`
		Project string         `json:"project"`
		Server  *string        `json:"server"`
		Allowed *bool          `json:"allowed"`
	}
	if !h.readBody(w, r, &raw) {
		return
	}
	var desired []store.Grant
	switch {
	case raw.Grants != nil:
		desired = *raw.Grants
	case raw.Label != nil && raw.Server != nil && raw.Allowed != nil:
		desired = []store.Grant{{Label: *raw.Label, Project: raw.Project, Server: *raw.Server, Allowed: *raw.Allowed}}
	default:
		writeError(w, http.StatusBadRequest, "send {grants: [...]} or a single grant {label, project, server, allowed}")
		return
	}
	for _, g := range desired {
		if strings.TrimSpace(g.Label) == "" || strings.TrimSpace(g.Server) == "" {
			writeError(w, http.StatusBadRequest, "a grant needs label and server (use \"*\" for any)")
			return
		}
	}
	revoked := []store.Grant{}
	for _, g := range desired {
		g.AgentID, g.Source = id, store.GrantHuman
		rev, err := h.opts.Agents.SetGrant(r.Context(), g)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		revoked = append(revoked, rev...)
	}
	grants, err := h.rd.ListGrants(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if grants == nil {
		grants = []store.Grant{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": grants, "revoked": revoked})
}

func (h *Handler) graph(w http.ResponseWriter, r *http.Request) {
	g, err := h.opts.Agents.Graph(r.Context())
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (h *Handler) putEdge(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Allowed *bool `json:"allowed"`
	}
	if !h.readBody(w, r, &b) {
		return
	}
	if b.Allowed == nil {
		writeError(w, http.StatusBadRequest, "allowed is required")
		return
	}
	e, err := h.opts.Agents.SetEdge(r.Context(), r.PathValue("from"), r.PathValue("to"), *b.Allowed)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// --------------------------------------------------------------------------
// chats
// --------------------------------------------------------------------------

func (h *Handler) listChats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ChatFilter{
		AgentID: clean(q.Get("agentId")), Kind: clean(q.Get("kind")),
		IncludeArchived: truthy(q.Get("includeArchived")),
	}
	tag, text := clean(q.Get("tag")), clean(q.Get("q"))
	limit := intParam(q.Get("limit"), store.DefaultLimit)
	if limit < 1 {
		limit = 1
	}
	if limit > store.MaxLimit {
		limit = store.MaxLimit
	}
	offset := max(intParam(q.Get("offset"), 0), 0)

	ctx := r.Context()
	if tag == "" && text == "" {
		f.Limit, f.Offset = limit, offset
		chats, err := h.rd.ListChats(ctx, f)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		if chats == nil {
			chats = []store.Chat{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"chats": chats, "limit": limit, "offset": offset})
		return
	}

	// tag and q filter in memory over the store's filtered listing (bounded).
	all := []store.Chat{}
	f.Limit = store.MaxLimit
	for page := 0; page < 20; page++ {
		f.Offset = page * store.MaxLimit
		chunk, err := h.rd.ListChats(ctx, f)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		all = append(all, chunk...)
		if len(chunk) < store.MaxLimit {
			break
		}
	}
	hasTag := func(c store.Chat) bool {
		if tag == "" {
			return true
		}
		for _, t := range c.Tags {
			if t == tag {
				return true
			}
		}
		return false
	}
	matched := map[string]store.Chat{}
	hits := map[string][]store.SearchHit{}
	if text == "" {
		for _, c := range all {
			if hasTag(c) {
				matched[c.ID] = c
			}
		}
	} else {
		lower := strings.ToLower(text)
		byID := map[string]store.Chat{}
		for _, c := range all {
			byID[c.ID] = c
			if hasTag(c) && strings.Contains(strings.ToLower(c.Title), lower) {
				matched[c.ID] = c
			}
		}
		found, err := h.rd.SearchMessages(ctx, store.SearchQuery{Text: text, AgentID: f.AgentID, Limit: store.MaxLimit})
		if err != nil {
			h.writeErr(w, err)
			return
		}
		for _, hit := range found {
			c, ok := byID[hit.ChatID] // honours kind / archived / agent filters
			if !ok || !hasTag(c) {
				continue
			}
			matched[c.ID] = c
			hits[c.ID] = append(hits[c.ID], hit)
		}
	}
	out := make([]store.Chat, 0, len(matched))
	for _, c := range matched {
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].ID > out[j].ID
	})
	if offset >= len(out) {
		out = out[:0]
	} else {
		out = out[offset:]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	body := map[string]any{"chats": out, "limit": limit, "offset": offset}
	if text != "" {
		body["hits"] = hits
	}
	writeJSON(w, http.StatusOK, body)
}

// createChat is POST /api/chats (docs/CHAT_MODEL_API.md). Precedence: a body with
// agentId is the legacy attach form {agentId, title?} (a new chat on an existing
// execution record); otherwise it is the primary form, which creates the chat and
// its record together. The older {profileId?, clientLabel} form is that primary
// form with fewer fields, except that clientLabel "" is now 400 like "*".
func (h *Handler) createChat(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Title        string          `json:"title"`
		ProfileID    json.RawMessage `json:"profileId"` // absent, null or a string
		SystemPrompt *string         `json:"systemPrompt"`
		ClientLabel  json.RawMessage `json:"clientLabel"` // absent, null or a string
		Model        json.RawMessage `json:"model"`
		ParentChatID string          `json:"parentChatId"`
		// Legacy attach form.
		AgentID string `json:"agentId"`
		Kind    string `json:"kind"`
	}
	if !h.readBody(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.AgentID) == "" {
		in := agents.CreateChatInput{
			Title: b.Title, SystemPrompt: b.SystemPrompt, Model: b.Model, ParentChatID: strings.TrimSpace(b.ParentChatID),
		}
		profile, present, err := clientField(b.ProfileID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "profileId must be a string or null")
			return
		}
		switch {
		case present && profile == nil:
			in.ProfileNone = true
		case profile != nil:
			in.ProfileID = strings.TrimSpace(*profile)
		}
		label, _, err := clientField(b.ClientLabel)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if label != nil {
			if strings.TrimSpace(*label) == "" || *label == store.Wildcard {
				writeError(w, http.StatusBadRequest, "clientLabel must name exactly one client (not empty, not *)")
				return
			}
			in.ClientLabel = strings.TrimSpace(*label)
		}
		c, err := h.opts.Agents.CreateChat(r.Context(), in)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, c)
		return
	}
	if b.Kind != "" && b.Kind != store.ChatKindHuman {
		writeError(w, http.StatusBadRequest, "only human chats can be created over the API")
		return
	}
	a, err := h.rd.GetAgent(r.Context(), b.AgentID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if a == nil || a.DeletedAt != nil {
		writeError(w, http.StatusNotFound, "unknown agent")
		return
	}
	// The chat mirrors the agent's profile and client (docs/AGENT_MODEL_API.md).
	c, err := h.rd.CreateChat(r.Context(), store.Chat{AgentID: b.AgentID, Title: b.Title, Kind: store.ChatKindHuman,
		ProfileID: a.ProfileID, ClientLabel: a.ClientLabel})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *Handler) loadChat(w http.ResponseWriter, r *http.Request) *store.Chat {
	c, err := h.rd.GetChat(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeErr(w, err)
		return nil
	}
	if c == nil {
		writeError(w, http.StatusNotFound, "unknown chat")
		return nil
	}
	return c
}

func (h *Handler) getChat(w http.ResponseWriter, r *http.Request) {
	if c := h.loadChat(w, r); c != nil {
		writeJSON(w, http.StatusOK, c)
	}
}

func (h *Handler) patchChat(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Title           *string   `json:"title"`
		Tags            *[]string `json:"tags"`
		Archived        *bool     `json:"archived"`
		ActiveLeafID    *string   `json:"activeLeafId"`
		SelectMessageID *string   `json:"selectMessageId"`
		// Rebinding (docs/CHAT_MODEL_API.md): absent = unchanged, null = none.
		ProfileID    json.RawMessage `json:"profileId"`
		SystemPrompt *string         `json:"systemPrompt"`
		ClientLabel  json.RawMessage `json:"clientLabel"`
	}
	if !h.readBody(w, r, &b) {
		return
	}
	c := h.loadChat(w, r)
	if c == nil {
		return
	}
	ctx := r.Context()
	var rebind agents.ChatUpdate
	if p, present, err := clientField(b.ProfileID); err != nil {
		writeError(w, http.StatusBadRequest, "profileId must be a string or null")
		return
	} else if present {
		if p != nil {
			t := strings.TrimSpace(*p)
			if t == "" {
				writeError(w, http.StatusBadRequest, "profileId must not be empty")
				return
			}
			p = &t
		}
		rebind.ProfileSet, rebind.ProfileID = true, p
	}
	if l, present, err := clientField(b.ClientLabel); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if present {
		if l != nil && (strings.TrimSpace(*l) == "" || *l == store.Wildcard) {
			writeError(w, http.StatusBadRequest, "clientLabel must name exactly one client (not empty, not *)")
			return
		}
		rebind.ClientSet, rebind.ClientLabel = true, l
	}
	rebind.SystemPrompt = b.SystemPrompt
	if b.ActiveLeafID != nil {
		if err := h.rd.SetActiveLeaf(ctx, c.ID, *b.ActiveLeafID); err != nil {
			h.writeErr(w, err)
			return
		}
	}
	if b.SelectMessageID != nil {
		m, err := h.rd.GetMessage(ctx, *b.SelectMessageID)
		if err != nil {
			h.writeErr(w, err)
			return
		}
		if m == nil || m.ChatID != c.ID {
			writeError(w, http.StatusNotFound, "unknown message")
			return
		}
		if _, err := h.rd.SelectSibling(ctx, m.ID); err != nil {
			h.writeErr(w, err)
			return
		}
	}
	if rebind.ProfileSet || rebind.ClientSet || rebind.SystemPrompt != nil {
		if _, err := h.opts.Agents.UpdateChat(ctx, c.ID, rebind); err != nil {
			h.writeErr(w, err)
			return
		}
	}
	updated := *c
	if b.Title != nil || b.Tags != nil || b.Archived != nil {
		u, err := h.rd.UpdateChat(ctx, c.ID, store.ChatPatch{Title: b.Title, Tags: b.Tags, Archived: b.Archived})
		if err != nil {
			h.writeErr(w, err)
			return
		}
		updated = u
	}
	if b.ActiveLeafID != nil || b.SelectMessageID != nil || rebind.ProfileSet || rebind.ClientSet || rebind.SystemPrompt != nil {
		fresh, err := h.rd.GetChat(ctx, c.ID)
		if err != nil || fresh == nil {
			h.writeErr(w, fmt.Errorf("re-reading chat: %w", firstErr(err, store.ErrNotFound)))
			return
		}
		updated = *fresh
	}
	writeJSON(w, http.StatusOK, updated)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// deleteChat permanently deletes the chat, its child chats and the execution
// records nothing else uses, cancelling their runs (Service.DeleteChat).
func (h *Handler) deleteChat(w http.ResponseWriter, r *http.Request) {
	c := h.loadChat(w, r)
	if c == nil {
		return
	}
	n, err := h.opts.Agents.DeleteChat(r.Context(), c.ID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deletedChats": n})
}

// --------------------------------------------------------------------------
// messages
// --------------------------------------------------------------------------

// messageView is a message plus its sibling set (A6).
func messageView(m store.Message, sib store.SiblingSet) (json.RawMessage, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	if sib.IDs == nil {
		sib.IDs = []string{}
	}
	s, _ := json.Marshal(sib)
	obj["siblings"] = s
	return json.Marshal(obj)
}

func (h *Handler) viewPath(ctx context.Context, path []store.Message) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(path))
	for _, m := range path {
		sib, err := h.rd.Siblings(ctx, m.ID)
		if err != nil {
			return nil, err
		}
		v, err := messageView(m, sib)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// viewTree computes sibling sets in memory from the creation-ordered list.
func viewTree(all []store.Message) ([]json.RawMessage, error) {
	groups := map[string][]string{}
	key := func(m store.Message) string {
		if m.ParentID == nil {
			return ""
		}
		return *m.ParentID
	}
	for _, m := range all {
		groups[key(m)] = append(groups[key(m)], m.ID)
	}
	out := make([]json.RawMessage, 0, len(all))
	for _, m := range all {
		ids := groups[key(m)]
		idx := 0
		for i, id := range ids {
			if id == m.ID {
				idx = i
			}
		}
		v, err := messageView(m, store.SiblingSet{IDs: ids, Index: idx})
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (h *Handler) getMessages(w http.ResponseWriter, r *http.Request) {
	c := h.loadChat(w, r)
	if c == nil {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	var (
		views []json.RawMessage
		err   error
		tree  bool
	)
	switch {
	case truthy(q.Get("tree")):
		tree = true
		var all []store.Message
		if all, err = h.rd.ListMessages(ctx, c.ID); err == nil {
			views, err = viewTree(all)
		}
	case clean(q.Get("leaf")) != "":
		leaf := clean(q.Get("leaf"))
		var m *store.Message
		if m, err = h.rd.GetMessage(ctx, leaf); err == nil && (m == nil || m.ChatID != c.ID) {
			writeError(w, http.StatusNotFound, "unknown message")
			return
		}
		if err == nil {
			var path []store.Message
			if path, err = h.rd.PathTo(ctx, leaf); err == nil {
				views, err = h.viewPath(ctx, path)
			}
		}
	default:
		var path []store.Message
		if path, err = h.rd.ActivePath(ctx, c.ID); err == nil {
			views, err = h.viewPath(ctx, path)
		}
	}
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if views == nil {
		views = []json.RawMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"chatId": c.ID, "activeLeafId": c.ActiveLeafID, "tree": tree, "messages": views,
	})
}

// contentBlocks accepts a string (shorthand for one text block) or a block array.
func contentBlocks(raw json.RawMessage) ([]llm.Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []llm.Block{{Type: "text", Text: s}}, nil
	}
	var blocks []llm.Block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	for i := range blocks {
		if blocks[i].Type == "" {
			blocks[i].Type = "text"
		}
	}
	return blocks, nil
}

func (h *Handler) postMessage(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Content  json.RawMessage `json:"content"`
		ParentID string          `json:"parentId"`
		Model    json.RawMessage `json:"model"`
	}
	if !h.readBody(w, r, &b) {
		return
	}
	blocks, err := contentBlocks(b.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, "content must be a string or an array of content blocks")
		return
	}
	if len(blocks) == 0 {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}
	res, err := h.opts.Agents.Post(r.Context(), r.PathValue("id"), agents.PostInput{
		Content: blocks, ParentID: b.ParentID, Model: b.Model,
		IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")),
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (h *Handler) branch(w http.ResponseWriter, r *http.Request) {
	var b struct {
		FromMessageID string          `json:"fromMessageId"`
		Content       json.RawMessage `json:"content"`
		Model         json.RawMessage `json:"model"`
	}
	if !h.readBody(w, r, &b) {
		return
	}
	if strings.TrimSpace(b.FromMessageID) == "" {
		writeError(w, http.StatusBadRequest, "fromMessageId is required")
		return
	}
	blocks, err := contentBlocks(b.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, "content must be a string or an array of content blocks")
		return
	}
	res, err := h.opts.Agents.Branch(r.Context(), r.PathValue("id"), agents.BranchInput{
		FromMessageID: b.FromMessageID, Content: blocks, Model: b.Model,
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, res)
}

func (h *Handler) chatStream(w http.ResponseWriter, r *http.Request) {
	c := h.loadChat(w, r)
	if c == nil {
		return
	}
	h.opts.Streams.ServeSSE(w, r, c.ID)
}

// --------------------------------------------------------------------------
// runs, models
// --------------------------------------------------------------------------

func (h *Handler) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.rd.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "unknown run")
		return
	}
	raw, _ := json.Marshal(run)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	pending := h.opts.Agents.PendingApprovals(run.ID)
	if pending == nil {
		pending = []agents.PendingApproval{}
	}
	obj["pendingApprovals"], _ = json.Marshal(pending)
	writeJSON(w, http.StatusOK, obj)
}

func (h *Handler) listApprovals(w http.ResponseWriter, r *http.Request) {
	list, err := h.opts.Agents.AllPendingApprovals(r.Context())
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if list == nil {
		list = []agents.PendingApprovalDetail{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": list})
}

func (h *Handler) cancelRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := h.rd.GetRun(r.Context(), id)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if run == nil {
		writeError(w, http.StatusNotFound, "unknown run")
		return
	}
	if err := h.opts.Agents.CancelRun(r.Context(), id); err != nil {
		h.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) approve(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Approved *bool  `json:"approved"`
		Reason   string `json:"reason"`
	}
	if !h.readBody(w, r, &b) {
		return
	}
	if b.Approved == nil {
		writeError(w, http.StatusBadRequest, "approved is required")
		return
	}
	if err := h.opts.Agents.Approve(r.Context(), r.PathValue("id"), r.PathValue("callId"), *b.Approved, b.Reason); err != nil {
		h.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	// Lazy discovery refresh, but never wait on a slow provider for long: serve
	// the cached list and let the refresh finish in the background.
	if rf, ok := h.opts.Agents.(interface {
		RefreshModels(ctx context.Context, wait time.Duration)
	}); ok {
		rf.RefreshModels(r.Context(), 2*time.Second)
	}
	m := h.opts.Agents.Models()
	if m == nil {
		m = []llm.ModelSpec{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": m})
}

// --------------------------------------------------------------------------
// profiles, hub tools, chat tools (docs/PROFILES_API.md)
// --------------------------------------------------------------------------

func (h *Handler) listProfiles(w http.ResponseWriter, r *http.Request) {
	ps, err := h.opts.Agents.ListProfiles(r.Context())
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if ps == nil {
		ps = []store.Profile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": ps})
}

func (h *Handler) getProfile(w http.ResponseWriter, r *http.Request) {
	p, err := h.opts.Agents.GetProfile(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) createProfile(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name         string             `json:"name"`
		Description  string             `json:"description"`
		SystemPrompt string             `json:"systemPrompt"`
		Model        json.RawMessage    `json:"model"`
		Capabilities store.Capabilities `json:"capabilities"`
		Approval     string             `json:"approval"`
		Budget       json.RawMessage    `json:"budget"`
	}
	if !h.readBody(w, r, &b) {
		return
	}
	p, err := h.opts.Agents.CreateProfile(r.Context(), agents.ProfileInput{
		Name: b.Name, Description: b.Description, SystemPrompt: b.SystemPrompt, Model: b.Model,
		Capabilities: b.Capabilities, Approval: b.Approval, Budget: b.Budget,
	})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *Handler) patchProfile(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name         *string         `json:"name"`
		Description  *string         `json:"description"`
		SystemPrompt *string         `json:"systemPrompt"`
		Model        json.RawMessage `json:"model"` // absent: unchanged; null: cleared
		Capabilities *struct {
			CanSpawn   *bool `json:"canSpawn"`
			CanMessage *bool `json:"canMessage"`
		} `json:"capabilities"`
		Approval  *string         `json:"approval"`
		Budget    json.RawMessage `json:"budget"`
		IsDefault *bool           `json:"isDefault"`
	}
	if !h.readBody(w, r, &b) {
		return
	}
	in := agents.ProfileUpdate{
		Name: b.Name, Description: b.Description, SystemPrompt: b.SystemPrompt, Model: b.Model,
		Approval: b.Approval, Budget: b.Budget, IsDefault: b.IsDefault,
	}
	if b.Capabilities != nil {
		in.CanSpawn, in.CanMessage = b.Capabilities.CanSpawn, b.Capabilities.CanMessage
	}
	p, err := h.opts.Agents.UpdateProfile(r.Context(), r.PathValue("id"), in)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *Handler) deleteProfile(w http.ResponseWriter, r *http.Request) {
	if err := h.opts.Agents.DeleteProfile(r.Context(), r.PathValue("id")); err != nil {
		h.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) hubTools(w http.ResponseWriter, r *http.Request) {
	tools := h.opts.Agents.HubTools()
	if tools == nil {
		tools = []agents.HubTool{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tools": tools})
}

func strOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (h *Handler) chatSystemPrompt(w http.ResponseWriter, r *http.Request) {
	v, err := h.opts.Agents.ChatSystemPrompt(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handler) chatTools(w http.ResponseWriter, r *http.Request) {
	v, err := h.opts.Agents.ChatTools(r.Context(), r.PathValue("id"))
	if err != nil {
		h.writeErr(w, err)
		return
	}
	if v.Tools == nil {
		v.Tools = []agents.ChatTool{}
	}
	writeJSON(w, http.StatusOK, v)
}

// --------------------------------------------------------------------------
// drafts
// --------------------------------------------------------------------------

const maxDraftBody = 256 << 10

type draftJSON struct {
	Draft     string  `json:"draft"`
	UpdatedAt *string `json:"updatedAt"`
}

func draftView(d store.Draft) draftJSON {
	out := draftJSON{Draft: d.Content}
	if d.UpdatedAt != nil {
		s := store.FormatTime(*d.UpdatedAt)
		out.UpdatedAt = &s
	}
	return out
}

func (h *Handler) getDraft(w http.ResponseWriter, r *http.Request) {
	c := h.loadChat(w, r)
	if c == nil {
		return
	}
	d, err := h.rd.GetDraft(r.Context(), c.ID)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, draftView(d))
}

func (h *Handler) putDraft(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Draft string `json:"draft"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxDraftBody)
	if err := decodeBody(r, &b); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "draft too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	c := h.loadChat(w, r)
	if c == nil {
		return
	}
	d, err := h.rd.SetDraft(r.Context(), c.ID, b.Draft)
	if err != nil {
		h.writeErr(w, err)
		return
	}
	h.opts.Bus.Publish(events.Event{Type: events.TypeChat, ChatID: c.ID})
	writeJSON(w, http.StatusOK, draftView(d))
}
