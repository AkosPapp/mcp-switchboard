package loki

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) CountLokiDropped(n int) {
	c.mu.Lock()
	c.n += n
	c.mu.Unlock()
}

func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// quiet keeps the one-per-outage warning out of the test output.
func quiet() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

type recorder struct {
	mu       sync.Mutex
	payloads []Payload
	paths    []string
	status   int
	block    chan struct{}
}

func newRecorder() *recorder { return &recorder{status: http.StatusNoContent} }

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r.block != nil {
		<-r.block
	}
	var p Payload
	_ = json.NewDecoder(req.Body).Decode(&p)
	r.mu.Lock()
	r.payloads = append(r.payloads, p)
	r.paths = append(r.paths, req.URL.Path)
	status := r.status
	r.mu.Unlock()
	w.WriteHeader(status)
}

func (r *recorder) streams() []Stream {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Stream
	for _, p := range r.payloads {
		out = append(out, p.Streams...)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestBatchedPushShape(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()

	drops := &counter{}
	e := New(Config{
		URL: srv.URL, Enabled: true, BatchSize: 2, FlushInterval: 10 * time.Millisecond,
		Labels: map[string]string{"host": "hub-1", "bad-name": "x"},
		Client: srv.Client(), Logger: quiet(),
	}, drops)
	e.Start()
	defer e.Close(context.Background())

	e.Emit("tool_call", map[string]any{"tool": "status", "duration_ms": 12}, map[string]string{"level": "info"})
	e.Emit("tool_call", map[string]any{"tool": "commit"}, map[string]string{"level": "info"})

	waitFor(t, "a push", func() bool { return len(rec.streams()) > 0 })

	rec.mu.Lock()
	path := rec.paths[0]
	rec.mu.Unlock()
	if path != PushPath {
		t.Errorf("push path = %q, want %q", path, PushPath)
	}

	s := rec.streams()[0]
	if s.Stream["event"] != "tool_call" || s.Stream["level"] != "info" || s.Stream["host"] != "hub-1" {
		t.Errorf("stream labels = %v", s.Stream)
	}
	if _, ok := s.Stream["bad_name"]; !ok {
		t.Errorf("label name was not sanitized: %v", s.Stream)
	}
	if len(s.Values) != 2 {
		t.Fatalf("values = %d, want 2 (both entries share a label set)", len(s.Values))
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(s.Values[0][1]), &line); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if line["event"] != "tool_call" || line["tool"] != "status" {
		t.Errorf("line = %v", line)
	}
	if drops.get() != 0 {
		t.Errorf("drops = %d, want 0", drops.get())
	}
}

// The whole point of the bounded queue: a wedged Loki must never hold up the
// hub, so the excess is dropped and counted (mcpsb_loki_dropped_total).
func TestHangingReceiverNeverBlocksThePublisher(t *testing.T) {
	rec := newRecorder()
	rec.block = make(chan struct{})
	srv := httptest.NewServer(rec)
	defer srv.Close()

	drops := &counter{}
	e := New(Config{
		URL: srv.URL, Enabled: true, BatchSize: 5, QueueSize: 10,
		FlushInterval: time.Millisecond, Timeout: time.Second,
		Client: srv.Client(), Logger: quiet(),
	}, drops)
	e.Start()

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for i := 0; i < 5000; i++ {
			e.Emit("noise", map[string]any{"i": i}, nil)
		}
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		if took > 2*time.Second {
			t.Errorf("5000 emits took %v; the producer is blocking", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked on a hung receiver")
	}

	waitFor(t, "drops to be counted", func() bool { return drops.get() > 0 })

	close(rec.block)
	e.Close(context.Background())
}

func TestFlushOnShutdown(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()

	drops := &counter{}
	e := New(Config{
		URL: srv.URL, Enabled: true, BatchSize: 100,
		FlushInterval: time.Hour, // only Close can get these out
		Client:        srv.Client(), Logger: quiet(),
	}, drops)
	e.Start()

	e.Emit("run_finished", map[string]any{"status": "ok"}, nil)
	e.Close(context.Background())

	streams := rec.streams()
	if len(streams) != 1 || len(streams[0].Values) != 1 {
		t.Fatalf("shutdown did not flush the backlog: %+v", streams)
	}
	if drops.get() != 0 {
		t.Errorf("drops = %d, want 0", drops.get())
	}
	if e.Pending() != 0 {
		t.Errorf("pending = %d after close", e.Pending())
	}
}

// Shutdown is bounded: an unreachable Loki costs the grace period, not the
// process.
func TestShutdownIsBoundedAndCountsWhatItLost(t *testing.T) {
	rec := newRecorder()
	rec.block = make(chan struct{})
	srv := httptest.NewServer(rec)
	defer srv.Close()

	drops := &counter{}
	e := New(Config{
		URL: srv.URL, Enabled: true, BatchSize: 1, FlushInterval: time.Hour,
		Timeout: 100 * time.Millisecond,
		Client:  srv.Client(), Logger: quiet(),
	}, drops)
	e.Start()
	for i := 0; i < 3; i++ {
		e.Emit("noise", map[string]any{"i": i}, nil)
	}

	start := time.Now()
	e.Close(context.Background())
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("Close took %v on a wedged receiver", took)
	}
	if drops.get() == 0 {
		t.Error("events lost at shutdown were not counted as drops")
	}
	close(rec.block)
}

// A 500 drops the batch and carries on; it must not wedge the pusher or retry
// forever.
func TestServerErrorDoesNotWedgeThePusher(t *testing.T) {
	rec := newRecorder()
	rec.status = http.StatusInternalServerError
	srv := httptest.NewServer(rec)
	defer srv.Close()

	drops := &counter{}
	e := New(Config{
		URL: srv.URL, Enabled: true, BatchSize: 1, FlushInterval: 5 * time.Millisecond,
		Client: srv.Client(), Logger: quiet(),
	}, drops)
	e.Start()
	defer e.Close(context.Background())

	e.Emit("a", nil, nil)
	e.Emit("b", nil, nil)
	waitFor(t, "both batches to be dropped", func() bool { return drops.get() >= 2 })
	waitFor(t, "the queue to drain", func() bool { return e.Pending() == 0 })

	rec.mu.Lock()
	rec.status = http.StatusNoContent
	rec.mu.Unlock()

	before := drops.get()
	e.Emit("c", nil, nil)
	waitFor(t, "recovery", func() bool { return e.Pending() == 0 && len(rec.streams()) >= 3 })
	if drops.get() != before {
		t.Errorf("drops = %d after recovery, want %d", drops.get(), before)
	}
}

func TestDisabledExporterDropsNothing(t *testing.T) {
	drops := &counter{}
	// Enabled with no URL is off: it would otherwise drop everything while
	// pretending to work.
	for _, cfg := range []Config{{Enabled: false, URL: "http://loki"}, {Enabled: true, URL: ""}} {
		cfg.Logger = quiet()
		e := New(cfg, drops)
		if e.Enabled() {
			t.Errorf("%+v should be disabled", cfg)
		}
		e.Start()
		e.Emit("tool_call", map[string]any{"tool": "status"}, nil)
		e.Close(context.Background())
	}
	if drops.get() != 0 {
		t.Errorf("drops = %d; an unconfigured Loki is not a loss", drops.get())
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	e := New(Config{URL: "http://loki", Enabled: true, Logger: quiet()}, nil)
	ent := e.build("llm_request", map[string]any{
		"provider":   "anthropic",
		"api_key":    "sk-live-123",
		"Authorized": "no",
		"headers":    map[string]any{"authorization": "Bearer abc", "accept": "json"},
		"tokens":     []any{map[string]any{"token": "sk-x"}},
	}, nil)

	if strings.Contains(ent.line, "sk-live-123") || strings.Contains(ent.line, "Bearer abc") || strings.Contains(ent.line, "sk-x") {
		t.Errorf("secret survived redaction: %s", ent.line)
	}
	if !strings.Contains(ent.line, `"provider":"anthropic"`) {
		t.Errorf("non-secret field was lost: %s", ent.line)
	}
}

// The caller may mutate its fields map the moment Emit returns, so the line is
// serialized up front.
func TestFieldsAreSnapshotAtEmit(t *testing.T) {
	rec := newRecorder()
	srv := httptest.NewServer(rec)
	defer srv.Close()

	e := New(Config{URL: srv.URL, Enabled: true, BatchSize: 1, FlushInterval: time.Hour,
		Client: srv.Client(), Logger: quiet()}, nil)
	e.Start()

	fields := map[string]any{"tool": "status"}
	e.Emit("tool_call", fields, nil)
	fields["tool"] = "mutated"
	e.Close(context.Background())

	streams := rec.streams()
	if len(streams) != 1 {
		t.Fatalf("streams = %d", len(streams))
	}
	if strings.Contains(streams[0].Values[0][1], "mutated") {
		t.Errorf("line reflects a post-Emit mutation: %s", streams[0].Values[0][1])
	}
}

func TestConcurrentEmitAndClose(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	e := New(Config{URL: srv.URL, Enabled: true, BatchSize: 4, FlushInterval: time.Millisecond,
		Client: srv.Client(), Logger: quiet()}, &counter{})
	e.Start()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				e.Emit("noise", map[string]any{"j": j}, nil)
			}
		}()
	}
	go e.Close(context.Background())
	wg.Wait()
	e.Close(context.Background())
}
