// Package push sends Web Push notifications (spec.md 8.7): a phone
// notification when a top-level human chat's run finishes, or when a run
// blocks on a human approval. Standard Web Push with VAPID, no third-party
// push SaaS - github.com/SherClockHolmes/webpush-go implements RFC 8291
// message encryption and the VAPID JWT (RFC 8292) directly against each
// browser's push service, which is all "standard Web Push" means.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Kind values for Payload.Kind, matching the two trigger points spec.md 8.7
// names: a top-level human chat's run reaching done/error, or entering
// blocked-on-approval.
const (
	KindRunDone          = "run_done"
	KindRunError         = "run_error"
	KindApprovalRequired = "approval_required"
	// KindQuestion: the model asked the user something and waits (switchboard.user.ask).
	KindQuestion = "question"
)

// Payload is the JSON body of every push message, exactly the shape spec.md
// 8.7 specifies: {title, body, chatId, kind}. The service worker's push
// handler (hub/web/public/sw.js) reads it straight off event.data.json().
type Payload struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	ChatID string `json:"chatId"`
	Kind   string `json:"kind"`
}

// Subscriptions is the slice of the store this package needs. Narrow, the
// same reason api.CallReader is narrow: a test wants a handful of fake rows,
// not a whole SQLite file.
type Subscriptions interface {
	ListPushSubscriptions(ctx context.Context) ([]store.PushSubscription, error)
	DeletePushSubscriptionByEndpoint(ctx context.Context, endpoint string) error
}

// Config is the VAPID identity every outgoing message signs with.
type Config struct {
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	// Subscriber is the contact URI (mailto: or https:) a push service can
	// use to reach the sender if it needs to, per RFC 8292. It has no effect
	// on delivery; some push services just require the claim to be present.
	Subscriber string
}

// Enabled reports whether both VAPID keys are configured. With either
// missing, push is off: Sender.Send becomes a no-op and the HTTP routes that
// construct a Sender should not be registered at all (spec.md 8.7 - "the
// routes 404 and no push is attempted").
func (c Config) Enabled() bool { return c.VAPIDPublicKey != "" && c.VAPIDPrivateKey != "" }

// sendFunc matches webpush.SendNotificationWithContext's signature, so a test
// can swap in a fake instead of making a real HTTP call to a browser's push
// service.
type sendFunc func(ctx context.Context, message []byte, s *webpush.Subscription, options *webpush.Options) (*http.Response, error)

// Sender fans a payload out to every registered subscription.
type Sender struct {
	store  Subscriptions
	cfg    Config
	logger *slog.Logger
	send   sendFunc
}

// New builds a Sender. logger may be nil (slog.Default() is used).
func New(st Subscriptions, cfg Config, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sender{store: st, cfg: cfg, logger: logger, send: webpush.SendNotificationWithContext}
}

// Enabled reports whether this Sender can actually send anything.
func (s *Sender) Enabled() bool { return s.cfg.Enabled() }

// Send fans payload out to every subscription, best-effort: one subscriber's
// failure never stops delivery to the rest, and a push service answering
// 404/410 (Gone - the browser unsubscribed) deletes that subscription from
// the store so the next run does not keep retrying a dead endpoint. It
// returns the errors seen, if any, purely for logging by the caller; a
// notification is a usability feature, not something a run should fail over.
func (s *Sender) Send(ctx context.Context, payload Payload) []error {
	if !s.Enabled() {
		return nil
	}
	subs, err := s.store.ListPushSubscriptions(ctx)
	if err != nil {
		return []error{fmt.Errorf("push: listing subscriptions: %w", err)}
	}
	if len(subs) == 0 {
		return nil
	}

	message, err := json.Marshal(payload)
	if err != nil {
		return []error{fmt.Errorf("push: encoding payload: %w", err)}
	}

	var errs []error
	for _, sub := range subs {
		if err := s.sendOne(ctx, sub, message); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (s *Sender) sendOne(ctx context.Context, sub store.PushSubscription, message []byte) error {
	resp, err := s.send(ctx, message, &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpush.Keys{
			Auth:   sub.Auth,
			P256dh: sub.P256dh,
		},
	}, &webpush.Options{
		Subscriber:      s.cfg.Subscriber,
		VAPIDPublicKey:  s.cfg.VAPIDPublicKey,
		VAPIDPrivateKey: s.cfg.VAPIDPrivateKey,
		TTL:             ttlSeconds,
	})
	if err != nil {
		s.logger.Warn("push: could not reach a subscriber's push service", "error", err)
		return err
	}
	defer resp.Body.Close()

	// 404/410 mean the push service has permanently discarded the
	// subscription (the user revoked permission, cleared site data, or the
	// service worker was unregistered). Anything else that is not 2xx is
	// left alone: it may be transient, and deleting a subscription over a
	// temporary 5xx would silently stop notifying a device that did nothing
	// wrong.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		if delErr := s.store.DeletePushSubscriptionByEndpoint(ctx, sub.Endpoint); delErr != nil {
			s.logger.Warn("push: could not remove a dead subscription", "error", delErr)
		}
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push: subscriber's push service answered %d", resp.StatusCode)
	}
	return nil
}

// ttlSeconds bounds how long a push service holds a message for an offline
// device before giving up. A day is plenty for "your run finished" - if the
// device has been offline longer than that, the chat will still say so when
// it reconnects.
const ttlSeconds = 24 * 60 * 60

// ErrPushDisabled is returned by callers that want to distinguish "not
// configured" from every other failure, without depending on this package's
// internals.
var ErrPushDisabled = errors.New("push: not configured")

// IsTopLevelHumanChat is the trigger condition of spec.md 8.7: a push is only
// ever sent for the top-level human chat of the run that transitioned, never
// for a descendant/sub-chat. A chat is top-level and human when both hold:
//
//   - chatKind == "human" - it is a chat with a person, not one leg of an
//     agent<->agent exchange (kind "agent") and not a spawn conversation
//     (kind "spawn"), per spec.md 5.2's `chats.kind` enum.
//   - agentIsRoot is true - the chat's owning agent has no parent. A human
//     chat belonging to a spawned child agent is still a sub-chat of the
//     tree the top-level run belongs to, and firing a phone notification for
//     every child agent's turn would be exactly the noise spec.md 8.7 rules
//     out ("NOT every descendant sub-chat").
func IsTopLevelHumanChat(chatKind string, agentIsRoot bool) bool {
	return chatKind == "human" && agentIsRoot
}
