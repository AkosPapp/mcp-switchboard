package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Errors callers are expected to branch on.
var (
	// ErrNotFound: the row (or a row it must reference) does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrNameTaken: a live sibling already has this agent name (5.2).
	ErrNameTaken = errors.New("store: agent name already taken among its siblings")
	// ErrInvalid: the request is structurally wrong (e.g. a message whose
	// parent belongs to another chat).
	ErrInvalid = errors.New("store: invalid request")
)

// NewID returns a UUIDv7 as 32 hex characters, no dashes. Version 7 ids sort by
// creation time, which keeps b-tree inserts append-mostly.
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		// NewV7 only fails when the system random source does.
		panic("store: no randomness available: " + err.Error())
	}
	return hex.EncodeToString(id[:])
}

// Enumerations. Stored as free text (like calls.status), validated by callers.
const (
	AgentIdle    = "idle"
	AgentRunning = "running"
	AgentWaiting = "waiting"
	AgentBlocked = "blocked"
	AgentDone    = "done"
	AgentError   = "error"

	ApprovalNever       = "never"
	ApprovalDestructive = "destructive"
	ApprovalAlways      = "always"

	ChatKindHuman = "human"
	ChatKindAgent = "agent"
	ChatKindSpawn = "spawn"

	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"

	GrantInherited = "inherited"
	GrantExplicit  = "explicit"
	GrantHuman     = "human"

	RunQueued      = "queued"
	RunRunning     = "running"
	RunWaiting     = "waiting"
	RunDone        = "done"
	RunCancelled   = "cancelled"
	RunInterrupted = "interrupted"
	RunError       = "error"

	TriggerHuman        = "human"
	TriggerAgentMessage = "agent_message"
	TriggerSpawn        = "spawn"
)

// Wildcard is the grant field that matches anything (A11).
const Wildcard = "*"

// Markers wrapped around matched terms in SearchHit.Snippet. Control characters
// rather than HTML so a snippet of arbitrary message text is never markup.
const (
	SnippetStart = "\x02"
	SnippetEnd   = "\x03"
)

// Capabilities drive M1. Stored in the column as {can_spawn, can_message}.
type Capabilities struct {
	CanSpawn   bool `json:"canSpawn"`
	CanMessage bool `json:"canMessage"`
}

// Agent is a durable named configuration in the parent-child tree (5.2).
type Agent struct {
	ID          string
	ParentID    *string
	Name        string
	Description string
	Project     *string
	// Model, Budget are opaque JSON owned by the orchestrator (5.7, 5.6).
	Model        json.RawMessage
	SystemPrompt string
	Depth        int
	Status       string
	Budget       json.RawMessage
	Capabilities Capabilities
	Approval     string
	AutoWake     bool
	// TokenTotal and CostTotalMicros are cumulative over self and descendants (B5).
	TokenTotal      int64
	CostTotalMicros int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	LastActivityAt  time.Time
	DeletedAt       *time.Time
	// ProfileID is the profile the agent was created from; nil for legacy
	// agents, spawned children of legacy agents and after the profile is deleted.
	ProfileID *string
	// Origin is how the agent came to be (AgentOrigin*); ClientLabel the one
	// MCP client it is bound to, nil for none (docs/AGENT_MODEL_API.md).
	Origin      string
	ClientLabel *string
}

// Agent origins.
const (
	OriginManual = "manual" // created by hand in the Chat panel
	OriginSpawn  = "spawn"  // created by another agent (or as a child)
	OriginChat   = "chat"   // the per-chat root agent of the legacy POST /api/chats
)

