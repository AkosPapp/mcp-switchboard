package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/calls"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/llm"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/push"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Pusher is the slice of push.Sender the orchestrator uses to notify a phone
// (spec.md 8.7) when a top-level human chat's run finishes or blocks on
// approval. Narrow, so a test can fake it without a real VAPID identity.
type Pusher interface {
	Enabled() bool
	Send(ctx context.Context, payload push.Payload) []error
}

// Metrics is the slice of the metrics surface the orchestrator touches; every
// label is bounded (spec.md 11.1). *metrics.Metrics implements it.
type Metrics interface {
	SetAgents(counts map[string]map[string]int)
	CountSpawn(project string)
	AddRunsActive(project string, delta int)
	ObserveRun(project, status, finishReason string, seconds float64)
	ObserveTurn(project, provider, model string, in, out, cacheRead, cacheWrite int, costMicros int64, seconds float64)
	CountLLMError(provider, model, kind string)
	CountAgentMessage(project string)
	ObserveDelivery(project string, seconds float64)
	CountApproval(outcome string)
}

// Options configure a Manager. Store, Registry, Dispatcher and LLM are
// required; the rest may be nil.
type Options struct {
	Store      store.Store
	Registry   *registry.Registry
	Dispatcher *calls.Dispatcher
	LLM        *llm.Registry
	Bus        *events.Bus
	Streamer   Streamer
	Settings   config.Settings
	Metrics    Metrics
	Loki       calls.Exporter
	Logger     *slog.Logger
	// Push sends the two phone notifications of spec.md 8.7. Nil (or an
	// unconfigured Sender) means notifications are simply never sent.
	Push Pusher
}

// Manager is the orchestrator: run scheduler, run loop, tool catalog and the
// switchboard.* tools. It implements Service.
type Manager struct {
	st       store.Store
	reg      *registry.Registry
	disp     *calls.Dispatcher
	llm      *llm.Registry
	bus      *events.Bus
	stream   Streamer
	set      config.Settings
	met      Metrics
	loki     calls.Exporter
	log      *slog.Logger
	push     Pusher
	maxRun   int
	parallel int

	baseCtx    context.Context
	baseCancel context.CancelCauseFunc
	wg         sync.WaitGroup

	// mu guards the scheduler state below and every runState field marked
	// "guarded by Manager.mu". It is never held across I/O.
	mu         sync.Mutex
	runs       map[string]*runState // active (queued in memory, running or waiting)
	byAgent    map[string]map[string]*runState
	chatActive map[string]*runState   // the one run per chat (R4)
	chatQueue  map[string][]*runState // runs queued behind it
	running    int                    // slot holders (R7)
	resumeQ    []*waiter              // runs coming back from a wait; served first
	startQ     []*waiter

	// statusMu serialises "derive status, then write it", so two racing
	// transitions cannot leave a stale value behind.
	statusMu sync.Mutex
	// chatMu serialises lazy creation of a record's fallback chat.
	chatMu sync.Mutex
}

type waiter struct {
	rs      *runState
	ch      chan struct{}
	granted bool
}

var (
	errCancelled = errors.New("run cancelled")
	errWall      = errors.New("max_wall_seconds exceeded")
	errShutdown  = errors.New("hub shutting down")
)

// New builds a Manager. It has no side effects until Start.
func New(o Options) *Manager {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	maxRun := o.Settings.AgentMaxConcurrentRuns
	if maxRun <= 0 {
		maxRun = 16
	}
	par := o.Settings.AgentMaxParallelToolCalls
	if par <= 0 {
		par = 8
	}
	base, cancel := context.WithCancelCause(context.Background())
	return &Manager{
		st: o.Store, reg: o.Registry, disp: o.Dispatcher, llm: o.LLM, bus: o.Bus, stream: o.Streamer,
		set: o.Settings, met: o.Metrics, loki: o.Loki, log: log, push: o.Push, maxRun: maxRun, parallel: par,
		baseCtx: base, baseCancel: cancel,
		runs: map[string]*runState{}, byAgent: map[string]map[string]*runState{},
		chatActive: map[string]*runState{}, chatQueue: map[string][]*runState{},
	}
}

