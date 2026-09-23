package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// --------------------------------------------------------------------------
// Web Push (spec.md 8.7)
// --------------------------------------------------------------------------

// pushVAPIDPublicKey answers the VAPID public key the console needs to call
// pushManager.subscribe(). Only ever registered when both VAPID keys are
// configured (see New), so this handler can assume it has one to serve.
func (h *Handler) pushVAPIDPublicKey(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"publicKey": h.opts.Settings.PushVAPIDPublicKey})
}

// subscribeRequest is the body POSTed by PushManager.subscribe()'s result:
// browsers hand back {endpoint, keys: {p256dh, auth}}, plus an optional
// operator-facing label the console may attach ("this device").
type subscribeRequest struct {
	Endpoint string  `json:"endpoint"`
	Keys     keys    `json:"keys"`
	Label    *string `json:"label"`
}

type keys struct {
	P256dh string `json:"p256dh"`
	Auth   string `json:"auth"`
}

type subscriptionJSON struct {
	ID         string  `json:"id"`
	Endpoint   string  `json:"endpoint"`
	CreatedAt  string  `json:"createdAt"`
	LastSeenAt string  `json:"lastSeenAt"`
	Label      *string `json:"label,omitempty"`
}

func toSubscriptionJSON(sub store.PushSubscription) subscriptionJSON {
	return subscriptionJSON{
		ID:         sub.ID,
		Endpoint:   sub.Endpoint,
		CreatedAt:  store.FormatTime(sub.CreatedAt),
		LastSeenAt: store.FormatTime(sub.LastSeenAt),
		Label:      sub.Label,
	}
}

func (h *Handler) pushSubscribe(w http.ResponseWriter, r *http.Request) {
	var body subscribeRequest
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Endpoint == "" || body.Keys.P256dh == "" || body.Keys.Auth == "" {
		writeError(w, http.StatusBadRequest, "endpoint and keys.p256dh and keys.auth are required")
		return
	}

	sub, err := h.opts.PushStore.UpsertPushSubscription(r.Context(), store.PushSubscription{
		ID:       uuid.New().String(),
		Endpoint: body.Endpoint,
		P256dh:   body.Keys.P256dh,
		Auth:     body.Keys.Auth,
		Label:    body.Label,
	})
	if err != nil {
		h.log.Error("could not store a push subscription", "error", err)
		writeError(w, http.StatusInternalServerError, "could not save the subscription")
		return
	}
	writeJSON(w, http.StatusOK, toSubscriptionJSON(sub))
}

func (h *Handler) pushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.opts.PushStore.DeletePushSubscription(r.Context(), id); err != nil {
		h.log.Error("could not delete a push subscription", "error", err)
		writeError(w, http.StatusInternalServerError, "could not delete the subscription")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