type agentJSON struct {
	ID              string          `json:"id"`
	ParentID        *string         `json:"parentId"`
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Project         *string         `json:"project"`
	Model           json.RawMessage `json:"model"`
	SystemPrompt    string          `json:"systemPrompt"`
	Depth           int             `json:"depth"`
	Status          string          `json:"status"`
	Budget          json.RawMessage `json:"budget"`
	Capabilities    Capabilities    `json:"capabilities"`
	Approval        string          `json:"approval"`
	AutoWake        bool            `json:"autoWake"`
	TokenTotal      int64           `json:"tokenTotal"`
	CostTotalMicros int64           `json:"costTotalMicros"`
	CreatedAt       string          `json:"createdAt"`
	UpdatedAt       string          `json:"updatedAt"`
	LastActivityAt  string          `json:"lastActivityAt"`
	DeletedAt       *string         `json:"deletedAt"`
	ProfileID       *string         `json:"profileId"`
	Origin          string          `json:"origin"`
	ClientLabel     *string         `json:"clientLabel"`
}

func (a Agent) MarshalJSON() ([]byte, error) {
	return json.Marshal(agentJSON{
		ID: a.ID, ParentID: a.ParentID, Name: a.Name, Description: a.Description,
		Project: a.Project, Model: rawOr(a.Model, "{}"), SystemPrompt: a.SystemPrompt,
		Depth: a.Depth, Status: a.Status, Budget: rawOr(a.Budget, "{}"),
		Capabilities: a.Capabilities, Approval: a.Approval, AutoWake: a.AutoWake,
		TokenTotal: a.TokenTotal, CostTotalMicros: a.CostTotalMicros,
		CreatedAt: FormatTime(a.CreatedAt), UpdatedAt: FormatTime(a.UpdatedAt),
		LastActivityAt: FormatTime(a.LastActivityAt), DeletedAt: fmtTimePtr(a.DeletedAt),
		ProfileID: a.ProfileID, Origin: originOr(a.Origin), ClientLabel: a.ClientLabel,
	})
}

func originOr(o string) string {
	if o == "" {
		return OriginChat
	}
	return o
}

// AgentFilter selects agents for ListAgents. Zero value: every live agent.
type AgentFilter struct {
	// ParentID limits to direct children of that agent.
	ParentID string
	// RootsOnly limits to agents with no parent. Ignored when ParentID is set.
	RootsOnly      bool
	Project        string
	IncludeDeleted bool
}

// AgentPatch changes an agent's configuration. Nil fields are left alone.
// Project "" clears the project. Status and totals have their own methods.
type AgentPatch struct {
	Name         *string
	Description  *string
	Project      *string
	Model        json.RawMessage
	SystemPrompt *string
	Budget       json.RawMessage
	Capabilities *Capabilities
	Approval     *string
	AutoWake     *bool
	// SetProfile changes profile_id to ProfileID (nil clears it).
	SetProfile bool
	ProfileID  *string
	// SetClient rebinds the agent to ClientLabel (nil: none) with the grant
	// rewrite and A14 propagation of SetAgentClient, in the same transaction.
	SetClient   bool
	ClientLabel *string
}

