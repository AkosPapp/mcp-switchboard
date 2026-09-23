package api

import (
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/config"
)

func withPushEnabled(opts *Options) {
	opts.Settings.PushVAPIDPublicKey = "test-public-key"
	opts.Settings.PushVAPIDPrivateKey = "test-private-key"
}

func TestPushRoutesAreNotRegisteredWithoutVAPIDKeys(t *testing.T) {
	f := newFixture(t, nil) // no VAPID keys in default settings

	rec := f.do(t, "GET", "/api/push/vapid-public-key", "")
	requireStatus(t, rec, 404)

	rec = f.do(t, "POST", "/api/push/subscribe", `{}`)
	requireStatus(t, rec, 404)

	rec = f.do(t, "DELETE", "/api/push/subscriptions/abc", "")
	requireStatus(t, rec, 404)
}

func TestPushVAPIDPublicKeyServesTheConfiguredKey(t *testing.T) {
	f := newFixture(t, withPushEnabled)

	rec := f.do(t, "GET", "/api/push/vapid-public-key", "")
	requireStatus(t, rec, 200)

	var body struct {
		PublicKey string `json:"publicKey"`
	}
	decode(t, rec, &body)
	if body.PublicKey != "test-public-key" {
		t.Fatalf("publicKey = %q, want test-public-key", body.PublicKey)
	}
}

func TestPushSubscribeStoresAndReturnsTheSubscription(t *testing.T) {
	f := newFixture(t, withPushEnabled)

	body := `{"endpoint":"https://push.example/abc","keys":{"p256dh":"p1","auth":"a1"}}`
	rec := f.do(t, "POST", "/api/push/subscribe", body)
	requireStatus(t, rec, 200)

	var got struct {
		ID       string `json:"id"`
		Endpoint string `json:"endpoint"`
	}
	decode(t, rec, &got)
	if got.Endpoint != "https://push.example/abc" {
		t.Fatalf("endpoint = %q", got.Endpoint)
	}
	if got.ID == "" {
		t.Fatal("expected a generated id")
	}
	if len(f.store.subs) != 1 {
		t.Fatalf("expected 1 stored subscription, got %d", len(f.store.subs))
	}
}

func TestPushSubscribeRejectsAnIncompleteBody(t *testing.T) {
	f := newFixture(t, withPushEnabled)

	for _, body := range []string{
		`{}`,
		`{"endpoint":"https://push.example/abc"}`,
		`{"endpoint":"https://push.example/abc","keys":{"p256dh":"p1"}}`,
	} {
		rec := f.do(t, "POST", "/api/push/subscribe", body)
		requireStatus(t, rec, 400)
	}
}

func TestPushUnsubscribeDeletesByID(t *testing.T) {
	f := newFixture(t, withPushEnabled)

	rec := f.do(t, "POST", "/api/push/subscribe",
		`{"endpoint":"https://push.example/abc","keys":{"p256dh":"p1","auth":"a1"}}`)
	requireStatus(t, rec, 200)
	var sub struct {
		ID string `json:"id"`
	}
	decode(t, rec, &sub)

	rec = f.do(t, "DELETE", "/api/push/subscriptions/"+sub.ID, "")
	requireStatus(t, rec, 204)

	if len(f.store.subs) != 0 {
		t.Fatalf("expected subscription to be deleted, got %d remaining", len(f.store.subs))
	}
}

func TestPushEnabledConfig(t *testing.T) {
	if (config.Settings{}).PushEnabled() {
		t.Fatal("zero-value settings must not report push enabled")
	}
	if !(config.Settings{PushVAPIDPublicKey: "a", PushVAPIDPrivateKey: "b"}).PushEnabled() {
		t.Fatal("settings with both keys must report push enabled")
	}
	if (config.Settings{PushVAPIDPublicKey: "a"}).PushEnabled() {
		t.Fatal("settings with only the public key must not report push enabled")
	}
}
