// Package api is the hub's REST surface: the live registry snapshot, the
// endpoints panel, manual tool calls, the call log, /api/stats and the global
// SSE change feed (spec.md 7.1).
//
// Everything here is a thin shell over the components handed to New. The
// package holds no state of its own, so whatever composes the hub can mount it
// without an initialisation order to get wrong - the same reason the Python
// api.py went through request.app.state.service and nothing else.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/agents"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/chatstream"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// SourceConsole is what a manual call from the console is logged as. A REST
// caller driving this endpoint by hand is doing the same thing the console
// does, so it gets the same source rather than a second name for one path.
const SourceConsole = store.SourceConsole

// Dispatcher is the slice of calls.Dispatcher this package uses. Narrow, for
// the reason calls.Metrics and calls.Exporter are narrow: a handler test wants
// a tool that answers, not a live MCP session to fake.
type Dispatcher interface {
	Call(ctx context.Context, req calls.Request) (*calls.Result, error)
}

// CallReader is the slice of the store the API reads. It never writes: the
// dispatcher owns the call log, and a handler that could also insert rows would
// be a second way for a call to appear in history (H8).
type CallReader interface {
	ListCalls(ctx context.Context, f store.CallFilter) ([]store.CallRecord, error)
	GetCall(ctx context.Context, id string) (*store.CallRecord, error)
	CallStats(ctx context.Context) (store.CallStats, error)
	Stats(ctx context.Context) (store.Stats, error)
}

// PushStore is the slice of the store the push endpoints use (spec.md 8.7).
type PushStore interface {
	UpsertPushSubscription(ctx context.Context, sub store.PushSubscription) (store.PushSubscription, error)
	ListPushSubscriptions(ctx context.Context) ([]store.PushSubscription, error)
	DeletePushSubscription(ctx context.Context, id string) error
}

// Options are the components the handlers serve from. Only Settings has a
// meaningful zero value; the rest are supplied by the hub's wiring.
type Options struct {
	Settings   config.Settings
	Registry   *registry.Registry
	Store      CallReader
	Dispatcher Dispatcher
	Bus        *events.Bus
	Logger     *slog.Logger

	// Agents is the orchestrator. Nil (AGENTS_ENABLED=false) means none of the
	// 7.2 routes are registered, so they 404 (E7).
	Agents agents.Service
	// Streams serves the per-chat SSE stream (7.4). Nil leaves that route off.
	Streams *chatstream.Hub
	// Reader serves the orchestrator's reads. When nil, Store is used if it
	// implements OrchestratorReader (the SQLite store does).
	Reader OrchestratorReader

	// PushStore backs the /api/push/* routes (spec.md 8.7). Nil is fine when
	// push is disabled - those routes are not even registered in that case.
	PushStore PushStore

	// Keepalive overrides the SSE comment interval. It exists so a test does
	// not have to wait fifteen real seconds to see one; production leaves it
	// zero and gets the interval N8 mandates.
	Keepalive time.Duration
}

// Handler serves /api/*. It is an http.Handler so the hub can mount it behind
// whatever middleware the listener needs.
type Handler struct {
	opts Options
	mux  *http.ServeMux
	log  *slog.Logger
	rd   OrchestratorReader
}

// New builds the routing table. The patterns use net/http's own method and
// wildcard syntax; the surface is eight routes and does not earn a router
// dependency.
func New(opts Options) *Handler {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Keepalive <= 0 {
		opts.Keepalive = KeepaliveInterval
	}

	h := &Handler{opts: opts, mux: http.NewServeMux(), log: opts.Logger, rd: opts.Reader}
	if h.rd == nil {
		if rd, ok := opts.Store.(OrchestratorReader); ok {
			h.rd = rd
		}
	}

	h.mux.HandleFunc("GET /api/connections", h.connections)
	h.mux.HandleFunc("GET /api/endpoints", h.endpoints)
	h.mux.HandleFunc("POST /api/connections/{cid}/servers/{server}/tools/{tool}/call", h.callTool)
	h.mux.HandleFunc("POST /api/connections/{cid}/servers/{server}/restart", h.restartServer)
	h.mux.HandleFunc("GET /api/calls", h.listCalls)
	h.mux.HandleFunc("GET /api/calls/{id}", h.getCall)
	h.mux.HandleFunc("GET /api/events", h.events)
	h.mux.HandleFunc("GET /api/stats", h.stats)

	// The orchestrator surface of spec.md 7.2 - 7.4. Per-chat streaming is
	// deliberately not the /api/events mechanism; see N2.
	h.registerOrchestrator()

	// spec.md 8.7: when the hub has no VAPID keys, push is simply disabled -
	// these routes 404 rather than existing and failing, which is why they
	// are only registered here, not answered with a "disabled" body above.
	if opts.Settings.PushEnabled() {
		h.mux.HandleFunc("GET /api/push/vapid-public-key", h.pushVAPIDPublicKey)
		h.mux.HandleFunc("POST /api/push/subscribe", h.pushSubscribe)
		h.mux.HandleFunc("DELETE /api/push/subscriptions/{id}", h.pushUnsubscribe)
	}

	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// --------------------------------------------------------------------------
// registry
// --------------------------------------------------------------------------

func (h *Handler) connections(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.opts.Registry.Snapshot())
}

