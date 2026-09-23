package store

import (
	"context"
	"testing"
)

func TestUpsertPushSubscriptionInsertsThenUpdatesByEndpoint(t *testing.T) {
	s := openStore(t, tempDB(t))
	ctx := context.Background()

	label := "phone"
	first, err := s.UpsertPushSubscription(ctx, PushSubscription{
		ID:       "sub-1",
		Endpoint: "https://push.example/abc",
		P256dh:   "p256dh-1",
		Auth:     "auth-1",
		Label:    &label,
	})
	if err != nil {
		t.Fatalf("UpsertPushSubscription (insert): %v", err)
	}
	if first.ID != "sub-1" {
		t.Fatalf("ID = %q, want sub-1", first.ID)
	}
	if first.Endpoint != "https://push.example/abc" {
		t.Fatalf("Endpoint = %q", first.Endpoint)
	}

	all, err := s.ListPushSubscriptions(ctx)
	if err != nil {
		t.Fatalf("ListPushSubscriptions: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 subscription, got %d", len(all))
	}

	// Re-subscribing the same endpoint (browser re-prompted, keys rotated)
	// must update in place, not create a second row, and must keep the
	// original id.
	second, err := s.UpsertPushSubscription(ctx, PushSubscription{
		ID:       "sub-2-ignored", // a fresh id from the caller; must not win
		Endpoint: "https://push.example/abc",
		P256dh:   "p256dh-2",
		Auth:     "auth-2",
	})
	if err != nil {
		t.Fatalf("UpsertPushSubscription (update): %v", err)
	}
	if second.ID != "sub-1" {
		t.Fatalf("ID after re-subscribe = %q, want sub-1 (kept)", second.ID)
	}
	if second.P256dh != "p256dh-2" || second.Auth != "auth-2" {
		t.Fatalf("keys not refreshed: %+v", second)
	}

	all, err = s.ListPushSubscriptions(ctx)
	if err != nil {
		t.Fatalf("ListPushSubscriptions: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("re-subscribing must not duplicate rows, got %d", len(all))
	}
}

func TestDeletePushSubscriptionByID(t *testing.T) {
	s := openStore(t, tempDB(t))
	ctx := context.Background()

	sub, err := s.UpsertPushSubscription(ctx, PushSubscription{
		ID: "sub-1", Endpoint: "https://push.example/1", P256dh: "p", Auth: "a",
	})
	if err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}

	if err := s.DeletePushSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("DeletePushSubscription: %v", err)
	}
	all, err := s.ListPushSubscriptions(ctx)
	if err != nil {
		t.Fatalf("ListPushSubscriptions: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0 subscriptions after delete, got %d", len(all))
	}

	// Deleting an id that no longer exists is not an error.
	if err := s.DeletePushSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("deleting an already-deleted id should be a no-op, got: %v", err)
	}
}

func TestDeletePushSubscriptionByEndpoint(t *testing.T) {
	s := openStore(t, tempDB(t))
	ctx := context.Background()

	if _, err := s.UpsertPushSubscription(ctx, PushSubscription{
		ID: "sub-1", Endpoint: "https://push.example/gone", P256dh: "p", Auth: "a",
	}); err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}
	if _, err := s.UpsertPushSubscription(ctx, PushSubscription{
		ID: "sub-2", Endpoint: "https://push.example/kept", P256dh: "p", Auth: "a",
	}); err != nil {
		t.Fatalf("UpsertPushSubscription: %v", err)
	}

	if err := s.DeletePushSubscriptionByEndpoint(ctx, "https://push.example/gone"); err != nil {
		t.Fatalf("DeletePushSubscriptionByEndpoint: %v", err)
	}

	all, err := s.ListPushSubscriptions(ctx)
	if err != nil {
		t.Fatalf("ListPushSubscriptions: %v", err)
	}
	if len(all) != 1 || all[0].Endpoint != "https://push.example/kept" {
		t.Fatalf("unexpected remaining subscriptions: %+v", all)
	}
}

func TestListPushSubscriptionsEmpty(t *testing.T) {
	s := openStore(t, tempDB(t))
	all, err := s.ListPushSubscriptions(context.Background())
	if err != nil {
		t.Fatalf("ListPushSubscriptions: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected no subscriptions, got %d", len(all))
	}
}