// AgentStore persists the agent tree.
type AgentStore interface {
	// CreateAgent inserts a child (or a root when ParentID is nil), computing
	// Depth from the parent, and in the same transaction writes the initial
	// grants and - for a child - the allowed parent->child edge (A8). An ID is
	// generated when empty. ErrNameTaken on a live sibling with the same name;
	// ErrNotFound when the parent is missing or soft-deleted.
	CreateAgent(ctx context.Context, a Agent, grants []Grant) (Agent, error)
	// GetAgent returns nil, nil when absent. Soft-deleted agents are returned.
	GetAgent(ctx context.Context, id string) (*Agent, error)
	// GetAgentByName finds a live agent by (parent, name); parentID "" means a root.
	GetAgentByName(ctx context.Context, parentID, name string) (*Agent, error)
	ListAgents(ctx context.Context, f AgentFilter) ([]Agent, error)
	// CountLiveChildren counts non-deleted direct children (AGENT_MAX_CHILDREN).
	CountLiveChildren(ctx context.Context, parentID string) (int, error)
	UpdateAgent(ctx context.Context, id string, p AgentPatch) (Agent, error)
	// SetAgentClient rebinds the agent to exactly one client (label nil: none)
	// in one transaction: its grants become exactly {label,*,*} (source
	// explicit) or nothing, agents.client_label follows, and everything the
	// change narrows is propagated below (A14): descendants' inherited allow
	// rows for any other label are deleted, and descendants whose client_label
	// is no longer served are cleared. Returns the descendant grants revoked.
	SetAgentClient(ctx context.Context, id string, label *string) ([]Grant, error)
	// SetAgentStatus writes the derived status and bumps last_activity_at.
	SetAgentStatus(ctx context.Context, id, status string) error
	// Ancestors returns the ids above id, nearest parent first.
	Ancestors(ctx context.Context, id string) ([]string, error)
	// Descendants returns every id below id (children, grandchildren, ...),
	// including soft-deleted ones, in no particular order.
	Descendants(ctx context.Context, id string) ([]string, error)
	// AddUsage adds tokens and cost to the agent and every ancestor in one
	// statement (B5). Prefer AppendMessageWithUsage when persisting a message.
	AddUsage(ctx context.Context, agentID string, tokens, costMicros int64) error
	// LifetimeExceeded reports whether the agent or any ancestor has
	// cost_total_micros >= limitMicros, returning the first offending agent's
	// id. A limit <= 0 disables the check. (B5)
	LifetimeExceeded(ctx context.Context, agentID string, limitMicros int64) (offender string, exceeded bool, err error)
	// SoftDeleteAgent marks the agent and all its live descendants deleted and
	// their queued/running/waiting runs cancelled, in one transaction. It
	// returns the descendants that were newly marked (not the agent itself), so
	// the caller can cancel their in-memory runs. ErrNotFound if already deleted.
	SoftDeleteAgent(ctx context.Context, id string) (descendants []string, err error)
	// HardDeleteAgent removes the agent, its descendants and everything they own
	// (chats, messages, edges, grants, runs, inbox, and their calls rows). It
	// returns every agent id removed, including id.
	HardDeleteAgent(ctx context.Context, id string) (removed []string, err error)
}

// Chat is a durable conversation; its agent row is the chat's internal execution record (5.2).
type Chat struct {
	ID              string
	AgentID         string
	PeerAgentID     *string
	Title           string
	Kind            string
	ActiveLeafID    *string
	Tags            []string
	TokenTotal      int64
	CostTotalMicros int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
	ArchivedAt      *time.Time
	// ProfileID / ClientLabel: the profile the chat was created from and the one
	// MCP client it may use (PROFILES_API); nil for legacy chats.
	ProfileID   *string
	ClientLabel *string
	// ParentChatID is the chat that spawned this one (docs/CHAT_MODEL_API.md);
	// nil for a top-level chat and for legacy chats it could not be inferred for.
	ParentChatID *string
}

type chatJSON struct {
	ID              string   `json:"id"`
	AgentID         string   `json:"agentId"`
	PeerAgentID     *string  `json:"peerAgentId"`
	Title           string   `json:"title"`
	Kind            string   `json:"kind"`
	ActiveLeafID    *string  `json:"activeLeafId"`
	Tags            []string `json:"tags"`
	TokenTotal      int64    `json:"tokenTotal"`
	CostTotalMicros int64    `json:"costTotalMicros"`
	CreatedAt       string   `json:"createdAt"`
	UpdatedAt       string   `json:"updatedAt"`
	ArchivedAt      *string  `json:"archivedAt"`
	ProfileID       *string  `json:"profileId"`
	ClientLabel     *string  `json:"clientLabel"`
	ParentChatID    *string  `json:"parentChatId"`
}

func (c Chat) MarshalJSON() ([]byte, error) {
	tags := c.Tags
	if tags == nil {
		tags = []string{}
	}
	return json.Marshal(chatJSON{
		ID: c.ID, AgentID: c.AgentID, PeerAgentID: c.PeerAgentID, Title: c.Title,
		Kind: c.Kind, ActiveLeafID: c.ActiveLeafID, Tags: tags,
		TokenTotal: c.TokenTotal, CostTotalMicros: c.CostTotalMicros,
		CreatedAt: FormatTime(c.CreatedAt), UpdatedAt: FormatTime(c.UpdatedAt),
		ArchivedAt: fmtTimePtr(c.ArchivedAt), ProfileID: c.ProfileID, ClientLabel: c.ClientLabel,
		ParentChatID: c.ParentChatID,
	})
}