func (h *Handler) endpoints(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, BuildEndpointsInfo(h.opts.Settings, h.opts.Registry.Snapshot()))
}

// --------------------------------------------------------------------------
// calling
// --------------------------------------------------------------------------

// callRequest is the manual-call body. An absent body is an empty argument
// map, because a no-argument tool should be callable with `curl -X POST`.
type callRequest struct {
	Arguments map[string]any `json:"arguments"`
}

func (h *Handler) callTool(w http.ResponseWriter, r *http.Request) {
	connectionID := r.PathValue("cid")
	serverName := r.PathValue("server")
	toolName := r.PathValue("tool")

	var body callRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	connection := h.opts.Registry.Get(connectionID)
	if connection == nil {
		writeError(w, http.StatusNotFound, "no connection "+quote(connectionID))
		return
	}
	channel := connection.Server(serverName)
	if channel == nil {
		writeError(w, http.StatusNotFound, "no server "+quote(serverName)+" on connection "+quote(connectionID))
		return
	}
	if !hasTool(channel, toolName) {
		writeError(w, http.StatusNotFound, "no tool "+quote(toolName)+" on "+connection.Label+"/"+serverName)
		return
	}

	// The name the call log shows is the one a consumer at /mcp would use, so
	// a row in the Calls view can be matched against a catalog entry by eye.
	exposed, _ := registry.ComposeToolName(registry.ScopeAll, connection.Label, channel.Project, channel.Name, toolName)

	arguments := body.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}

	result, err := h.opts.Dispatcher.Call(r.Context(), calls.Request{
		Connection:  connection,
		Channel:     channel,
		Tool:        toolName,
		ExposedName: exposed,
		Arguments:   arguments,
		Source:      SourceConsole,
	})
	// N1. A tool that answers with an MCP error is a successful call that
	// returned an error: 200 with status "error" in the body. Even a server
	// that is not connected answers 200 here, because the row exists and the
	// console's job is to show what happened to the call - only an unknown
	// connection, server or tool, decided above, is a 404. Folding the two
	// together would make the console report transport failures as tool bugs.
	if result == nil {
		h.log.Error("dispatch returned nothing", "error", err)
		writeError(w, http.StatusInternalServerError, "the call could not be dispatched")
		return
	}

	writeJSON(w, http.StatusOK, h.callBody(r.Context(), result))
}

// callBody is the persisted row for a call that just happened. The store is
// authoritative - it holds the arguments, timing and normalised result the
// console renders - but a row can be missing if the write failed, and a call
// that really ran should still answer with something, hence the fallback.
func (h *Handler) callBody(ctx context.Context, result *calls.Result) any {
	if h.opts.Store != nil {
		if record, err := h.opts.Store.GetCall(ctx, result.CallID); err == nil && record != nil {
			return record
		}
	}
	status := store.StatusOK
	if result.IsError || result.Error != "" {
		status = store.StatusError
	}
	return store.CallRecord{
		ID:        result.CallID,
		Status:    status,
		Error:     result.Error,
		Source:    SourceConsole,
		StartedAt: time.Now().UTC(),
	}
}