var _ Service = (*Manager)(nil)

// Start performs the A3 startup sweep: runs left running/waiting/queued by a
// previous process are marked interrupted. It also starts the agent gauge
// refresher, which stops when ctx is cancelled.
func (m *Manager) Start(ctx context.Context) error {
	n, err := m.st.InterruptRunning(ctx)
	if err != nil {
		return fmt.Errorf("agents: interrupting stale runs: %w", err)
	}
	if n > 0 {
		m.log.Info("marked runs from a previous process as interrupted", "runs", n)
	}
	if err := m.seedProfiles(ctx); err != nil {
		return fmt.Errorf("agents: seeding the default profile: %w", err)
	}
	m.syncAgentGauge()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.baseCtx.Done():
				return
			case <-t.C:
				m.syncAgentGauge()
			}
		}
	}()
	return nil
}

// Shutdown cancels every run (they are recorded as interrupted, G3) and waits
// for them to finish, bounded by ctx.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.baseCancel(errShutdown)
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------- scheduler

// acquire takes a concurrency slot, blocking until one frees or ctx ends.
// resume queues ahead of new starts so a run coming back from a wait cannot be
// starved by a stream of fresh runs.
func (m *Manager) acquire(ctx context.Context, rs *runState, resume bool) error {
	m.mu.Lock()
	if m.running < m.maxRun {
		m.running++
		m.mu.Unlock()
		m.gauge(rs, +1)
		return nil
	}
	w := &waiter{rs: rs, ch: make(chan struct{})}
	if resume {
		m.resumeQ = append(m.resumeQ, w)
	} else {
		m.startQ = append(m.startQ, w)
	}
	m.mu.Unlock()

	select {
	case <-w.ch:
		return nil
	case <-ctx.Done():
		m.mu.Lock()
		if w.granted {
			m.releaseLocked(rs)
		} else {
			m.resumeQ, m.startQ = removeWaiter(m.resumeQ, w), removeWaiter(m.startQ, w)
		}
		m.mu.Unlock()
		return context.Cause(ctx)
	}
}

func removeWaiter(q []*waiter, w *waiter) []*waiter {
	for i, x := range q {
		if x == w {
			return append(q[:i:i], q[i+1:]...)
		}
	}
	return q
}

// releaseLocked gives a slot back and hands it to the next waiter.
func (m *Manager) releaseLocked(rs *runState) {
	m.running--
	m.gauge(rs, -1)
	for m.running < m.maxRun {
		var w *waiter
		switch {
		case len(m.resumeQ) > 0:
			w, m.resumeQ = m.resumeQ[0], m.resumeQ[1:]
		case len(m.startQ) > 0:
			w, m.startQ = m.startQ[0], m.startQ[1:]
		default:
			return
		}
		m.running++
		w.granted = true
		m.gauge(w.rs, +1)
		close(w.ch)
	}
}

func (m *Manager) gauge(rs *runState, delta int) {
	if m.met != nil && rs != nil {
		m.met.AddRunsActive(rs.project, delta)
	}
}

// enterWait marks a run as waiting (approval or another chat's reply): it stops
// holding a concurrency slot (R7).
func (m *Manager) enterWait(rs *runState, approval bool) {
	if rs == nil {
		return
	}
	m.mu.Lock()
	rs.waiters++
	if approval {
		rs.approvalWaiters++
	}
	if rs.holdsSlot {
		rs.holdsSlot = false
		m.releaseLocked(rs)
	}
	m.mu.Unlock()
	m.syncRun(rs)
}

// leaveWait ends one wait; when the last one ends the run re-acquires a slot.
func (m *Manager) leaveWait(rs *runState, approval bool) {
	if rs == nil {
		return
	}
	m.mu.Lock()
	rs.waiters--
	if approval {
		rs.approvalWaiters--
	}
	need := rs.waiters == 0 && !rs.holdsSlot && !rs.acquiring && !rs.finished
	if need {
		rs.acquiring = true
	}
	m.mu.Unlock()
	if need {
		err := m.acquire(rs.ctx, rs, true)
		m.mu.Lock()
		rs.acquiring = false
		if err == nil {
			if rs.waiters > 0 || rs.finished {
				m.releaseLocked(rs)
			} else {
				rs.holdsSlot = true
			}
		}
		m.mu.Unlock()
	}
	m.syncRun(rs)
}

