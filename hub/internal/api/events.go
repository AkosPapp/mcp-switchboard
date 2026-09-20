package api

import (
	"encoding/json"
	"net/http"
	"time"
)

// KeepaliveInterval is how long the stream may stay silent before it emits a
// comment. Proxies and browsers drop idle connections well before a quiet hub
// would otherwise say anything (spec.md N8).
const KeepaliveInterval = 15 * time.Second

// events is the global SSE change feed.
//
// What travels here is only ever an idempotent notification - "connections
// changed", "this call happened" - and never incremental content (N3). That is
// what makes the bus's drop-oldest behaviour correct: a subscriber that missed
// a notification refetches and is right again. Anything where every item
// matters, above all LLM token deltas, must not ride on this stream: a dropped
// delta corrupts a visible message with no way for the reader to notice it.
// That is N2, a hard rule, and the reason per-chat streaming (7.4) is a
// separate mechanism rather than another event type here.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	stream, unsubscribe := h.opts.Bus.Subscribe(ctx)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// nginx buffers a streamed response unless it is told not to, which turns
	// a live feed into one delivered in blocks minutes late.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher := http.NewResponseController(w)

	// Flush the headers immediately so the browser reports the stream as open
	// rather than waiting for the first thing that happens on the hub.
	if _, err := w.Write([]byte(": connected\n\n")); err != nil {
		return
	}
	_ = flusher.Flush()

	keepalive := time.NewTicker(h.opts.Keepalive)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			// The client went away: unsubscribe (deferred) and return, which is
			// what frees the request goroutine and the bus slot.
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
			if _, err := w.Write([]byte("data: " + string(payload) + "\n\n")); err != nil {
				return
			}
			// Flushed per event, not per batch: the whole point of the feed is
			// that the console reacts as things happen.
			_ = flusher.Flush()
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			_ = flusher.Flush()
		}
	}
}
