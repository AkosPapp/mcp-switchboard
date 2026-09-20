// Package calls is the one path every tool call takes.
//
// Console, MCP consumer, REST and - once the orchestrator lands - agent all
// dispatch through Dispatcher.Call, so the call log, the metrics, the Loki
// export and the timeout are written once and apply to all of them by
// construction rather than by four callers remembering to do the same thing
// (spec.md H8, R1).
package calls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// ErrNotConnected is returned when the named server has no live session. It is
// deliberately distinguishable from a tool error: the tool did not fail, it was
// never reached, and for an agent that difference decides whether retrying
// elsewhere makes sense (spec.md A15).
var ErrNotConnected = errors.New("server not connected")

// Metrics is the slice of the metrics surface a dispatch touches.
type Metrics interface {
	ObserveCall(label, server, tool, status, source string, durationSeconds float64)
}

// Exporter is the slice of the Loki surface a dispatch touches. It must never
// block or fail a call, which is the exporter's own contract.
type Exporter interface {
	Emit(event string, fields map[string]any, labels map[string]string)
}

// Request is one tool call, already resolved to a concrete server.
//
// Resolution happens in the caller because each surface resolves differently -
// a consumer by composed name and scope, the console by connection id - but
// everything after resolution is identical, and that is what lives here.
type Request struct {
	Connection *registry.Connection
	Channel    *registry.ServerChannel

	// Tool is the upstream name; ExposedName is what the caller asked for,
	// which differs by scope and is what the call log shows.
	Tool        string
	ExposedName string
	Arguments   map[string]any

	Source string

	// Set only for a call made by a run (R1); nil for every human-driven call.
	AgentID *string
	ChatID  *string
	RunID   *string
}

// Result is what the tool answered, plus the row that was written about it.
type Result struct {
	CallID string
	Result *mcp.CallToolResult

	// IsError mirrors the MCP result: a tool that answers with an error is a
	// successful call that returned an error, not a transport failure (N1).
	IsError bool

	// Error is set when the call could not be made at all.
	Error string
}

// Dispatcher owns the shared instrumentation.
type Dispatcher struct {
	store   store.CallStore
	bus     *events.Bus
	metrics Metrics
	loki    Exporter
	timeout time.Duration
	log     *slog.Logger
}

// Options configure a Dispatcher. Everything but the store is optional.
type Options struct {
	Store   store.CallStore
	Bus     *events.Bus
	Metrics Metrics
	Loki    Exporter
	Timeout time.Duration
	Logger  *slog.Logger
}

const defaultTimeout = 120 * time.Second

func NewDispatcher(opts Options) *Dispatcher {
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Dispatcher{
		store:   opts.Store,
		bus:     opts.Bus,
		metrics: opts.Metrics,
		loki:    opts.Loki,
		timeout: opts.Timeout,
		log:     logger,
	}
}

// Call invokes one tool and records everything about it.
//
// The returned error is reserved for "the call could not be made" - an unknown
// or disconnected server. A tool that ran and failed comes back as a Result
// with IsError set, because folding the two together would make the console
// report transport failures as tool bugs and vice versa (N1).
func (d *Dispatcher) Call(ctx context.Context, req Request) (*Result, error) {
	record := store.CallRecord{
		ID:           uuid.New().String(),
		ConnectionID: req.Connection.ID,
		Label:        req.Connection.Label,
		Server:       req.Channel.Name,
		Tool:         req.Tool,
		ExposedName:  req.ExposedName,
		Arguments:    req.Arguments,
		Status:       store.StatusOK,
		Source:       req.Source,
		StartedAt:    time.Now().UTC(),
		AgentID:      req.AgentID,
		ChatID:       req.ChatID,
		RunID:        req.RunID,
	}
	if record.Arguments == nil {
		record.Arguments = map[string]any{}
	}

	session, ok := req.Channel.Session().(*mcp.ClientSession)
	if !ok || session == nil {
		record.Status = store.StatusError
		record.Error = ErrNotConnected.Error()
		d.finish(ctx, &record)
		return &Result{CallID: record.ID, Error: record.Error}, ErrNotConnected
	}

	// The call's own deadline, independent of how long the caller is willing to
	// wait: cancelling the caller still cancels this through ctx (G2), but a
	// caller that waits forever must not be able to hold a channel forever.
	callCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	started := time.Now()
	result, err := session.CallTool(callCtx, &mcp.CallToolParams{
		Name:      req.Tool,
		Arguments: req.Arguments,
	})
	record.DurationMs = float64(time.Since(started).Microseconds()) / 1000

	switch {
	case err != nil:
		record.Status = store.StatusError
		record.Error = err.Error()
		d.finish(ctx, &record)
		return &Result{CallID: record.ID, Error: record.Error, IsError: true}, nil
	case result.IsError:
		record.Status = store.StatusError
		record.Error = firstText(result)
		record.Result = resultToMap(result)
		d.finish(ctx, &record)
		return &Result{CallID: record.ID, Result: result, IsError: true, Error: record.Error}, nil
	default:
		record.Result = resultToMap(result)
		d.finish(ctx, &record)
		return &Result{CallID: record.ID, Result: result}, nil
	}
}

