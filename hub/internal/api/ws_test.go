package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
)

// wsConn is a test client of GET /api/ws.
type wsConn struct {
	t *testing.T
	c *websocket.Conn
}

func dialWS(t *testing.T, srvURL string) *wsConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srvURL, "http")+"/api/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return &wsConn{t: t, c: c}
}

func (w *wsConn) send(v any) {
	w.t.Helper()
	b, _ := json.Marshal(v)
	if err := w.c.Write(context.Background(), websocket.MessageText, b); err != nil {
		w.t.Fatal(err)
	}
}

// next reads messages until one is not a heartbeat.
func (w *wsConn) next() wsOut {
	w.t.Helper()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, data, err := w.c.Read(ctx)
		cancel()
		if err != nil {
			w.t.Fatalf("read: %v", err)
		}
		var m wsOut
		if err := json.Unmarshal(data, &m); err != nil {
			w.t.Fatal(err)
		}
		if m.T != "hb" {
			return m
		}
	}
}

func TestWebSocketCarriesTheChangeFeedAndChatFrames(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	c := o.chat(t, a.ID, "t")
	srv := httptest.NewServer(o.handler)
	defer srv.Close()

	w := dialWS(t, srv.URL)
	if m := w.next(); m.T != "hello" {
		t.Fatalf("first message = %+v", m)
	}

	// The lossy change feed.
	o.bus.Publish(events.Event{Type: events.TypeDraft, ChatID: c.ID})
	m := w.next()
	if m.T != "event" || !strings.Contains(string(m.Data), `"type":"draft"`) || !strings.Contains(string(m.Data), c.ID) {
		t.Fatalf("event = %+v %s", m, m.Data)
	}

	// A chat subscription is live-only without "last", and resumes with it.
	o.hub.Publish(c.ID, "runA", "run_started", map[string]any{})
	o.hub.Publish(c.ID, "runA", "delta", map[string]any{"messageId": "m", "contentIndex": 0, "text": "1"})
	w.send(wsIn{Op: "sub", Chat: c.ID, Last: "runA:1"})
	m = w.next()
	if m.T != "frame" || m.Chat != c.ID || m.Name != "delta" || m.ID != "runA:2" || !strings.Contains(string(m.Data), `"text":"1"`) {
		t.Fatalf("resumed frame = %+v %s", m, m.Data)
	}
	o.hub.Publish(c.ID, "runA", "delta", map[string]any{"messageId": "m", "contentIndex": 0, "text": "2"})
	if m = w.next(); m.ID != "runA:3" {
		t.Fatalf("live frame = %+v", m)
	}

	// A position that cannot be honoured ends the subscription with an overflow.
	w.send(wsIn{Op: "sub", Chat: c.ID, Last: "nope:9"})
	if m = w.next(); m.T != "overflow" || m.Chat != c.ID || m.Reason == "" {
		t.Fatalf("overflow = %+v", m)
	}

	// After unsub, that chat's frames stop; the change feed does not.
	o.hub.EndRun(c.ID, "runA") // so a fresh subscription has nothing to replay
	w.send(wsIn{Op: "sub", Chat: c.ID})
	w.send(wsIn{Op: "unsub", Chat: c.ID})
	time.Sleep(50 * time.Millisecond)
	o.hub.Publish(c.ID, "runA", "delta", map[string]any{"messageId": "m", "contentIndex": 0, "text": "3"})
	o.bus.Publish(events.Event{Type: events.TypeChat, ChatID: c.ID})
	if m = w.next(); m.T != "event" {
		t.Fatalf("expected only the change-feed event, got %+v", m)
	}
}

func TestWebSocketRefusesAnotherOrigin(t *testing.T) {
	o := newOrch(t)
	srv := httptest.NewServer(o.handler)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/ws", &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Origin": {"https://evil.example"}},
	})
	if err == nil {
		t.Fatal("cross-origin upgrade was accepted")
	}
	if resp == nil || resp.StatusCode != 403 {
		t.Fatalf("response = %+v (%v)", resp, err)
	}
}

func TestWebSocketReplaysARunInFlightToALateReader(t *testing.T) {
	o := newOrch(t)
	a := o.agent(t, "a")
	c := o.chat(t, a.ID, "t")
	srv := httptest.NewServer(o.handler)
	defer srv.Close()

	// A reply is already streaming when the second tab opens.
	o.hub.Publish(c.ID, "runA", "run_started", map[string]any{})
	for _, text := range []string{"one ", "two "} {
		o.hub.Publish(c.ID, "runA", "delta", map[string]any{"messageId": "m", "contentIndex": 0, "text": text})
	}

	w := dialWS(t, srv.URL)
	w.next() // hello
	w.send(wsIn{Op: "sub", Chat: c.ID})
	for _, id := range []string{"runA:1", "runA:2", "runA:3"} {
		if m := w.next(); m.T != "frame" || m.ID != id {
			t.Fatalf("want replayed %s, got %+v", id, m)
		}
	}
	o.hub.Publish(c.ID, "runA", "delta", map[string]any{"messageId": "m", "contentIndex": 0, "text": "three"})
	if m := w.next(); m.ID != "runA:4" {
		t.Fatalf("live frame after the replay = %+v", m)
	}
}