// -------------------------------------------------------------- run bookkeeping

// syncRun writes the run's derived status (running/waiting) and the agent's.
func (m *Manager) syncRun(rs *runState) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.mu.Lock()
	if rs.finished || !rs.started {
		m.mu.Unlock()
		return
	}
	status := store.RunRunning
	if rs.waiters > 0 {
		status = store.RunWaiting
	}
	m.mu.Unlock()
	ctx, cancel := detached()
	defer cancel()
	if _, err := m.st.UpdateRun(ctx, rs.id, store.RunPatch{Status: &status}); err != nil {
		m.log.Warn("could not update run status", "run", rs.id, "error", err)
	}
	m.writeAgentStatusLocked(rs.agentID, "")
	m.publish(events.Event{Type: events.TypeChat, ChatID: rs.chatID, AgentID: rs.agentID})
}

// refreshAgentStatus recomputes and stores an agent's status. terminal is the
// status to fall back to when no run is active ("" = idle).
func (m *Manager) refreshAgentStatus(agentID, terminal string) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.writeAgentStatusLocked(agentID, terminal)
}

// writeAgentStatusLocked requires statusMu.
func (m *Manager) writeAgentStatusLocked(agentID, terminal string) {
	m.mu.Lock()
	status := ""
	for _, rs := range m.byAgent[agentID] {
		if !rs.started || rs.finished {
			continue
		}
		switch {
		case rs.waiters == 0:
			status = store.AgentRunning
		case rs.approvalWaiters > 0 && status != store.AgentRunning:
			status = store.AgentBlocked
		case status == "":
			status = store.AgentWaiting
		}
	}
	m.mu.Unlock()
	if status == "" {
		status = terminal
		if status == "" {
			status = store.AgentIdle
		}
	}
	ctx, cancel := detached()
	defer cancel()
	if err := m.st.SetAgentStatus(ctx, agentID, status); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.log.Warn("could not set agent status", "agent", agentID, "error", err)
		}
		return
	}
	m.publish(events.Event{Type: events.TypeAgent, AgentID: agentID})
}

// notifyPush sends a phone notification for rs's chat (spec.md 8.7), but only
// when it is a top-level human chat (push.IsTopLevelHumanChat) - a spawned
// child finishing its turn is not interrupt-worthy on its own. title falls
// back to the chat's own title when empty.
func (m *Manager) notifyPush(rs *runState, kind, body string) {
	if m.push == nil || !m.push.Enabled() {
		return
	}
	ctx, cancel := detached()
	defer cancel()
	chat, err := m.st.GetChat(ctx, rs.chatID)
	if err != nil || chat == nil {
		return
	}
	agent, err := m.st.GetAgent(ctx, rs.agentID)
	if err != nil || agent == nil {
		return
	}
	if !push.IsTopLevelHumanChat(chat.Kind, agent.ParentID == nil) {
		return
	}
	m.push.Send(ctx, push.Payload{Title: chat.Title, Body: body, ChatID: chat.ID, Kind: kind})
}

func (m *Manager) publish(ev events.Event) { m.bus.Publish(ev) }

func (m *Manager) emit(kind string, fields map[string]any) {
	if m.loki != nil {
		m.loki.Emit(kind, fields, nil)
	}
}

func (m *Manager) frame(rs *runState, typ string, data any) {
	if m.stream != nil {
		m.stream.Publish(rs.chatID, rs.id, typ, data)
	}
}

func detached() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (m *Manager) syncAgentGauge() {
	if m.met == nil {
		return
	}
	ctx, cancel := detached()
	defer cancel()
	agents, err := m.st.ListAgents(ctx, store.AgentFilter{})
	if err != nil {
		return
	}
	counts := map[string]map[string]int{}
	for _, a := range agents {
		project := ""
		if a.Project != nil {
			project = *a.Project
		}
		if counts[a.Status] == nil {
			counts[a.Status] = map[string]int{}
		}
		counts[a.Status][project]++
	}
	m.met.SetAgents(counts)
}

