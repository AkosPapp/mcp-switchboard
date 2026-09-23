package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/chatstream"
)

// GET /api/ws: the console's one live connection.
//
// Browsers allow only about six HTTP/1.1 connections per host, and the
// console used to hold two server-sent-event streams open per tab (the change
// feed and the open chat's stream). A third tab exhausted the pool, and the
// next tab's ordinary requests then queued behind streams that never end:
// history that did not load, reloads that hung. A WebSocket is not part of
// that pool, so both feeds now travel over a single one per tab.
//
// The two feeds keep their separate guarantees (N2): the change feed is lossy
// (a slow client loses its oldest notification and refetches), while a chat
// subscription is lossless or ends in an explicit `overflow`. The SSE routes
// remain for anything that is not the console.
//
// Messages are JSON text frames.
//
//	client -> hub  {"op":"sub","chat":ID,"last":"runId:seq"}   start (or restart) a chat subscription; with no "last",
//	                                                            runs still in flight are replayed from their first frame
//	               {"op":"unsub","chat":ID}
//	hub -> client  {"t":"hello"}                               the connection is up
//	               {"t":"event","data":{...}}                  a change-feed event
//	               {"t":"frame","chat":ID,"name":TYPE,"id":"runId:seq","data":{...}}
//	               {"t":"overflow","chat":ID,"reason":R}       the subscription ended; refetch, resubscribe with no "last"
//	               {"t":"hb"}                                  heartbeat, so a dead link is noticed

const (
	wsWriteTimeout = 10 * time.Second
	wsReadLimit    = 4 << 10
)

type wsIn struct {
	Op   string `json:"op"`
	Chat string `json:"chat"`
	Last string `json:"last"`
}

type wsOut struct {
	T      string          `json:"t"`
	Chat   string          `json:"chat,omitempty"`
	Name   string          `json:"name,omitempty"`
	ID     string          `json:"id,omitempty"`
	Reason string          `json:"reason,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

func (h *Handler) ws(w http.ResponseWriter, r *http.Request) {
	// Accept refuses a cross-origin upgrade by default, which is what keeps
	// another site's page from listening in on the console's feeds.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		h.log.Warn("console websocket handshake failed", "error", err)
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(wsReadLimit)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	send := func(m wsOut) {
		b, err := json.Marshal(m)
		if err != nil {
			h.log.Warn("could not encode a websocket message", "error", err)
			return
		}
		wctx, done := context.WithTimeout(ctx, wsWriteTimeout)
		defer done()
		if err := c.Write(wctx, websocket.MessageText, b); err != nil {
			cancel() // a client that cannot take a write is gone, or too slow to be served
		}
	}

	// Subscribed before "hello", so hello means the feed is already listening.
	stream, unsubscribe := h.opts.Bus.Subscribe(ctx)
	defer unsubscribe()
	send(wsOut{T: "hello"})

	var wg sync.WaitGroup
	defer wg.Wait()

	// The change feed.
	wg.Add(1)
	go func() {
		defer wg.Done()
		beat := time.NewTicker(h.opts.Keepalive)
		defer beat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-stream:
				if !ok {
					return
				}
				payload, err := json.Marshal(ev)
				if err != nil {
					h.log.Warn("could not encode an event", "error", err)
					continue
				}
				send(wsOut{T: "event", Data: payload})
			case <-beat.C:
				send(wsOut{T: "hb"})
			}
		}
	}()

	// Chat subscriptions, by chat id. Subscribing again replaces the old one.
	var mu sync.Mutex
	subs := map[string]context.CancelFunc{}
	stop := func(chat string) {
		mu.Lock()
		defer mu.Unlock()
		if cancelSub, ok := subs[chat]; ok {
			cancelSub()
			delete(subs, chat)
		}
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for _, cancelSub := range subs {
			cancelSub()
		}
	}()

	start := func(chat, last string) {
		if h.opts.Streams == nil {
			send(wsOut{T: "overflow", Chat: chat, Reason: "unavailable"})
			return
		}
		subCtx, cancelSub := context.WithCancel(ctx)
		mu.Lock()
		if old, ok := subs[chat]; ok {
			old()
		}
		subs[chat] = cancelSub
		mu.Unlock()

		// No resume position: a late reader gets the runs in flight replayed from
		// their start, so a tab opened mid-reply shows the whole reply.
		var sub *chatstream.Subscription
		if last == "" {
			sub = h.opts.Streams.SubscribeActive(chat)
		} else {
			sub = h.opts.Streams.Subscribe(chat, last)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer sub.Close()
			defer cancelSub()
			for {
				f, err := sub.Next(subCtx.Done(), 0)
				if err != nil {
					return // cancelled, or the subscription ended after draining
				}
				if f.Type == chatstream.TypeOverflow {
					var body struct {
						Reason string `json:"reason"`
					}
					_ = json.Unmarshal(f.Data, &body)
					send(wsOut{T: "overflow", Chat: chat, Reason: body.Reason})
					return
				}
				send(wsOut{T: "frame", Chat: chat, Name: f.Type, ID: f.ID(), Data: f.Data})
			}
		}()
	}

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) && websocket.CloseStatus(err) == -1 {
				h.log.Debug("console websocket closed", "error", err)
			}
			return
		}
		var in wsIn
		if json.Unmarshal(data, &in) != nil {
			continue // not ours; nothing useful to say back
		}
		switch in.Op {
		case "sub":
			if in.Chat != "" {
				start(in.Chat, in.Last)
			}
		case "unsub":
			stop(in.Chat)
		}
	}
}
