package events

import (
	"context"
	"sync"
	"testing"
	"time"
)

func recv(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed while an event was expected")
		}
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for an event")
		return Event{}
	}
}

func TestFanOut(t *testing.T) {
	bus := NewBus()
	ctx := context.Background()

	var channels []<-chan Event
	for i := 0; i < 3; i++ {
		ch, unsubscribe := bus.Subscribe(ctx)
		defer unsubscribe()
		channels = append(channels, ch)
	}
	if got := bus.Subscribers(); got != 3 {
		t.Fatalf("Subscribers = %d; want 3", got)
	}

	bus.Publish(Event{Type: TypeConnections})
	bus.Publish(Event{Type: TypeChat, ChatID: "chat-1"})

	for i, ch := range channels {
		if got := recv(t, ch); got.Type != TypeConnections {
			t.Fatalf("subscriber %d got %+v first", i, got)
		}
		got := recv(t, ch)
		if got.Type != TypeChat || got.ChatID != "chat-1" {
			t.Fatalf("subscriber %d got %+v second", i, got)
		}
	}
}

// A subscriber that never reads must not hold anybody up, and must end up with
// the newest events rather than the oldest: a stale notification is worth less
// than a fresh one, and either way the reader refetches.
func TestSlowSubscriberLosesOldestAndNeverBlocks(t *testing.T) {
	bus := NewBus()
	ctx := context.Background()

	slow, unsubscribeSlow := bus.Subscribe(ctx)
	defer unsubscribeSlow()
	fast, unsubscribeFast := bus.Subscribe(ctx)
	defer unsubscribeFast()

	const total = QueueSize * 3
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			bus.Publish(Event{Type: TypeAgent, AgentID: string(rune('a' + i%26))})
			// The fast subscriber is drained inline so the publisher is only
			// ever contending with the slow one.
			<-fast
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher blocked on a subscriber that stopped reading")
	}

	if got := len(slow); got != QueueSize {
		t.Fatalf("slow subscriber holds %d events; want a full queue of %d", got, QueueSize)
	}
	// What is left must be the tail of the stream, not its head.
	for i := total - QueueSize; i < total; i++ {
		want := string(rune('a' + i%26))
		if got := recv(t, slow); got.AgentID != want {
			t.Fatalf("event %d is %q; want %q — the queue kept the wrong end", i, got.AgentID, want)
		}
	}
}

func TestUnsubscribeClosesAndStopsDelivery(t *testing.T) {
	bus := NewBus()
	ch, unsubscribe := bus.Subscribe(context.Background())

	bus.Publish(Event{Type: TypeGraph})
	unsubscribe()
	unsubscribe() // idempotent

	if got := bus.Subscribers(); got != 0 {
		t.Fatalf("Subscribers = %d after unsubscribe; want 0", got)
	}

	// Events queued before the unsubscribe are still readable, then the
	// channel is closed rather than left dangling.
	if got := recv(t, ch); got.Type != TypeGraph {
		t.Fatalf("got %+v", got)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed after unsubscribe")
	}

	// A publish with the subscriber gone must not panic on the closed channel.
	bus.Publish(Event{Type: TypeConnections})
}

func TestContextCancelUnsubscribes(t *testing.T) {
	bus := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	ch, unsubscribe := bus.Subscribe(ctx)
	defer unsubscribe()

	cancel()
	for deadline := time.Now().Add(time.Second); bus.Subscribers() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("cancelling the context did not unsubscribe")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel should be closed once the context is done")
	}
}

func TestNilBusPublishesNowhere(t *testing.T) {
	var bus *Bus
	bus.Publish(Event{Type: TypeConnections})
	if got := bus.Subscribers(); got != 0 {
		t.Fatalf("Subscribers = %d", got)
	}
}

func TestConcurrentPublishAndSubscribe(t *testing.T) {
	bus := NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 500; n++ {
				bus.Publish(Event{Type: TypeCall, CallID: "c"})
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				ch, unsubscribe := bus.Subscribe(ctx)
				select {
				case <-ch:
				default:
				}
				unsubscribe()
			}
		}()
	}
	wg.Wait()
}