func projectOf(a *store.Agent) string {
	if a != nil && a.Project != nil {
		return *a.Project
	}
	return ""
}

// ----------------------------------------------------------------- submission

// submission is everything needed to create and enqueue one run.
type submission struct {
	agent       *store.Agent
	chat        *store.Chat
	trigger     string
	triggeredBy *string
	// msg, when set, is appended before the run starts (immediately if the chat
	// is idle, else when the run reaches the head of the chat's queue, so a
	// message never lands between a tool call and its results, R4).
	msg *store.Message
	// parentExplicit: msg.ParentID was chosen by the caller, not "the leaf".
	parentExplicit bool
	// setLeaf re-points the chat before the run (regenerate).
	setLeaf       string
	modelOverride json.RawMessage
	runID         string
	reply         *replyRoute
	enqueuedAt    time.Time
}

// submit creates the run row and schedules it. A run refused by the lifetime
// budget (B5) is recorded as done/"budget" and never started.
func (m *Manager) submit(ctx context.Context, sub submission) (*store.Run, error) {
	sub.agent, _ = m.resolveAgent(ctx, sub.agent) // live profile: budget snapshot, approval, prompt
	budget := mergeBudget(m.set.AgentDefaultBudget, sub.agent.Budget)
	snapshot := mustJSON(budget)
	run := store.Run{
		ID: sub.runID, AgentID: sub.agent.ID, ChatID: sub.chat.ID, Trigger: sub.trigger,
		TriggeredByAgentID: sub.triggeredBy, Status: store.RunQueued, BudgetSnapshot: snapshot,
	}

	if offender, exceeded, err := m.st.LifetimeExceeded(ctx, sub.agent.ID, budget.MaxLifetimeCostMicros); err != nil {
		return nil, err
	} else if exceeded {
		if sub.msg != nil {
			if err := m.appendPending(ctx, sub, nil); err != nil {
				return nil, err
			}
		}
		now := time.Now().UTC()
		fr := "budget"
		run.Status, run.FinishReason, run.FinishedAt, run.StartedAt = store.RunDone, &fr, &now, &now
		run.Usage = mustJSON(map[string]any{"limit": "max_lifetime_cost_micros", "offenderAgentId": offender})
		created, err := m.st.CreateRun(ctx, run)
		if err != nil {
			return nil, err
		}
		if m.met != nil {
			m.met.ObserveRun(projectOf(sub.agent), store.RunDone, "budget", 0)
		}
		m.emit("run_finished", map[string]any{"agentId": created.AgentID, "runId": created.ID, "status": created.Status, "finishReason": "budget", "limit": "max_lifetime_cost_micros"})
		m.publish(events.Event{Type: events.TypeChat, ChatID: created.ChatID, AgentID: created.AgentID})
		return &created, nil
	}

	created, err := m.st.CreateRun(ctx, run)
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithCancelCause(m.baseCtx)
	rs := &runState{
		id: created.ID, agentID: sub.agent.ID, chatID: sub.chat.ID, trigger: sub.trigger,
		triggeredBy: sub.triggeredBy, project: projectOf(sub.agent), ctx: rctx, cancel: cancel,
		done: make(chan struct{}), budget: budget, sub: sub, reply: sub.reply,
		enqueuedAt: sub.enqueuedAt, approvals: map[string]*pendingApproval{}, createdAt: time.Now(),
	}

	m.mu.Lock()
	m.runs[rs.id] = rs
	if m.byAgent[rs.agentID] == nil {
		m.byAgent[rs.agentID] = map[string]*runState{}
	}
	m.byAgent[rs.agentID][rs.id] = rs
	if m.chatActive[rs.chatID] != nil {
		m.chatQueue[rs.chatID] = append(m.chatQueue[rs.chatID], rs)
		m.mu.Unlock()
		m.publish(events.Event{Type: events.TypeChat, ChatID: rs.chatID, AgentID: rs.agentID})
		return &created, nil
	}
	m.chatActive[rs.chatID] = rs
	m.mu.Unlock()

	// The chat is ours: append the triggering message synchronously so the
	// caller can read it back as soon as Post returns.
	if err := m.materialize(ctx, rs); err != nil {
		m.finish(rs, runOutcome{status: store.RunError, finishReason: "error", err: err.Error()})
		return &created, err
	}
	m.start(rs)
	m.publish(events.Event{Type: events.TypeChat, ChatID: rs.chatID, AgentID: rs.agentID})
	return &created, nil
}

