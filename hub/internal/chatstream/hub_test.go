package chatstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func next(t *testing.T, s *Subscription) Frame {
	t.Helper()
	done := make(chan struct{})
	timer := time.AfterFunc(2*time.Second, func() { close(done) })
	defer timer.Stop()
	f, err := s.Next(done, 0)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return f
}

func TestLiveFramesAndSeq(t *testing.T) {
	h := NewHub()
	defer h.Close()
	s := h.Subscribe("c1", "")
	defer s.Close()
	if h.Subscribers() != 1 {
		t.Fatalf("subscribers = %d", h.Subscribers())
	}
	h.Publish("c1", "r1", TypeRunStarted, map[string]any{"agentId": "a"})
	h.Publish("c1", "r1", TypeDelta, map[string]any{"messageId": "m", "contentIndex": 0, "text": "hi"})
	h.Publish("c1", "r2", TypeRunStarted, struct{}{})
	h.Publish("other", "rx", TypeDelta, map[string]any{})

	f1, f2, f3 := next(t, s), next(t, s), next(t, s)
	if f1.ID() != "r1:1" || f2.ID() != "r1:2" || f3.ID() != "r2:1" {
		t.Fatalf("ids %s %s %s", f1.ID(), f2.ID(), f3.ID())
	}
	var m map[string]any
	if err := json.Unmarshal(f2.Data, &m); err != nil {
		t.Fatal(err)
	}
	if m["text"] != "hi" || m["seq"].(float64) != 2 || m["type"] != "delta" || m["runId"] != "r1" {
		t.Fatalf("payload %v", m)
	}
	if !strings.HasPrefix(string(f2.Wire()), "id: r1:2\nevent: delta\ndata: {") {
		t.Fatalf("wire %q", f2.Wire())
	}
	s.Close()
	s.Close()
	if h.Subscribers() != 0 {
		t.Fatalf("subscribers = %d", h.Subscribers())
	}
}

func TestSlowConsumerOverflow(t *testing.T) {
	h := NewHubWithOptions(Options{SubscriberBudget: 600})
	defer h.Close()
	s := h.Subscribe("c", "")
	defer s.Close()
	for i := 0; i < 50; i++ {
		h.Publish("c", "r", TypeDelta, map[string]any{"text": strings.Repeat("x", 50)})
	}
	f := next(t, s)
	if f.Type != TypeOverflow {
		t.Fatalf("first frame %s, want overflow", f.Type)
	}
	if !strings.Contains(string(f.Data), ReasonSlowConsumer) {
		t.Fatalf("data %s", f.Data)
	}
	if _, err := s.Next(nil, 0); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
	// Publisher is unaffected.
	h.Publish("c", "r", TypeDelta, map[string]any{})
}

func TestResume(t *testing.T) {
	h := NewHub()
	defer h.Close()
	for i := 0; i < 5; i++ {
		h.Publish("c", "r1", TypeDelta, map[string]any{"i": i})
	}
	h.EndRun("c", "r1")
	h.Publish("c", "r2", TypeRunStarted, map[string]any{})
	h.Publish("c", "r2", TypeDelta, map[string]any{})

	s := h.Subscribe("c", "r1:3")
	defer s.Close()
	var got []string
	for i := 0; i < 4; i++ {
		got = append(got, next(t, s).ID())
	}
	if strings.Join(got, ",") != "r1:4,r1:5,r2:1,r2:2" {
		t.Fatalf("got %v", got)
	}
	// Live continues after the backlog with no gap or duplicate.
	h.Publish("c", "r2", TypeDelta, map[string]any{})
	if id := next(t, s).ID(); id != "r2:3" {
		t.Fatalf("id %s", id)
	}
}

func TestResumeEvictedAndUnknown(t *testing.T) {
	h := NewHubWithOptions(Options{RingFrames: 3})
	defer h.Close()
	for i := 0; i < 10; i++ {
		h.Publish("c", "r", TypeDelta, map[string]any{})
	}
	for _, tc := range []struct{ id, reason string }{
		{"r:2", ReasonEvicted},
		{"gone:1", ReasonUnknownRun},
		{"r:99", ReasonUnknownRun},
		{"garbage", ReasonUnknownRun},
	} {
		s := h.Subscribe("c", tc.id)
		f := next(t, s)
		if f.Type != TypeOverflow || !strings.Contains(string(f.Data), tc.reason) {
			t.Fatalf("%s: %s %s", tc.id, f.Type, f.Data)
		}
		if _, err := s.Next(nil, 0); !errors.Is(err, io.EOF) {
			t.Fatalf("want EOF got %v", err)
		}
		s.Close()
	}
	// Exactly at the edge of the ring is fine: r:7 -> 8,9,10.
	s := h.Subscribe("c", "r:7")
	defer s.Close()
	if id := next(t, s).ID(); id != "r:8" {
		t.Fatalf("id %s", id)
	}
	if h.Subscribers() != 1 {
		t.Fatalf("subscribers %d", h.Subscribers())
	}
}

