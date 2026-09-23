package agents

import (
	"context"
	"encoding/json"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Service is the surface the HTTP API (internal/api) is written against. The
// orchestrator (Manager) implements it; the API layer never reaches into runs,
// grants or the store's orchestrator tables by any other route, so this file is
// the whole contract between the two. Errors use the sentinel errors below so
// handlers can map them to status codes.
type Service interface {
	// --- agents & grants (spec 5.2, 7.2) ---
	CreateAgent(ctx context.Context, in CreateAgentInput) (*store.Agent, error)
	UpdateAgent(ctx context.Context, id string, patch store.AgentPatch) (*store.Agent, error)
	// DeleteAgent soft-deletes (or hard-deletes when hard) the agent, cancelling
	// its and its descendants' runs.
	DeleteAgent(ctx context.Context, id string, hard bool) error
	// SetGrant is a *human* edit (A13): source "human", not bound by A12, and
	// narrowing propagates per A14. Returns the descendant grants revoked.
	SetGrant(ctx context.Context, g store.Grant) ([]store.Grant, error)
	SetEdge(ctx context.Context, from, to string, allowed bool) (*store.Edge, error)

	// Graph is the one-response join for the Graph view: agents + edges +
	// grants + which grant refs match a live connection + mailbox counts.
	Graph(ctx context.Context) (*GraphView, error)

	// --- chats (spec 5.3, 7.2) ---
	// Post appends a user message under the chat's active leaf (or under
	// ParentID when set) and enqueues a run. A repeated IdempotencyKey within 10
	// minutes returns the original run (R5), Deduplicated true.
	Post(ctx context.Context, chatID string, in PostInput) (*PostResult, error)
	// Branch creates a sibling of FromMessageID (with Content if given, else a
	// copy point for regenerate) and re-points the leaf; for a user-role sibling
	// with content it also enqueues a run, for regenerate (assistant message id)
	// it enqueues a run from the assistant's parent.
	Branch(ctx context.Context, chatID string, in BranchInput) (*PostResult, error)
	CancelRun(ctx context.Context, runID string) error
	// Approve answers a pending approval (W2). ErrNotPending if none waits.
	Approve(ctx context.Context, runID, callID string, approved bool, reason string) error
	// Answer resolves a pending switchboard.user.ask with one answer per
	// question. ErrNotPending if none waits, ErrInvalid on a mismatched answer.
	Answer(ctx context.Context, runID, callID string, answers []QuestionAnswer) error
	// PendingApprovals lists what is currently blocked, for GET /api/runs/{id}.
	PendingApprovals(runID string) []PendingApproval
	// AllPendingApprovals lists every pending approval across all runs, oldest
	// first, for GET /api/approvals (never nil).
	AllPendingApprovals(ctx context.Context) ([]PendingApprovalDetail, error)

	// --- profiles, chats from profiles, tool inspection (docs/PROFILES_API.md) ---
	ListProfiles(ctx context.Context) ([]store.Profile, error)
	GetProfile(ctx context.Context, id string) (*store.Profile, error)
	// CreateProfile: ErrInvalid on a bad field, ErrNameTaken when the name exists.
	CreateProfile(ctx context.Context, in ProfileInput) (*store.Profile, error)
	// UpdateProfile applies a partial update; IsDefault true atomically moves
	// the default, false on the current default is ErrInvalid.
	UpdateProfile(ctx context.Context, id string, in ProfileUpdate) (*store.Profile, error)
	// DeleteProfile: ErrConflict for the default (while others exist) or the
	// only profile. Chats and agents made from it keep working (profileId nulled).
	DeleteProfile(ctx context.Context, id string) error
	// --- skills (agents/skills.go) ---
	ListSkills(ctx context.Context) ([]store.Skill, error)
	// CreateSkill: ErrInvalid on a bad name or empty body, ErrNameTaken on a clash.
	CreateSkill(ctx context.Context, in SkillInput) (*store.Skill, error)
	UpdateSkill(ctx context.Context, id string, in SkillUpdate) (*store.Skill, error)
	DeleteSkill(ctx context.Context, id string) error
	// CreateChat is the primary chat creation (docs/CHAT_MODEL_API.md): it creates
	// the chat and its 1:1 execution record together.
	CreateChat(ctx context.Context, in CreateChatInput) (*store.Chat, error)
	// UpdateChat rebinds the chat's profile / own system prompt / client (the
	// chat's execution record is changed, the mirrored chat fields follow).
	UpdateChat(ctx context.Context, id string, in ChatUpdate) (*store.Chat, error)
	// DeleteChat permanently deletes the chat, its child chats (parentChatId) and
	// the execution records nothing else uses, cancelling their runs. It returns
	// how many chats were deleted.
	DeleteChat(ctx context.Context, id string) (int, error)
	// HubTools lists every switchboard.* tool the hub provides.
	HubTools() []HubTool
	// ChatTools is what the chat's agent can call right now, computed by the
	// run loop's own catalog function.
	ChatTools(ctx context.Context, chatID string) (*ChatToolsView, error)
	// ChatSystemPrompt is the exact system prompt the run loop sends for the
	// chat's agent next turn (GET /api/chats/{id}/system-prompt).
	ChatSystemPrompt(ctx context.Context, chatID string) (*SystemPromptView, error)

	// Models is what /api/models serves: configured providers and models with
	// prices, never keys.
	Models() []llm.ModelSpec
	// Stats adds live counters (active runs, subscribers) to /api/stats.
	ActiveRuns() int
}

type CreateAgentInput struct {
	ParentID     string // "" for a root agent created from the console
	Name         string
	Description  string
	Project      string
	Model        json.RawMessage // {provider, model, ...}; defaults to first configured model
	SystemPrompt string
	Budget       json.RawMessage // merged over the hub defaults
	Capabilities store.Capabilities
	Approval     string // "", never|destructive|always; "" derives per W1 at creation
	AutoWake     *bool
	// Grants requested. Root agents: defaults to AGENT_DEFAULT_GRANTS. For a
	// child (ParentID set) they are intersected with the parent's (A12).
	Grants []store.Grant
	// ParentChatID (children only) is the chat the child chat nests under (the
	// parent's most recently active chat when empty); for a spawn it is also the
	// chat whose client label and profile the child's chat records.
	ParentChatID string
	// ChatTitle is the title of the chat created with the record.
	ChatTitle string

	// ProfileID references a profile (live, see resolveAgent); "" is none.
	ProfileID string
	// ClientSet is true when the request carried a clientLabel key (even null):
	// a manual agent whose grants are exactly {ClientLabel,*,*} (none when
	// ClientLabel is nil), without AGENT_DEFAULT_GRANTS. Grants is ignored.
	ClientSet   bool
	ClientLabel *string

	// origin overrides the derived Origin; withChat makes the record's chat even
	// for a ClientSet record (which otherwise gets none).
	origin   string
	withChat bool
}

type PostInput struct {
	Content        []llm.Block
	ParentID       string // optional: post under this message instead of the leaf
	Model          json.RawMessage
	IdempotencyKey string
}

type BranchInput struct {
	FromMessageID string
	Content       []llm.Block
	Model         json.RawMessage
}

type PostResult struct {
	MessageID    string `json:"messageId"`
	RunID        string `json:"runId"`
	Deduplicated bool   `json:"deduplicated,omitempty"`
}

type PendingApproval struct {
	CallID    string         `json:"callId"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	// ExpiresAt is RFC 3339 (store.FormatTime): when W3 auto-denies.
	ExpiresAt string `json:"expiresAt"`
	// Questions is set instead of asking for approval when the model asked the
	// user something (switchboard.user.ask); Answer resolves it.
	Questions []Question `json:"questions,omitempty"`
}

// PendingApprovalDetail is one GET /api/approvals element.
type PendingApprovalDetail struct {
	RunID     string `json:"runId"`
	ChatID    string `json:"chatId"`
	AgentID   string `json:"agentId"`
	AgentName string `json:"agentName"`
	ChatTitle string `json:"chatTitle"`
	PendingApproval
}

// GraphView is the /api/graph payload (camelCase JSON).
type GraphView struct {
	Agents  []GraphAgent  `json:"agents"`
	Edges   []store.Edge  `json:"edges"`
	Grants  []GraphGrant  `json:"grants"`
	Servers []GraphServer `json:"servers"` // every live server, for the permission panel
}

type GraphAgent struct {
	store.Agent
	UnreadMail int `json:"unreadMail"`
	// CurrentTool is the latest tool call of a streaming run, if any (U14).
	CurrentTool string `json:"currentTool,omitempty"`
}

type GraphGrant struct {
	store.Grant
	Connected bool `json:"connected"` // matches a live server now (I3, A15)
	// Orphaned marks a human grant whose parent no longer holds it (A14).
	Orphaned bool `json:"orphaned,omitempty"`
}

type GraphServer struct {
	Label     string `json:"label"`
	Project   string `json:"project"`
	Server    string `json:"server"`
	Connected bool   `json:"connected"`
	ToolCount int    `json:"toolCount"`
}

// Streamer is what the run loop publishes per-chat stream frames through
// (spec 7.4). internal/chatstream.Hub implements it. Deltas never touch the
// global event bus (N2). Publish assigns the per-run monotonic seq itself.
type Streamer interface {
	// Publish appends a frame of the given N6 type ("run_started", "delta",
	// "tool_call", "tool_result", "approval_required", "message_done",
	// "run_done", "error") with data JSON-marshalled by the hub.
	Publish(chatID, runID, frameType string, data any)
	// EndRun tells the hub no more frames will come for the run (the ring
	// buffer may then be released after a grace period).
	EndRun(chatID, runID string)
}

// ProfileInput creates a profile. Name and SystemPrompt are required.
type ProfileInput struct {
	Name         string
	Description  string
	SystemPrompt string
	Model        json.RawMessage // nil/"null": first configured model
	Capabilities store.Capabilities
	Approval     string // "" means destructive
	Budget       json.RawMessage
}

// ProfileUpdate is a partial profile update; nil fields are left alone.
// Model "null" clears the model; CanSpawn/CanMessage merge into the current
// capabilities.
type ProfileUpdate struct {
	Name         *string
	Description  *string
	SystemPrompt *string
	Model        json.RawMessage
	CanSpawn     *bool
	CanMessage   *bool
	Approval     *string
	Budget       json.RawMessage
	IsDefault    *bool
}

// SkillInput creates a skill. Name (a slug) and Body are required.
type SkillInput struct {
	Name        string
	Description string
	Body        string
	Auto        bool
}

// SkillUpdate is a partial skill update; nil fields are left alone.
type SkillUpdate struct {
	Name        *string
	Description *string
	Body        *string
	Auto        *bool
}

// CreateChatInput is POST /api/chats' primary form. Prompt source: ProfileID
// set = a live profile reference; ProfileNone (profileId: null was sent) or a
// non-nil SystemPrompt with no ProfileID = the chat's own prompt (possibly empty);
// none of them = the default profile. ClientLabel "" is no client, "*" is
// ErrInvalid. Title "" becomes "New chat". ParentChatID makes a child chat.
type CreateChatInput struct {
	Title        string
	ProfileID    string
	ProfileNone  bool
	SystemPrompt *string
	ClientLabel  string
	Model        json.RawMessage // optional {provider, model}
	ParentChatID string
}

// ChatUpdate is the rebinding part of PATCH /api/chats/{id}. ProfileSet with a
// nil ProfileID detaches the profile (the chat keeps the values it last had
// unless SystemPrompt gives new own text); SystemPrompt is only used while no
// profile is referenced. ClientSet with a nil ClientLabel means no client.
type ChatUpdate struct {
	ProfileSet   bool
	ProfileID    *string
	SystemPrompt *string
	ClientSet    bool
	ClientLabel  *string
}

// ToolAnnotations is the MCP hint set as the API shows it.
type ToolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

// HubTool is one switchboard.* tool (GET /api/hub-tools).
type HubTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema map[string]any  `json:"inputSchema"`
	Annotations ToolAnnotations `json:"annotations"`
	// Requires is "canSpawn", "canMessage", "canSpawn or canMessage" or "always" (M1).
	Requires string `json:"requires"`
}

// ToolServer names the MCP server a ChatTool comes from.
type ToolServer struct {
	Label   string `json:"label"`
	Project string `json:"project"`
	Server  string `json:"server"`
}

// ChatTool is one tool in a chat's live catalog (GET /api/chats/{id}/tools).
type ChatTool struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Origin      string           `json:"origin"` // "mcp" | "hub"
	Server      *ToolServer      `json:"server,omitempty"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
	InputSchema map[string]any   `json:"inputSchema,omitempty"`
}

// ChatToolsView is the GET /api/chats/{id}/tools payload.
type ChatToolsView struct {
	ClientLabel     *string    `json:"clientLabel"`
	ClientConnected bool       `json:"clientConnected"`
	Tools           []ChatTool `json:"tools"`
}

// ModelRef is {provider, model} as the system-prompt endpoint shows it.
type ModelRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// SystemPromptView is the GET /api/chats/{id}/system-prompt payload.
type SystemPromptView struct {
	SystemPrompt string    `json:"systemPrompt"`
	Source       string    `json:"source"` // "profile" | "agent" | "none"
	ProfileID    *string   `json:"profileId"`
	ProfileName  *string   `json:"profileName"`
	Model        *ModelRef `json:"model"`
	// ModelIsDefault is true when Model is the hub's first configured model
	// (what a turn falls back to when nothing more specific was chosen), not
	// necessarily because it was picked for that reason - just that it matches.
	ModelIsDefault bool `json:"modelIsDefault"`
	ToolCount      int  `json:"toolCount"`
}