func (m *Manager) start(rs *runState) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.runMain(rs)
	}()
}

// materialize appends the pending message / re-points the leaf. Idempotent.
func (m *Manager) materialize(ctx context.Context, rs *runState) error {
	if rs.materialized {
		return nil
	}
	rs.materialized = true
	if err := m.appendPending(ctx, rs.sub, rs); err != nil {
		return err
	}
	if rs.leaf == nil {
		chat, err := m.st.GetChat(ctx, rs.chatID)
		if err != nil {
			return err
		}
		if chat != nil {
			rs.leaf = chat.ActiveLeafID
		}
	}
	return nil
}

func (m *Manager) appendPending(ctx context.Context, sub submission, rs *runState) error {
	if sub.setLeaf != "" {
		if err := m.st.SetActiveLeaf(ctx, sub.chat.ID, sub.setLeaf); err != nil {
			return err
		}
	}
	if sub.msg == nil {
		return nil
	}
	msg := *sub.msg
	if !sub.parentExplicit {
		chat, err := m.st.GetChat(ctx, sub.chat.ID)
		if err != nil {
			return err
		}
		if chat == nil {
			return ErrNotFound
		}
		msg.ParentID = chat.ActiveLeafID
	}
	var saved store.Message
	var err error
	if sub.trigger == store.TriggerHuman && msg.Role == store.RoleUser {
		// A human send clears the chat's unsent draft in the same transaction.
		saved, err = m.st.AppendMessageClearingDraft(ctx, msg)
	} else {
		saved, err = m.st.AppendMessage(ctx, msg)
	}
	if err != nil {
		return err
	}
	if rs != nil {
		rs.leaf = &saved.ID
	}
	m.publish(events.Event{Type: events.TypeChat, ChatID: saved.ChatID})
	return nil
}

// runMain is the goroutine of one run.
func (m *Manager) runMain(rs *runState) {
	out := m.execute(rs)
	m.finish(rs, out)
}