func TestGracePurge(t *testing.T) {
	h := NewHubWithOptions(Options{Grace: 20 * time.Millisecond})
	defer h.Close()
	h.Publish("c", "r", TypeRunDone, map[string]any{})
	h.EndRun("c", "r")
	h.Publish("c", "r", TypeDelta, map[string]any{}) // dropped: run ended
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.chats)
		h.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run state not purged")
		}
		time.Sleep(5 * time.Millisecond)
	}
	s := h.Subscribe("c", "r:1")
	defer s.Close()
	if f := next(t, s); f.Type != TypeOverflow {
		t.Fatalf("got %s", f.Type)
	}
}

func TestIdleAndCancel(t *testing.T) {
	h := NewHub()
	defer h.Close()
	s := h.Subscribe("c", "")
	defer s.Close()
	if _, err := s.Next(nil, 10*time.Millisecond); !errors.Is(err, ErrIdle) {
		t.Fatalf("err %v", err)
	}
	done := make(chan struct{})
	close(done)
	if _, err := s.Next(done, 0); err == nil {
		t.Fatal("want cancel error")
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	h := NewHubWithOptions(Options{Grace: time.Millisecond})
	before := runtime.NumGoroutine()
	var wg sync.WaitGroup
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			run := string(rune('a' + p))
			for i := 0; i < 500; i++ {
				h.Publish("c", run, TypeDelta, map[string]any{"i": i})
			}
			h.EndRun("c", run)
		}(p)
	}
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := h.Subscribe("c", "")
			defer s.Close()
			for i := 0; i < 50; i++ {
				if _, err := s.Next(nil, 5*time.Millisecond); err != nil && !errors.Is(err, ErrIdle) {
					return
				}
			}
		}()
	}
	wg.Wait()
	h.Close()
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines %d -> %d", before, after)
	}
	if h.Subscribers() != 0 {
		t.Fatalf("subscribers %d", h.Subscribers())
	}
}

// readEvents parses an SSE body into (id, event, data) triples.
type sseEvent struct{ id, event, data string }

func readEvents(t *testing.T, body io.Reader, n int) ([]sseEvent, []string) {
	t.Helper()
	var evs []sseEvent
	var comments []string
	cur := sseEvent{}
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.event != "" {
				evs = append(evs, cur)
				cur = sseEvent{}
				if len(evs) == n {
					return evs, comments
				}
			}
		case strings.HasPrefix(line, ": "):
			comments = append(comments, line[2:])
		case strings.HasPrefix(line, "id: "):
			cur.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			cur.event = line[7:]
		case strings.HasPrefix(line, "data: "):
			cur.data = line[6:]
		}
	}
	return evs, comments
}

func TestServeSSEResumeAndKeepalive(t *testing.T) {
	h := NewHubWithOptions(Options{Keepalive: 20 * time.Millisecond})
	defer h.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream/{id}", func(w http.ResponseWriter, r *http.Request) { h.ServeSSE(w, r, r.PathValue("id")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	for i := 0; i < 3; i++ {
		h.Publish("c", "r", TypeDelta, map[string]any{"text": "t"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/stream/c", nil)
	req.Header.Set("Last-Event-ID", "r:1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("X-Accel-Buffering") != "no" || resp.Header.Get("Cache-Control") != "no-cache, no-transform" ||
		!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("headers %v", resp.Header)
	}
	go func() {
		time.Sleep(80 * time.Millisecond)
		h.Publish("c", "r", TypeRunDone, map[string]any{})
	}()
	evs, comments := readEvents(t, resp.Body, 3)
	if len(evs) != 3 || evs[0].id != "r:2" || evs[1].id != "r:3" || evs[2].id != "r:4" || evs[2].event != "run_done" {
		t.Fatalf("events %+v", evs)
	}
	hasKeepalive := false
	for _, c := range comments {
		if c == "keepalive" {
			hasKeepalive = true
		}
	}
	if !hasKeepalive {
		t.Fatalf("no keepalive in %v", comments)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for h.Subscribers() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscriber leaked after disconnect")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
