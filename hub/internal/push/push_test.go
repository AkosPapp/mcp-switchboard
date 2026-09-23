package push

import (
	"context"
	"errors"
	"net/http"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestIsTopLevelHumanChat(t *testing.T) {
	cases := []struct {
		name        string
		chatKind    string
		agentIsRoot bool
		want        bool
	}{
		{"root agent, human chat", "human", true, true},
		{"root agent, agent chat", "agent", true, false},
		{"root agent, spawn chat", "spawn", true, false},
		{"child agent, human chat", "human", false, false},
		{"child agent, agent chat", "agent", false, false},
		{"child agent, spawn chat", "spawn", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTopLevelHumanChat(c.chatKind, c.agentIsRoot); got != c.want {
				t.Errorf("IsTopLevelHumanChat(%q, %v) = %v, want %v", c.chatKind, c.agentIsRoot, got, c.want)
			}
		})
	}
}

// fakeStore is a minimal Subscriptions for testing Send's fan-out and
// 404/410 cleanup behaviour without a real database.
type fakeStore struct {
	subs    []store.PushSubscription
	deleted []string
}

func (f *fakeStore) ListPushSubscriptions(ctx context.Context) ([]store.PushSubscription, error) {
	return f.subs, nil
}

func (f *fakeStore) DeletePushSubscriptionByEndpoint(ctx context.Context, endpoint string) error {
	f.deleted = append(f.deleted, endpoint)
	return nil
}

func resp(status int) *http.Response {
	return &http.Response{StatusCode: status, Body: http.NoBody}
}

func TestSendDisabledIsANoOp(t *testing.T) {
	fs := &fakeStore{subs: []store.PushSubscription{{ID: "1", Endpoint: "https://push.example/1"}}}
	s := New(fs, Config{}, nil)
	called := false
	s.send = func(ctx context.Context, message []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error) {
		called = true
		return resp(201), nil
	}
	if s.Enabled() {
		t.Fatal("expected Send to be disabled with no VAPID keys")
	}
	if errs := s.Send(context.Background(), Payload{Title: "x"}); errs != nil {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if called {
		t.Fatal("send should never be invoked while disabled")
	}
}

func TestSendFansOutToEverySubscription(t *testing.T) {
	fs := &fakeStore{subs: []store.PushSubscription{
		{ID: "1", Endpoint: "https://push.example/1", P256dh: "p1", Auth: "a1"},
		{ID: "2", Endpoint: "https://push.example/2", P256dh: "p2", Auth: "a2"},
	}}
	s := New(fs, Config{VAPIDPublicKey: "pub", VAPIDPrivateKey: "priv"}, nil)

	var seen []string
	s.send = func(ctx context.Context, message []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error) {
		seen = append(seen, sub.Endpoint)
		if opts.VAPIDPublicKey != "pub" || opts.VAPIDPrivateKey != "priv" {
			t.Errorf("VAPID keys not passed through: %+v", opts)
		}
		return resp(201), nil
	}

	if errs := s.Send(context.Background(), Payload{Title: "done", ChatID: "c1", Kind: KindRunDone}); errs != nil {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(seen) != 2 {
		t.Fatalf("expected both subscriptions to be sent to, got %v", seen)
	}
	if len(fs.deleted) != 0 {
		t.Fatalf("nothing should have been deleted, got %v", fs.deleted)
	}
}

func TestSendDeletesSubscriptionOn404And410(t *testing.T) {
	fs := &fakeStore{subs: []store.PushSubscription{
		{ID: "1", Endpoint: "https://push.example/gone-404"},
		{ID: "2", Endpoint: "https://push.example/gone-410"},
		{ID: "3", Endpoint: "https://push.example/alive"},
	}}
	s := New(fs, Config{VAPIDPublicKey: "pub", VAPIDPrivateKey: "priv"}, nil)
	s.send = func(ctx context.Context, message []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error) {
		switch sub.Endpoint {
		case "https://push.example/gone-404":
			return resp(http.StatusNotFound), nil
		case "https://push.example/gone-410":
			return resp(http.StatusGone), nil
		default:
			return resp(http.StatusCreated), nil
		}
	}

	s.Send(context.Background(), Payload{Title: "x"})

	if len(fs.deleted) != 2 {
		t.Fatalf("expected 2 deletions, got %v", fs.deleted)
	}
	want := map[string]bool{"https://push.example/gone-404": true, "https://push.example/gone-410": true}
	for _, e := range fs.deleted {
		if !want[e] {
			t.Errorf("unexpected deletion: %s", e)
		}
	}
}

func TestSendKeepsSubscriptionOnTransientError(t *testing.T) {
	fs := &fakeStore{subs: []store.PushSubscription{{ID: "1", Endpoint: "https://push.example/1"}}}
	s := New(fs, Config{VAPIDPublicKey: "pub", VAPIDPrivateKey: "priv"}, nil)
	s.send = func(ctx context.Context, message []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error) {
		return resp(http.StatusInternalServerError), nil
	}

	errs := s.Send(context.Background(), Payload{Title: "x"})
	if len(errs) != 1 {
		t.Fatalf("expected one error, got %v", errs)
	}
	if len(fs.deleted) != 0 {
		t.Fatalf("a 500 must not delete the subscription, got %v", fs.deleted)
	}
}

func TestSendContinuesAfterOneSubscriberFails(t *testing.T) {
	fs := &fakeStore{subs: []store.PushSubscription{
		{ID: "1", Endpoint: "https://push.example/fails"},
		{ID: "2", Endpoint: "https://push.example/ok"},
	}}
	s := New(fs, Config{VAPIDPublicKey: "pub", VAPIDPrivateKey: "priv"}, nil)
	var okSent bool
	s.send = func(ctx context.Context, message []byte, sub *webpush.Subscription, opts *webpush.Options) (*http.Response, error) {
		if sub.Endpoint == "https://push.example/fails" {
			return nil, errors.New("network unreachable")
		}
		okSent = true
		return resp(201), nil
	}

	errs := s.Send(context.Background(), Payload{Title: "x"})
	if len(errs) != 1 {
		t.Fatalf("expected exactly one error, got %v", errs)
	}
	if !okSent {
		t.Fatal("the second subscriber should still have been sent to")
	}
}