// ChatFilter selects chats for ListChats, most recently updated first.
type ChatFilter struct {
	AgentID         string
	PeerAgentID     string
	ParentChatID    string
	Kind            string
	IncludeArchived bool
	Limit           int // clamped to MaxLimit; zero means DefaultLimit
	Offset          int
}

// ChatPatch edits a chat. Nil fields are left alone.
type ChatPatch struct {
	Title    *string
	Tags     *[]string
	Archived *bool
	// SetProfile / SetClient overwrite the chat's mirrored profile_id /
	// client_label (nil clears). The owning agent is changed by the orchestrator.
	SetProfile  bool
	ProfileID   *string
	SetClient   bool
	ClientLabel *string
}

// ChatDeletion is what DeleteChatCascade removed.
type ChatDeletion struct {
	ChatIDs  []string // the chat and every chat below it (parent_chat_id)
	AgentIDs []string // execution records deleted with them (including their descendants)
}

// Message is one node of a chat's DAG (5.2).
type Message struct {
	ID       string
	ChatID   string
	ParentID *string
	Role     string
	// Content, ToolCalls, ToolResults and Model are provider-neutral JSON owned
	// by the orchestrator; nil ToolCalls/ToolResults/Model are stored as NULL.
	Content     json.RawMessage
	ToolCalls   json.RawMessage
	ToolResults json.RawMessage
	TokenInput  int64
	TokenOutput int64
	CostMicros  int64
	LatencyMs   int64
	// Model is the full turn options actually sent (provider, model,
	// max_tokens, temperature, thinking) - not just {provider, model} - set
	// on the assistant message a turn produced.
	Model json.RawMessage
	// SystemPrompt and Tools are the exact system prompt text and tool
	// definitions (llm.Tool[]) a turn was generated against, set alongside
	// Model on the same assistant message. They are resolved live per turn
	// (a profile or grants can change later), so this is the only durable
	// record of what a *past* turn actually saw - without it, a JSON export
	// cannot reproduce the conversation byte for byte, only what the chat
	// currently resolves to.
	SystemPrompt *string
	Tools        json.RawMessage
	FinishReason *string
	RunID        *string
	// Sender, set on a user-role message injected by another chat, is JSON
	// {chatId, chatTitle, kind}; nil otherwise (docs/CHAT_MODEL_API.md).
	Sender json.RawMessage
	// LastActiveChildID remembers which child was last active below this
	// message, for sibling navigation (A6). Maintained by the store.
	LastActiveChildID *string
	CreatedAt         time.Time
}

