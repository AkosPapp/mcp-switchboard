// Package store is the hub's durable state: every tool call that passes through
// the hub, and - as later migrations land - the orchestrator's agents, chats and
// runs. It knows nothing about tunnels, registries or MCP; callers hand it
// records and it persists them.
//
// SQLite is the only implementation, but everything goes through the interfaces
// below (spec.md D1) so a second one can be added without touching call sites.
// The schema itself lives in migrations/ and nowhere else (D5).
package store

import (
	"context"
	"encoding/json"
	"time"
)

// Call status values. A tool that answers with an MCP error is a successful
// call that returned an error (N1), so StatusError describes the tool's answer,
// never the transport.
const (
	StatusOK = "ok"
	// StatusError is a call the tool itself failed.
	StatusError = "error"
	// StatusDenied is a call an agent was not permitted to make (R2). It is a
	// tool result the model can adapt to, not an error raised to anyone, and it
	// is a separate status so "the agent tried something it may not do" never
	// reads as "the tool is broken".
	StatusDenied = "denied"
)

// Where a call came from. Agent calls go down the identical dispatch path as
// every other call (R1); only the source and the agent/chat/run columns differ.
const (
	SourceConsole = "console"
	SourceMCP     = "mcp"
	SourceAPI     = "api"
	SourceAgent   = "agent"
)

// MaxLimit clamps ListCalls. A console page never wants more than a screenful;
// this stops an API caller from asking for the whole table in one query.
const MaxLimit = 1000

// CallRecord is one tool call, start to finish.
type CallRecord struct {
	ID           string
	ConnectionID string
	Label        string
	Server       string
	Tool         string
	ExposedName  string
	Arguments    map[string]any
	Result       map[string]any
	// Error is the tool's error message, empty when there was none. There is no
	// useful difference between an absent error and an empty one, so the pair
	// collapses to "" here and to null on the wire.
	Error      string
	Status     string
	Source     string
	StartedAt  time.Time
	DurationMs float64

	// Nil for anything that did not come from a run (R1). Pointers rather than
	// sql.NullString: they are carried through the API layer as well as the
	// store, and a nil check reads better at every one of those call sites than
	// a two-field struct does.
	AgentID *string
	ChatID  *string
	RunID   *string
}

// CallFilter selects rows for ListCalls. A zero-valued field is "no filter";
// a filter naming something that does not exist simply matches nothing.
type CallFilter struct {
	Server  string
	Tool    string
	Status  string
	Label   string
	Source  string
	AgentID string
	ChatID  string

	// Limit is clamped to MaxLimit; zero means DefaultLimit.
	Limit  int
	Offset int
}

// DefaultLimit is what ListCalls uses when a filter asks for no particular size.
const DefaultLimit = 100

// CallStats are the counts the console header shows.
type CallStats struct {
	Total int64 `json:"total"`
	OK    int64 `json:"ok"`
	Error int64 `json:"error"`
}

// Stats is what /api/stats serves. Chat growth is unbounded by design (D11),
// which is a deployment fact rather than a bug - so the size on disk and the
// per-table row counts are reported continuously, and become visible long
// before they become a problem.
type Stats struct {
	Calls CallStats `json:"calls"`
	// DatabaseBytes counts the main file plus its -wal and -shm sidecars: the
	// WAL can be a large fraction of the total between checkpoints, and a
	// number that ignored it would understate what the volume actually holds.
	DatabaseBytes int64            `json:"databaseBytes"`
	Tables        map[string]int64 `json:"tables"`
}

// CallStore is the durable history of tool calls. Every method takes a context
// (D2) because every one of them can be reached from a cancellable request.
type CallStore interface {
	RecordCall(ctx context.Context, rec CallRecord) error
	ListCalls(ctx context.Context, f CallFilter) ([]CallRecord, error)
	// GetCall returns nil, nil when there is no such call.
	GetCall(ctx context.Context, id string) (*CallRecord, error)
	// PurgeCalls applies both retention rules and returns the rows deleted.
	PurgeCalls(ctx context.Context) (int64, error)
	CallStats(ctx context.Context) (CallStats, error)
}

// Store is the whole of persistence. It grows by composition as the
// orchestrator's sub-interfaces land, which is why the call methods carry a
// "Call" in their names: two sub-interfaces cannot both contribute a List.
type Store interface {
	CallStore

	Stats(ctx context.Context) (Stats, error)
	Close() error
}

// callJSON is the camelCase payload the HTTP API serves verbatim. It exists as
// a separate type so the field order and the null-vs-absent decisions are
// stated once, here, instead of being an emergent property of struct tags.
type callJSON struct {
	ID           string         `json:"id"`
	ConnectionID string         `json:"connectionId"`
	Label        string         `json:"label"`
	Server       string         `json:"server"`
	Tool         string         `json:"tool"`
	ExposedName  string         `json:"exposedName"`
	Arguments    map[string]any `json:"arguments"`
	Result       map[string]any `json:"result"`
	Error        *string        `json:"error"`
	Status       string         `json:"status"`
	Source       string         `json:"source"`
	StartedAt    string         `json:"startedAt"`
	DurationMs   float64        `json:"durationMs"`
	AgentID      *string        `json:"agentId,omitempty"`
	ChatID       *string        `json:"chatId,omitempty"`
	RunID        *string        `json:"runId,omitempty"`
}

// MarshalJSON keeps the payload identical to the Python hub's to_json(), field
// for field, because the console and the API tests both consume it.
func (r CallRecord) MarshalJSON() ([]byte, error) {
	args := r.Arguments
	if args == nil {
		args = map[string]any{}
	}
	var errMsg *string
	if r.Error != "" {
		errMsg = &r.Error
	}
	return json.Marshal(callJSON{
		ID:           r.ID,
		ConnectionID: r.ConnectionID,
		Label:        r.Label,
		Server:       r.Server,
		Tool:         r.Tool,
		ExposedName:  r.ExposedName,
		Arguments:    args,
		Result:       r.Result,
		Error:        errMsg,
		Status:       r.Status,
		Source:       r.Source,
		StartedAt:    FormatTime(r.StartedAt),
		DurationMs:   r.DurationMs,
		AgentID:      r.AgentID,
		ChatID:       r.ChatID,
		RunID:        r.RunID,
	})
}

// FormatTime renders an instant the way the whole API renders instants: RFC 3339
// in UTC with an explicit "+00:00" offset rather than a trailing "Z".
// JavaScript's Date parses both, but Python's datetime.fromisoformat - which the
// client and the end-to-end tests use to round-trip these - rejects "Z" on 3.10.
// The fraction is six digits or none, matching what the Python hub wrote, so
// timestamps compare equal across the cutover.
func FormatTime(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05-07:00")
	}
	return t.Format("2006-01-02T15:04:05.000000-07:00")
}

// parseTime reads back what FormatTime wrote, and anything else RFC 3339 shaped
// that an older row might hold.
func parseTime(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999999-07:00",
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
