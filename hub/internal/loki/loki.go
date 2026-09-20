// Package loki is fire-and-forget structured logging to Grafana Loki.
//
// The hub serves tool calls; shipping logs is strictly secondary to that. So
// the producer side (Emit) never blocks, never fails and never returns an
// error, and all the network work happens on one background goroutine that
// batches whatever has piled up. If Loki is slow, wedged or gone, events are
// counted as dropped and the hub carries on at full speed.
//
// LABEL DISCIPLINE: Loki indexes stream labels, so every distinct combination
// of label values creates a stream. Labels here must stay low-cardinality -
// machine label, server name, event name, level. Never pass tool arguments,
// call ids, error strings, durations or anything else unbounded as a label;
// those belong in Fields, which is serialized into the log line and not
// indexed. (Loki copes with high cardinality in the body; only Prometheus
// labels are constrained - spec.md O4.)
//
// Message content never goes here at all (spec.md O5): prompts and completions
// stay in the store, and an operator who wants them reads the chat.
package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PushPath is Loki's push API, appended to the configured base URL.
const PushPath = "/loki/api/v1/push"

// MaxLabelValue is a backstop against a caller that ignores the label
// discipline above: a long value is truncated rather than minting an unbounded
// number of streams.
const MaxLabelValue = 128

// Redacted replaces any field value whose key looks like a credential (spec.md
// X7: keys are never returned by an endpoint, never written to the call log and
// redacted from Loki payloads).
const Redacted = "[redacted]"

// DropCounter is how the exporter reports events it threw away. It is taken as
// a dependency rather than importing the metrics package, so the two stay
// decoupled and this one is testable on its own. *metrics.Metrics satisfies it.
type DropCounter interface {
	CountLokiDropped(n int)
}

// DropCounterFunc adapts a plain function to DropCounter.
type DropCounterFunc func(n int)

func (f DropCounterFunc) CountLokiDropped(n int) { f(n) }

// Config is what the hub knows about Loki from its environment.
type Config struct {
	URL     string            // base URL; empty means off
	Labels  map[string]string // static stream labels, e.g. host or env
	Enabled bool

	BatchSize     int           // entries per push (default 100)
	FlushInterval time.Duration // periodic tick (default 2s)
	QueueSize     int           // bounded backlog (default 10000)
	Timeout       time.Duration // per-push HTTP timeout (default 5s)

	Client *http.Client // test seam; production callers leave it nil
	Logger *slog.Logger // defaults to JSON on stderr (spec.md O3)
}

type entry struct {
	tsNanos int64
	stream  string // the canonical label set, used as a map key
	labels  map[string]string
	line    string
}

// Exporter batches structured events and pushes them to Loki.
type Exporter struct {
	pushURL      string
	staticLabels map[string]string
	enabled      bool
	batchSize    int
	flushEvery   time.Duration
	queueSize    int
	closeBudget  time.Duration
	client       *http.Client
	log          *slog.Logger
	drops        DropCounter

	mu      sync.Mutex
	queue   []entry
	closed  bool
	running bool

	wake    chan struct{}
	done    chan struct{}
	stop    chan struct{}
	stopOne sync.Once

	// Outage bookkeeping, touched only by the pusher goroutine.
	failing       bool
	failedBatches int
}