type messageJSON struct {
	ID                string          `json:"id"`
	ChatID            string          `json:"chatId"`
	ParentID          *string         `json:"parentId"`
	Role              string          `json:"role"`
	Content           json.RawMessage `json:"content"`
	ToolCalls         json.RawMessage `json:"toolCalls"`
	ToolResults       json.RawMessage `json:"toolResults"`
	TokenInput        int64           `json:"tokenInput"`
	TokenOutput       int64           `json:"tokenOutput"`
	CostMicros        int64           `json:"costMicros"`
	LatencyMs         int64           `json:"latencyMs"`
	Model             json.RawMessage `json:"model"`
	SystemPrompt      *string         `json:"systemPrompt,omitempty"`
	Tools             json.RawMessage `json:"tools,omitempty"`
	FinishReason      *string         `json:"finishReason"`
	RunID             *string         `json:"runId"`
	Sender            json.RawMessage `json:"sender"`
	LastActiveChildID *string         `json:"lastActiveChildId"`
	CreatedAt         string          `json:"createdAt"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	return json.Marshal(messageJSON{
		ID: m.ID, ChatID: m.ChatID, ParentID: m.ParentID, Role: m.Role,
		Content: rawOr(m.Content, "[]"), ToolCalls: rawOr(m.ToolCalls, "null"),
		ToolResults: rawOr(m.ToolResults, "null"),
		TokenInput:  m.TokenInput, TokenOutput: m.TokenOutput, CostMicros: m.CostMicros,
		LatencyMs: m.LatencyMs, Model: rawOr(m.Model, "null"), SystemPrompt: m.SystemPrompt,
		Tools: m.Tools, FinishReason: m.FinishReason,
		RunID: m.RunID, Sender: rawOr(m.Sender, "null"), LastActiveChildID: m.LastActiveChildID, CreatedAt: FormatTime(m.CreatedAt),
	})
}

// SiblingSet is a message and its siblings (same parent, or all roots of the
// chat for a root message), in creation order, for the "2/3" control (A6).
type SiblingSet struct {
	IDs   []string `json:"ids"`
	Index int      `json:"index"` // position of the asked-about message in IDs
}

// SearchQuery is a chat search (U6). Text is matched as a conjunction of
// prefix-matched words; FTS syntax in it is neutralised.
type SearchQuery struct {
	Text    string
	AgentID string // optional: only chats owned by this agent
	Limit   int    // clamped to MaxLimit; zero means DefaultLimit
}

// SearchHit is one matching message.
type SearchHit struct {
	ChatID    string `json:"chatId"`
	ChatTitle string `json:"chatTitle"`
	AgentID   string `json:"agentId"`
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
	// Snippet has matches wrapped in SnippetStart / SnippetEnd.
	Snippet string `json:"snippet"`
}

// ChatStore persists chats and their message DAGs.
type ChatStore interface {
	// CreateChat inserts a chat; ID generated when empty, Kind defaults to human.
	CreateChat(ctx context.Context, c Chat) (Chat, error)
	// GetChat returns nil, nil when absent.
	GetChat(ctx context.Context, id string) (*Chat, error)
	ListChats(ctx context.Context, f ChatFilter) ([]Chat, error)
	UpdateChat(ctx context.Context, id string, p ChatPatch) (Chat, error)
	// DeleteChat removes the chat, its messages and its runs.
	DeleteChat(ctx context.Context, id string) error
	// DeleteChatCascade deletes the chat and every chat below it (parent_chat_id),
	// then each execution record no remaining chat uses (and that has no live
	// child record outside the deleted set), with its descendants, grants, edges,
	// runs, inbox and call rows, all in one transaction. ErrNotFound when the chat
	// does not exist.
	DeleteChatCascade(ctx context.Context, id string) (ChatDeletion, error)

	// AppendMessage inserts m as a child of m.ParentID (a sibling of any
	// existing children: branching, A4) and in one transaction points the
	// chat's active leaf at it, remembers it as last_active_child_id along the
	// whole path to the root, and adds its tokens and cost to the chat totals.
	AppendMessage(ctx context.Context, m Message) (Message, error)
	// AppendMessageWithUsage is AppendMessage plus AddUsage on the chat's owning
	// agent and every ancestor, all in one transaction (B5).
	AppendMessageWithUsage(ctx context.Context, m Message) (Message, error)
	// GetMessage returns nil, nil when absent.
	GetMessage(ctx context.Context, id string) (*Message, error)
	// ActivePath is the conversation: root ... active leaf, one query (A5).
	// Empty for a chat with no messages.
	ActivePath(ctx context.Context, chatID string) ([]Message, error)
	// PathTo is the same walk from an arbitrary message.
	PathTo(ctx context.Context, messageID string) ([]Message, error)
	Siblings(ctx context.Context, messageID string) (SiblingSet, error)
	// SelectSibling makes messageID the current branch: the chat's active leaf
	// becomes the deepest leaf previously active below it (A6), which is
	// returned. The path's last_active_child_id pointers are updated.
	SelectSibling(ctx context.Context, messageID string) (activeLeafID string, err error)
	// SetActiveLeaf points the chat at an arbitrary message of that chat.
	SetActiveLeaf(ctx context.Context, chatID, messageID string) error
	// SearchMessages runs a full-text search over message text (U6), best first.
	SearchMessages(ctx context.Context, q SearchQuery) ([]SearchHit, error)
}

// Grant is one MCP permission row (A10); Label/Project/Server may be Wildcard.
type Grant struct {
	AgentID   string    `json:"agentId"`
	Label     string    `json:"label"`
	Project   string    `json:"project"`
	Server    string    `json:"server"`
	Allowed   bool      `json:"allowed"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

func (g Grant) MarshalJSON() ([]byte, error) {
	type plain Grant
	return json.Marshal(struct {
		plain
		CreatedAt string `json:"createdAt"`
		UpdatedAt string `json:"updatedAt"`
	}{plain(g), FormatTime(g.CreatedAt), FormatTime(g.UpdatedAt)})
}

// Edge is an agent-to-agent messaging permission (A8). Absent means denied.
// Edges are symmetric (D14): one row stands for the unordered pair, stored
// with the lexicographically smaller id first, and `allowed` governs both
// directions. From/To on a value returned from the store reflect whichever
// order was asked for (or the query's own From/To, for SetEdge), not
// necessarily the row's canonical storage order.
type Edge struct {
	From      string    `json:"fromAgentId"`
	To        string    `json:"toAgentId"`
	Allowed   bool      `json:"allowed"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"-"`
}

func (e Edge) MarshalJSON() ([]byte, error) {
	type plain Edge
	return json.Marshal(struct {
		plain
		CreatedAt string `json:"createdAt"`
		UpdatedAt string `json:"updatedAt"`
	}{plain(e), FormatTime(e.CreatedAt), FormatTime(e.UpdatedAt)})
}

// EdgeFilter selects edges; empty fields match anything.
type EdgeFilter struct {
	From string
	To   string
}

// GrantStore persists grants and edges.
type GrantStore interface {
	// ListGrants returns an agent's own stored grant set (authoritative at call
	// time, A12), ordered by (label, project, server).
	ListGrants(ctx context.Context, agentID string) ([]Grant, error)
	// SetGrant upserts g. When g.Allowed is false, the revocation propagates
	// (A14) in the same transaction: every descendant's `inherited` allow row
	// covered by g's pattern is deleted, transitively. Human and explicit rows
	// survive. Adding an allow affects only that agent. It returns the
	// descendant rows revoked. Times on g are ignored.
	SetGrant(ctx context.Context, g Grant) (revoked []Grant, err error)
	// DeleteGrant removes the row and propagates exactly like a revocation.
	DeleteGrant(ctx context.Context, agentID, label, project, server string) (revoked []Grant, err error)

	// ListEdges is symmetric (D14): a filter naming only From or only To means
	// "every edge touching this id" and normalises each row's From/To to put
	// the named id first; naming both means the exact unordered pair.
	ListEdges(ctx context.Context, f EdgeFilter) ([]Edge, error)
	// SetEdge opens or closes the symmetric edge between from and to (D14);
	// order does not matter, it is the same row and the same permission both
	// ways.
	SetEdge(ctx context.Context, from, to string, allowed bool) (Edge, error)
	DeleteEdge(ctx context.Context, from, to string) error
	// EdgeAllowed is symmetric by construction (D14) and false when the edge
	// is absent or allowed=false.
	EdgeAllowed(ctx context.Context, from, to string) (bool, error)
}

// Run is one execution of an agent against a chat (5.2).
type Run struct {
	ID                 string
	AgentID            string
	ChatID             string
	Trigger            string
	TriggeredByAgentID *string
	Status             string
	BudgetSnapshot     json.RawMessage
	Usage              json.RawMessage
	Error              *string
	FinishReason       *string
	CreatedAt          time.Time // queued at
	StartedAt          *time.Time
	FinishedAt         *time.Time
}

type runJSON struct {
	ID                 string          `json:"id"`
	AgentID            string          `json:"agentId"`
	ChatID             string          `json:"chatId"`
	Trigger            string          `json:"trigger"`
	TriggeredByAgentID *string         `json:"triggeredByAgentId"`
	Status             string          `json:"status"`
	BudgetSnapshot     json.RawMessage `json:"budgetSnapshot"`
	Usage              json.RawMessage `json:"usage"`
	Error              *string         `json:"error"`
	FinishReason       *string         `json:"finishReason"`
	CreatedAt          string          `json:"createdAt"`
	StartedAt          *string         `json:"startedAt"`
	FinishedAt         *string         `json:"finishedAt"`
}

func (r Run) MarshalJSON() ([]byte, error) {
	return json.Marshal(runJSON{
		ID: r.ID, AgentID: r.AgentID, ChatID: r.ChatID, Trigger: r.Trigger,
		TriggeredByAgentID: r.TriggeredByAgentID, Status: r.Status,
		BudgetSnapshot: rawOr(r.BudgetSnapshot, "{}"), Usage: rawOr(r.Usage, "{}"),
		Error: r.Error, FinishReason: r.FinishReason, CreatedAt: FormatTime(r.CreatedAt),
		StartedAt: fmtTimePtr(r.StartedAt), FinishedAt: fmtTimePtr(r.FinishedAt),
	})
}

// RunFilter selects runs, newest first.
type RunFilter struct {
	AgentID string
	ChatID  string
	Status  []string // any of
	Limit   int      // clamped to MaxLimit; zero means DefaultLimit
	Offset  int
}

// RunPatch updates a run. Nil / empty fields are left alone.
type RunPatch struct {
	Status       *string
	Usage        json.RawMessage
	Error        *string
	FinishReason *string
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// InboxItem is one mailbox entry (B1).
type InboxItem struct {
	ID          string     `json:"id"`
	AgentID     string     `json:"agentId"`
	FromAgentID *string    `json:"fromAgentId"`
	ChatID      string     `json:"chatId"`
	MessageID   string     `json:"messageId"`
	CreatedAt   time.Time  `json:"-"`
	DeliveredAt *time.Time `json:"-"`
}

func (i InboxItem) MarshalJSON() ([]byte, error) {
	type plain InboxItem
	return json.Marshal(struct {
		plain
		CreatedAt   string  `json:"createdAt"`
		DeliveredAt *string `json:"deliveredAt"`
	}{plain(i), FormatTime(i.CreatedAt), fmtTimePtr(i.DeliveredAt)})
}

// IdempotencyWindow is how long a key remembers its run (R5).
const IdempotencyWindow = 10 * time.Minute

// RunStore persists runs, the mailbox and idempotency keys.
type RunStore interface {
	// CreateRun inserts a run; ID generated when empty, CreatedAt defaults to now.
	CreateRun(ctx context.Context, r Run) (Run, error)
	// GetRun returns nil, nil when absent.
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)
	UpdateRun(ctx context.Context, id string, p RunPatch) (Run, error)
	// CountRunning counts runs with status running (R7).
	CountRunning(ctx context.Context) (int, error)
	// InterruptRunning is the startup sweep of A3: every queued, running or
	// waiting run becomes 'interrupted' with an explanatory error, and agents
	// left running/waiting/blocked go back to idle. Returns runs interrupted.
	InterruptRunning(ctx context.Context) (int64, error)
	// PurgeRuns deletes finished runs older than RetentionDays that no message
	// references (D9), and expired idempotency keys. Returns runs deleted.
	PurgeRuns(ctx context.Context) (int64, error)

	EnqueueInbox(ctx context.Context, item InboxItem) (InboxItem, error)
	// DrainInbox atomically marks all undelivered items delivered and returns
	// them oldest first.
	DrainInbox(ctx context.Context, agentID string) ([]InboxItem, error)
	// CountInbox counts undelivered items (the Graph badge).
	CountInbox(ctx context.Context, agentID string) (int, error)
	// PruneInbox deletes items delivered more than InboxRetentionDays ago.
	PruneInbox(ctx context.Context) (int64, error)

	// ClaimIdempotencyKey records key -> runID unless the key already exists
	// within IdempotencyWindow, in which case it returns that earlier run id
	// and claimed=false. An expired key is replaced. (R5)
	ClaimIdempotencyKey(ctx context.Context, key, runID string) (existingRunID string, claimed bool, err error)
}

func rawOr(r json.RawMessage, fallback string) json.RawMessage {
	if len(r) == 0 || string(r) == "null" {
		return json.RawMessage(fallback)
	}
	return r
}

func fmtTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := FormatTime(*t)
	return &s
}