// Deny records a call an agent was not permitted to make. It is a first-class
// outcome rather than an error: the model gets a tool result it can adapt to,
// and the row is visible in the Calls view with status "denied" (spec.md R2).
func (d *Dispatcher) Deny(ctx context.Context, req Request, reason string) *Result {
	record := store.CallRecord{
		ID:           uuid.New().String(),
		ConnectionID: connectionID(req),
		Label:        label(req),
		Server:       server(req),
		Tool:         req.Tool,
		ExposedName:  req.ExposedName,
		Arguments:    req.Arguments,
		Status:       store.StatusDenied,
		Error:        reason,
		Source:       req.Source,
		StartedAt:    time.Now().UTC(),
		AgentID:      req.AgentID,
		ChatID:       req.ChatID,
		RunID:        req.RunID,
	}
	if record.Arguments == nil {
		record.Arguments = map[string]any{}
	}
	d.finish(ctx, &record)
	return &Result{CallID: record.ID, IsError: true, Error: reason}
}

// finish writes the row, the metric and the log line.
//
// It takes the caller's context only to pass on; a cancelled call must still be
// recorded, or the one call anyone wants to read about afterwards is the one
// that is missing. Hence the detached context for the store write.
func (d *Dispatcher) finish(ctx context.Context, record *store.CallRecord) {
	if d.metrics != nil {
		d.metrics.ObserveCall(
			record.Label, record.Server, record.Tool,
			record.Status, record.Source, record.DurationMs/1000,
		)
	}

	if d.store != nil {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := d.store.RecordCall(writeCtx, *record); err != nil {
			// History must never fail a call that already happened.
			d.log.Warn("could not record a call", "call", record.ID, "error", err)
		}
	}

	if d.loki != nil {
		d.loki.Emit("tool_call", map[string]any{
			"callId":   record.ID,
			"tool":     record.Tool,
			"status":   record.Status,
			"error":    record.Error,
			"duration": record.DurationMs,
			"agentId":  deref(record.AgentID),
			"runId":    deref(record.RunID),
		}, map[string]string{
			"label":  record.Label,
			"server": record.Server,
			"source": record.Source,
		})
	}

	if d.bus != nil {
		d.bus.Publish(events.Event{Type: events.TypeCall, CallID: record.ID})
	}
}

// resultToMap normalises an MCP result into the generic map the call log
// stores. A result that will not round-trip through JSON is recorded as a note
// rather than dropped, so the row still says something happened.
func resultToMap(result *mcp.CallToolResult) map[string]any {
	if result == nil {
		return nil
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return map[string]any{"unserializable": fmt.Sprintf("%T", result)}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{"unserializable": string(raw)}
	}
	return out
}

// firstText pulls a human-readable message out of an error result, which is
// what the Calls view shows in its error column.
func firstText(result *mcp.CallToolResult) string {
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok && text.Text != "" {
			return text.Text
		}
	}
	return "the tool reported an error"
}

func connectionID(req Request) string {
	if req.Connection == nil {
		return ""
	}
	return req.Connection.ID
}

func label(req Request) string {
	if req.Connection == nil {
		return ""
	}
	return req.Connection.Label
}

func server(req Request) string {
	if req.Channel == nil {
		return ""
	}
	return req.Channel.Name
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