// New builds an exporter. It does not start the background goroutine; call
// Start for that. drops may be nil.
func New(cfg Config, drops DropCounter) *Exporter {
	url := strings.TrimRight(cfg.URL, "/")
	e := &Exporter{
		pushURL:      url + PushPath,
		staticLabels: cleanLabels(cfg.Labels),
		// A configured-on exporter with no URL would drop everything while
		// pretending to work; treat it as off.
		enabled:    cfg.Enabled && url != "",
		batchSize:  orDefaultInt(cfg.BatchSize, 100),
		flushEvery: orDefaultDuration(cfg.FlushInterval, 2*time.Second),
		queueSize:  orDefaultInt(cfg.QueueSize, 10_000),
		client:     cfg.Client,
		log:        cfg.Logger,
		drops:      drops,
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
		stop:       make(chan struct{}),
	}
	if e.log == nil {
		// O3: structured JSON on stderr. Defaulting here rather than to
		// slog.Default() keeps the shape right even if the process never
		// installed a handler.
		e.log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	timeout := orDefaultDuration(cfg.Timeout, 5*time.Second)
	if e.client == nil {
		e.client = &http.Client{Timeout: timeout}
	}
	// Shutdown must not hang on an unreachable Loki.
	e.closeBudget = timeout
	if e.closeBudget < time.Second {
		e.closeBudget = time.Second
	}
	return e
}

// Enabled reports whether events are shipped at all.
func (e *Exporter) Enabled() bool { return e != nil && e.enabled }

// Start launches the pusher. Calling it twice, or on a disabled exporter, is a
// no-op.
func (e *Exporter) Start() {
	if !e.Enabled() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running || e.closed {
		return
	}
	e.running = true
	go e.run()
}

// Emit queues one event. It never blocks, never fails and is safe from any
// goroutine, including the tool-call path - so every failure mode here is a
// drop plus a counter bump.
func (e *Exporter) Emit(event string, fields map[string]any, labels map[string]string) {
	if !e.Enabled() {
		// Loki is simply not configured: that is not a loss, so it is not
		// counted as a drop. The counter would otherwise tick on every event
		// for everyone who does not run Loki.
		return
	}

	ent := e.build(event, fields, labels)

	e.mu.Lock()
	if e.closed || len(e.queue) >= e.queueSize {
		e.mu.Unlock()
		// Drop the new event rather than the backlog: the oldest entries are
		// the ones already being flushed.
		e.drop(1)
		return
	}
	e.queue = append(e.queue, ent)
	full := len(e.queue) >= e.batchSize
	e.mu.Unlock()

	if full {
		e.wakeSoon()
	}
}

// Pending is the current backlog, for tests and /api/stats.
func (e *Exporter) Pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.queue)
}

// Close flushes what is queued within a bounded grace period, then gives up and
// counts the rest as dropped. It is safe to call more than once.
func (e *Exporter) Close(ctx context.Context) {
	if !e.Enabled() {
		return
	}
	e.stopOne.Do(func() {
		e.mu.Lock()
		e.closed = true
		running := e.running
		e.mu.Unlock()
		close(e.stop)

		ctx, cancel := context.WithTimeout(ctx, e.closeBudget)
		defer cancel()

		if running {
			select {
			case <-e.done:
			case <-ctx.Done():
				// A wedged POST is not allowed to hold shutdown open; the
				// pusher goroutine notices e.stop and unwinds on its own.
			}
		}

		e.mu.Lock()
		lost := len(e.queue)
		e.queue = nil
		e.mu.Unlock()
		if lost > 0 {
			e.drop(lost)
		}
	})
}

func (e *Exporter) build(event string, fields map[string]any, labels map[string]string) entry {
	stream := make(map[string]string, len(e.staticLabels)+len(labels)+1)
	for k, v := range e.staticLabels {
		stream[k] = v
	}
	stream["event"] = cleanValue(event)
	for k, v := range cleanLabels(labels) {
		stream[k] = v
	}

	line := make(map[string]any, len(fields)+1)
	for k, v := range fields {
		line[k] = redact(k, v, 0)
	}
	line["event"] = event

	// Serialize now, not at flush time: the caller is free to mutate its fields
	// map the moment Emit returns.
	payload, err := json.Marshal(line)
	if err != nil {
		payload, _ = json.Marshal(map[string]string{"event": event, "error": "unserializable fields"})
	}

	return entry{tsNanos: time.Now().UnixNano(), stream: streamKey(stream), labels: stream, line: string(payload)}
}

func (e *Exporter) drop(n int) {
	if e.drops == nil || n <= 0 {
		return
	}
	defer func() { _ = recover() }() // a broken counter must not fail a tool call
	e.drops.CountLokiDropped(n)
}

func (e *Exporter) wakeSoon() {
	select {
	case e.wake <- struct{}{}:
	default: // a wake is already pending; one is enough
	}
}

func (e *Exporter) run() {
	defer close(e.done)

	tick := time.NewTicker(e.flushEvery)
	defer tick.Stop()

	for {
		select {
		case <-tick.C:
		case <-e.wake:
		case <-e.stop:
			// The parting drain is bounded, because shutdown is not allowed to
			// hang on an unresponsive Loki.
			ctx, cancel := context.WithTimeout(context.Background(), e.closeBudget)
			e.drain(ctx)
			cancel()
			return
		}
		e.drain(context.Background())
	}
}