// Profile is a reusable template for the working agent of a chat (spec 5.2a).
// It is not an agent instance and carries no grants.
type Profile struct {
	ID           string
	Name         string
	Description  string
	SystemPrompt string
	// Model is {provider, model, ...}; nil means the first configured model.
	Model        json.RawMessage
	Capabilities Capabilities
	Approval     string
	Budget       json.RawMessage
	IsDefault    bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type profileJSON struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	SystemPrompt string          `json:"systemPrompt"`
	Model        json.RawMessage `json:"model"`
	Capabilities Capabilities    `json:"capabilities"`
	Approval     string          `json:"approval"`
	Budget       json.RawMessage `json:"budget"`
	IsDefault    bool            `json:"isDefault"`
	CreatedAt    string          `json:"createdAt"`
	UpdatedAt    string          `json:"updatedAt"`
}

func (p Profile) MarshalJSON() ([]byte, error) {
	return json.Marshal(profileJSON{
		ID: p.ID, Name: p.Name, Description: p.Description, SystemPrompt: p.SystemPrompt,
		Model: rawOr(p.Model, "null"), Capabilities: p.Capabilities, Approval: p.Approval,
		Budget: rawOr(p.Budget, "{}"), IsDefault: p.IsDefault,
		CreatedAt: FormatTime(p.CreatedAt), UpdatedAt: FormatTime(p.UpdatedAt),
	})
}

