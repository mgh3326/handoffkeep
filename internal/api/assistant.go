package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The applied receipt is attributed to the authenticated client, never the
// caller-supplied identity.

// outboxAppliedInput is the applied body. By is declared so a caller echoing
// it is not a malformed request; the server never reads it — the receipt is
// attributed to the token's client id, like every assistant route.
type outboxAppliedInput struct {
	EventID string `json:"event_id"`
	By      string `json:"by,omitempty"`
}

// assistantOutboxApplied records the receiving lane's ack for one outbox row,
// the applied receipt after the hub's received one. The ack is attributed to
// the authenticated client, idempotent per (event_id, client) and refused
// until the hub has accepted the event.
func (s Server) assistantOutboxApplied(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input outboxAppliedInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, r, err)
		return
	}
	x, err := s.Service.Store.MarkNotificationApplied(r.Context(), input.EventID, client)
	if err != nil {
		appErr(w, r, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}