func (e *Exporter) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		e.mu.Lock()
		if len(e.queue) == 0 {
			e.mu.Unlock()
			return
		}
		n := min(len(e.queue), e.batchSize)
		batch := e.queue[:n:n]
		e.queue = e.queue[n:]
		e.mu.Unlock()

		e.post(ctx, batch)
	}
}

func (e *Exporter) post(ctx context.Context, batch []entry) {
	body, err := json.Marshal(BuildPayload(batch))
	if err != nil {
		e.drop(len(batch))
		e.noteFailure(err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.pushURL, bytes.NewReader(body))
	if err != nil {
		e.drop(len(batch))
		e.noteFailure(err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		// A failed push is dropped, never retried: a backlog kept for a dead
		// Loki would grow until it evicted live events.
		e.drop(len(batch))
		e.noteFailure(err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode >= 400 {
		e.drop(len(batch))
		e.noteFailure(fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}
	e.noteSuccess()
}

func (e *Exporter) noteFailure(err error) {
	e.failedBatches++
	if !e.failing {
		// One warning per outage, not one per batch: a down Loki must not drown
		// the hub's own logs.
		e.failing = true
		e.log.Warn("loki push failed, dropping events", "url", e.pushURL, "error", err.Error())
	}
}

func (e *Exporter) noteSuccess() {
	if e.failing {
		e.log.Info("loki push recovered", "url", e.pushURL, "failed_batches", e.failedBatches)
		e.failing = false
		e.failedBatches = 0
	}
}

// Stream is one Loki stream: a label set and its lines.
type Stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// Payload is the body of a Loki push.
type Payload struct {
	Streams []Stream `json:"streams"`
}

// BuildPayload groups entries sharing a label set into Loki streams.
func BuildPayload(batch []entry) Payload {
	order := make([]string, 0, len(batch))
	byKey := make(map[string]*Stream, len(batch))
	for _, ent := range batch {
		s, ok := byKey[ent.stream]
		if !ok {
			s = &Stream{Stream: ent.labels}
			byKey[ent.stream] = s
			order = append(order, ent.stream)
		}
		s.Values = append(s.Values, [2]string{strconv.FormatInt(ent.tsNanos, 10), ent.line})
	}

	out := Payload{Streams: make([]Stream, 0, len(order))}
	for _, key := range order {
		s := byKey[key]
		// Loki rejects out-of-order lines within a stream on older versions,
		// and the queue only approximates order once batches interleave.
		sort.SliceStable(s.Values, func(i, j int) bool { return s.Values[i][0] < s.Values[j][0] })
		out.Streams = append(out.Streams, *s)
	}
	return out
}

// secretish keys carry credentials whatever the caller named them; matching is
// substring and case-insensitive because the alternative is maintaining a list
// that a new config option quietly falls off.
var secretish = []string{"token", "secret", "password", "passwd", "apikey", "api_key", "authorization", "credential", "private_key"}

func redact(key string, value any, depth int) any {
	if looksSecret(key) {
		return Redacted
	}
	if depth > 8 { // cheap cycle/blowup guard; the JSON encoder would recurse too
		return value
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, inner := range v {
			out[k] = redact(k, inner, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, inner := range v {
			out[i] = redact(key, inner, depth+1)
		}
		return out
	default:
		return value
	}
}

func looksSecret(key string) bool {
	k := strings.ToLower(key)
	for _, needle := range secretish {
		if strings.Contains(k, needle) {
			return true
		}
	}
	return false
}

func streamKey(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
		b.WriteByte(0)
	}
	return b.String()
}

func cleanLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if name := cleanName(k); name != "" {
			out[name] = cleanValue(v)
		}
	}
	return out
}

// Loki label names are [a-zA-Z_][a-zA-Z0-9_]*; anything else is rewritten.
func cleanName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out != "" && out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

func cleanValue(value string) string {
	if len(value) > MaxLabelValue {
		return value[:MaxLabelValue]
	}
	return value
}

func orDefaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orDefaultDuration(v, def time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	return v
}