// ProfilePatch changes a profile. Nil fields are left alone. ClearModel resets
// the model to "first configured"; Model, when non-empty, replaces it.
type ProfilePatch struct {
	Name         *string
	Description  *string
	SystemPrompt *string
	Model        json.RawMessage
	ClearModel   bool
	Capabilities *Capabilities
	Approval     *string
	Budget       json.RawMessage
}

// ProfileStore persists profiles.
type ProfileStore interface {
	// CreateProfile inserts p (ID generated when empty). When p.IsDefault is
	// set the previous default is cleared in the same transaction. ErrNameTaken
	// on a duplicate name.
	CreateProfile(ctx context.Context, p Profile) (Profile, error)
	// GetProfile returns nil, nil when absent.
	GetProfile(ctx context.Context, id string) (*Profile, error)
	// GetDefaultProfile returns nil, nil when no profile is the default.
	GetDefaultProfile(ctx context.Context) (*Profile, error)
	// ListProfiles returns the default first, then by name.
	ListProfiles(ctx context.Context) ([]Profile, error)
	UpdateProfile(ctx context.Context, id string, p ProfilePatch) (Profile, error)
	// SetDefaultProfile makes id the only default, atomically.
	SetDefaultProfile(ctx context.Context, id string) (Profile, error)
	// DeleteProfile removes the profile and nulls profile_id on every chat and
	// agent that referenced it.
	DeleteProfile(ctx context.Context, id string) error
	CountProfiles(ctx context.Context) (int, error)
}

// Draft is the unsent composer text of one chat. UpdatedAt is nil when there
// is no draft.
type Draft struct {
	Content   string
	UpdatedAt *time.Time
}

// DraftStore persists per-chat drafts.
type DraftStore interface {
	// GetDraft returns an empty Draft (nil UpdatedAt) when none is stored.
	GetDraft(ctx context.Context, chatID string) (Draft, error)
	// SetDraft stores content; empty or whitespace-only content deletes the
	// row. ErrNotFound for an unknown chat.
	SetDraft(ctx context.Context, chatID, content string) (Draft, error)
	DeleteDraft(ctx context.Context, chatID string) error
	// AppendMessageClearingDraft is AppendMessage plus deleting the chat's
	// draft in the same transaction. A failing draft delete never fails or
	// rolls back the message.
	AppendMessageClearingDraft(ctx context.Context, m Message) (Message, error)
}