func (h *Handler) restartServer(w http.ResponseWriter, r *http.Request) {
	connectionID := r.PathValue("cid")
	serverName := r.PathValue("server")

	connection := h.opts.Registry.Get(connectionID)
	if connection == nil || connection.Server(serverName) == nil {
		writeError(w, http.StatusNotFound, "no server "+quote(serverName)+" on connection "+quote(connectionID))
		return
	}

	// The hub only asks; the client stops and respawns the process and reports
	// back through server_state, which is what H4's restart discipline keys on.
	if err := connection.Send(r.Context(), protocol.Restart(serverName)); err != nil {
		h.log.Warn("could not ask a client to restart a server",
			"connection", connectionID, "server", serverName, "error", err)
		writeError(w, http.StatusBadGateway, "could not reach the client")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --------------------------------------------------------------------------
// call log
// --------------------------------------------------------------------------

// callsPage is the body of GET /api/calls. limit and offset are echoed because
// they may have been clamped, and a console that paginates needs to know what
// it actually got rather than what it asked for.
type callsPage struct {
	Calls  []store.CallRecord `json:"calls"`
	Stats  store.CallStats    `json:"stats"`
	Limit  int                `json:"limit"`
	Offset int                `json:"offset"`
}

func (h *Handler) listCalls(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filter := store.CallFilter{
		Server:  clean(query.Get("server")),
		Tool:    clean(query.Get("tool")),
		Status:  clean(query.Get("status")),
		Label:   clean(query.Get("label")),
		Source:  clean(query.Get("source")),
		AgentID: clean(query.Get("agentId")),
		ChatID:  clean(query.Get("chatId")),
		Limit:   intParam(query.Get("limit"), store.DefaultLimit),
		Offset:  intParam(query.Get("offset"), 0),
	}
	// Clamped rather than rejected: a console asking for too much should get a
	// page of results, not a validation error.
	if filter.Limit < 1 {
		filter.Limit = 1
	}
	if filter.Limit > store.MaxLimit {
		filter.Limit = store.MaxLimit
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}

	records, err := h.opts.Store.ListCalls(r.Context(), filter)
	if err != nil {
		h.log.Error("could not list calls", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the call log")
		return
	}
	stats, err := h.opts.Store.CallStats(r.Context())
	if err != nil {
		h.log.Error("could not read call stats", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the call log")
		return
	}
	if records == nil {
		records = []store.CallRecord{}
	}

	writeJSON(w, http.StatusOK, callsPage{
		Calls:  records,
		Stats:  stats,
		Limit:  filter.Limit,
		Offset: filter.Offset,
	})
}

func (h *Handler) getCall(w http.ResponseWriter, r *http.Request) {
	record, err := h.opts.Store.GetCall(r.Context(), r.PathValue("id"))
	if err != nil {
		h.log.Error("could not read a call", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the call log")
		return
	}
	if record == nil {
		writeError(w, http.StatusNotFound, "unknown call")
		return
	}
	writeJSON(w, http.StatusOK, record)
}

// --------------------------------------------------------------------------
// stats
// --------------------------------------------------------------------------

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	// Chat growth is unbounded by design (D11), so the size on disk and the
	// per-table row counts are served continuously: the point is that it is
	// visible long before it is a problem. Active runs join this payload with
	// the orchestrator.
	stats, err := h.opts.Store.Stats(r.Context())
	if err != nil {
		h.log.Error("could not read stats", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read stats")
		return
	}
	body := statsBody{Stats: stats}
	if h.opts.Agents != nil {
		n := h.opts.Agents.ActiveRuns()
		body.ActiveRuns = &n
		if h.opts.Streams != nil {
			c := h.opts.Streams.Subscribers()
			body.ChatStreamClients = &c
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// statsBody is store.Stats (whose Tables already count the orchestrator's
// tables) plus the live orchestrator counters, present only when it is enabled.
type statsBody struct {
	store.Stats
	ActiveRuns        *int `json:"activeRuns,omitempty"`
	ChatStreamClients *int `json:"chatStreamClients,omitempty"`
}

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

func hasTool(channel *registry.ServerChannel, name string) bool {
	for _, tool := range channel.Tools() {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// clean treats an empty query parameter as an absent filter, so a console that
// clears a filter box by sending "" does not filter for the empty string.
func clean(value string) string { return strings.TrimSpace(value) }

func intParam(raw string, fallback int) int {
	if strings.TrimSpace(raw) == "" {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return fallback
	}
	return n
}

func quote(s string) string { return "'" + s + "'" }

func decodeBody(r *http.Request, into any) error {
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(into); err != nil {
		// An empty body is not a malformed one; see callRequest.
		if strings.Contains(err.Error(), "EOF") {
			return nil
		}
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	payload, err := json.Marshal(body)
	if err != nil {
		slog.Error("could not encode a response", "error", err)
		http.Error(w, `{"detail":"could not encode the response"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// writeError keeps the {"detail": ...} shape the console already parses.
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, map[string]string{"detail": detail})
}
