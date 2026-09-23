package chatstream

import (
	"errors"
	"io"
	"net/http"
)

// ServeSSE serves GET /api/chats/{id}/stream. The resume position is the
// Last-Event-ID header (or, for clients that cannot set it, ?lastEventId=).
//
// After an `overflow` event the server ends the stream; the client must close
// its EventSource, refetch the chat's messages and reconnect without a
// Last-Event-ID (the overflow event deliberately carries no id).
func (h *Hub) ServeSSE(w http.ResponseWriter, r *http.Request, chatID string) {
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = r.URL.Query().Get("lastEventId")
	}
	sub := h.Subscribe(chatID, last)
	defer sub.Close()

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache, no-transform")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if _, err := w.Write([]byte(": connected\n\n")); err != nil {
		return
	}
	_ = rc.Flush()

	for {
		f, err := sub.Next(r.Context().Done(), h.opts.Keepalive)
		switch {
		case err == nil:
			if _, werr := w.Write(f.wire); werr != nil {
				return
			}
			_ = rc.Flush()
		case errors.Is(err, ErrIdle):
			if _, werr := w.Write([]byte(": keepalive\n\n")); werr != nil {
				return
			}
			_ = rc.Flush()
		case errors.Is(err, io.EOF):
			return
		default:
			return
		}
	}
}
