// Package events is the hub's global change bus: one publisher fans an event
// out to every subscriber's bounded queue, dropping that subscriber's oldest
// event when it falls behind rather than making anybody wait.
//
// The lossiness is a design decision, not an accident (spec.md 7.3). A
// subscriber that missed "connections changed" refetches and is correct again,
// so the bus may trade an old notification for liveness. What it may never
// carry is anything where each item matters — above all LLM token deltas: a
// dropped delta corrupts a visible message with no way for the reader to
// notice. That is spec.md N2, a hard rule, and the reason per-chat streaming
// (7.4) exists as a separate mechanism instead of riding on this one.
package events

import (
	"context"
	"log/slog"
	"sync"
)

// QueueSize is how far behind a subscriber may fall before it starts losing
// its oldest events. Matches the Python hub's EVENT_QUEUE_SIZE.
const QueueSize = 256

// Event types on the bus. Every one is an idempotent notification: it says
// something changed and names the thing, never what changed about it (N3).
const (
	TypeConnections = "connections"
	TypeCall        = "call"
	TypeAgent       = "agent"
	TypeGraph       = "graph"
	TypeChat        = "chat"
	TypeProfile     = "profile"
)

// Event is one notification. The optional ids let a subscriber refetch only the
// thing that changed; they are not content, and nothing incremental belongs in
// this struct.
type Event struct {
	Type    string `json:"type"`
	CallID  string `json:"callId,omitempty"`
	AgentID string `json:"agentId,omitempty"`
	ChatID  string `json:"chatId,omitempty"`
}

type subscriber struct {
	ch chan Event
}

// Bus fans events out to every current subscriber.
type Bus struct {
	mu   sync.Mutex
	subs map[*subscriber]struct{}
}

// NewBus returns a bus with no subscribers.
func NewBus() *Bus {
	return &Bus{subs: make(map[*subscriber]struct{})}
}

// Publish delivers ev to every subscriber. It never blocks: a subscriber whose
// queue is full loses its oldest event to make room for this one, on the
// reasoning that a stale notification is worth less than a fresh one.
//
// A nil bus publishes nowhere, so a component can be wired up without one.
func (b *Bus) Publish(ev Event) {
	if b == nil {
		return
	}
	// Publishers are serialised by this lock, which is what makes the
	// drop-oldest dance below safe; it only ever touches channels, never I/O.
	b.mu.Lock()
	defer b.mu.Unlock()
	for sub := range b.subs {
		select {
		case sub.ch <- ev:
		default:
			select {
			case <-sub.ch:
				slog.Debug("event subscriber is behind, dropping its oldest event")
			default:
			}
			select {
			case sub.ch <- ev:
			default:
			}
		}
	}
}

// Subscribe returns a channel of events and a function that stops the
// subscription and closes the channel. The channel is also closed when ctx is
// cancelled, so a request handler can subscribe and forget.
//
// Unsubscribing twice, or unsubscribing after ctx is done, is harmless.
func (b *Bus) Subscribe(ctx context.Context) (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, QueueSize)}

	b.mu.Lock()
	b.subs[sub] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	stop := make(chan struct{})
	unsubscribe := func() {
		once.Do(func() {
			close(stop)
			// Removing and closing under the same lock the publisher holds is
			// what guarantees no send can land on a closed channel.
			b.mu.Lock()
			defer b.mu.Unlock()
			delete(b.subs, sub)
			close(sub.ch)
		})
	}

	go func() {
		select {
		case <-ctx.Done():
			unsubscribe()
		case <-stop:
		}
	}()

	return sub.ch, unsubscribe
}

// Subscribers reports how many subscriptions are live, for tests and /api/stats.
func (b *Bus) Subscribers() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