// finish records the outcome and moves the scheduler on.
func (m *Manager) finish(rs *runState, out runOutcome) {
	rs.cancel(nil)
	if rs.wallTimer != nil {
		rs.wallTimer.Stop()
	}
	now := time.Now().UTC()
	usage := rs.usageJSON(out.limit, now.Sub(rs.createdAt))
	patch := store.RunPatch{Status: &out.status, FinishReason: &out.finishReason, Usage: usage, FinishedAt: &now}
	if out.err != "" {
		patch.Error = &out.err
	}
	ctx, cancel := detached()
	if _, err := m.st.UpdateRun(ctx, rs.id, patch); err != nil {
		m.log.Warn("could not finalise run", "run", rs.id, "error", err)
	}
	cancel()

	m.statusMu.Lock()
	m.mu.Lock()
	rs.finished = true
	delete(m.runs, rs.id)
	delete(m.byAgent[rs.agentID], rs.id)
	if len(m.byAgent[rs.agentID]) == 0 {
		delete(m.byAgent, rs.agentID)
	}
	if rs.holdsSlot {
		rs.holdsSlot = false
		m.releaseLocked(rs)
	}
	m.mu.Unlock()
	terminal := store.AgentIdle
	if out.status == store.RunError {
		terminal = store.AgentError
	}
	m.writeAgentStatusLocked(rs.agentID, terminal)
	m.statusMu.Unlock()

	if rs.reply != nil {
		m.deliverReply(rs, out)
	}
	if out.status == store.RunDone || out.status == store.RunError {
		kind := push.KindRunDone
		body := firstLine(rs.lastText, 140)
		if out.status == store.RunError {
			kind, body = push.KindRunError, out.err
		}
		m.notifyPush(rs, kind, body)
	}

	if m.met != nil {
		m.met.ObserveRun(rs.project, out.status, out.finishReason, time.Since(rs.createdAt).Seconds())
	}
	m.frame(rs, "run_done", map[string]any{"runId": rs.id, "status": out.status, "finishReason": out.finishReason, "error": out.err, "limit": out.limit})
	if m.stream != nil {
		m.stream.EndRun(rs.chatID, rs.id)
	}
	m.emit("run_finished", map[string]any{"agentId": rs.agentID, "runId": rs.id, "status": out.status, "finishReason": out.finishReason, "limit": out.limit, "durationSeconds": time.Since(rs.createdAt).Seconds()})
	m.publish(events.Event{Type: events.TypeChat, ChatID: rs.chatID, AgentID: rs.agentID})
	close(rs.done)

	// Promote the next queued run for this chat (R4).
	m.mu.Lock()
	delete(m.chatActive, rs.chatID)
	var next *runState
	if q := m.chatQueue[rs.chatID]; len(q) > 0 {
		next, m.chatQueue[rs.chatID] = q[0], q[1:]
		if len(m.chatQueue[rs.chatID]) == 0 {
			delete(m.chatQueue, rs.chatID)
		}
		m.chatActive[rs.chatID] = next
	}
	m.mu.Unlock()
	if next != nil {
		m.start(next)
	}
}

// cancelRunState cancels a run wherever it is. A run still queued behind
// another is finished on the spot.
func (m *Manager) cancelRunState(rs *runState, cause error) {
	m.mu.Lock()
	queued := false
	if q := m.chatQueue[rs.chatID]; len(q) > 0 {
		for i, x := range q {
			if x == rs {
				m.chatQueue[rs.chatID] = append(q[:i:i], q[i+1:]...)
				if len(m.chatQueue[rs.chatID]) == 0 {
					delete(m.chatQueue, rs.chatID)
				}
				queued = true
				break
			}
		}
	}
	m.mu.Unlock()
	rs.cancel(cause)
	if queued {
		// Never started: finish() would promote a successor of its own chat
		// slot, which it does not hold. Record and drop it directly.
		m.finishQueued(rs)
	}
}

func (m *Manager) finishQueued(rs *runState) {
	now := time.Now().UTC()
	status, fr := store.RunCancelled, "cancelled"
	ctx, cancel := detached()
	defer cancel()
	_, _ = m.st.UpdateRun(ctx, rs.id, store.RunPatch{Status: &status, FinishReason: &fr, FinishedAt: &now})
	m.mu.Lock()
	rs.finished = true
	delete(m.runs, rs.id)
	delete(m.byAgent[rs.agentID], rs.id)
	if len(m.byAgent[rs.agentID]) == 0 {
		delete(m.byAgent, rs.agentID)
	}
	m.mu.Unlock()
	if m.met != nil {
		m.met.ObserveRun(rs.project, status, fr, 0)
	}
	close(rs.done)
	m.publish(events.Event{Type: events.TypeChat, ChatID: rs.chatID, AgentID: rs.agentID})
}

// cancelAgents cancels every in-memory run of the given agents and waits for
// them (bounded) so a delete does not race their final writes.
func (m *Manager) cancelAgents(ids []string, cause error) int {
	var victims []*runState
	m.mu.Lock()
	for _, id := range ids {
		for _, rs := range m.byAgent[id] {
			victims = append(victims, rs)
		}
	}
	m.mu.Unlock()
	for _, rs := range victims {
		m.cancelRunState(rs, cause)
	}
	deadline := time.After(5 * time.Second)
	for _, rs := range victims {
		select {
		case <-rs.done:
		case <-deadline:
			return len(victims)
		}
	}
	return len(victims)
}

func describeErr(err error) string {
	s := err.Error()
	if len(s) > 500 {
		s = s[:500]
	}
	return strings.TrimSpace(s)
}
