package api

import (
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The /v1/assistant routes are the panewire (c) surface (MGH-36): a pending
// work list, the notification outbox, and the assistant resolve. They share
// the flat bearer token; attribution (by, responder) is fixed server-side —
// caller fields carrying it are decoded only so an old client does not 400,
// and are never read.

func (s Server) assistantPending(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	out, err := s.Service.Store.ListAssistantPending(r.Context())
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, out)
}

func (s Server) assistantOutboxList(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	limit, err := queryLimit(r, 200, 1000)
	if err != nil {
		appErr(w, err)
		return
	}
	xs, err := s.Service.Store.ListUnsentNotifications(r.Context(), limit)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"notifications": xs})
}

type outboxSentInput struct {
	EventID  string `json:"event_id"`
	HubRowID int64  `json:"hub_row_id"`
}

// assistantOutboxSent records the hub receipt for one outbox row. The mark
// is idempotent per event_id; hub_row_id must be the row hub persisted — a
// duplicate answer that returned id 0 is not a receipt and never marks sent.
func (s Server) assistantOutboxSent(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	defer r.Body.Close()
	var input outboxSentInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	x, err := s.Service.Store.MarkNotificationSent(r.Context(), input.EventID, input.HubRowID)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}

// assistantResolveInput is the assistant resolve body. Responder and By are
// declared so a caller echoing them is not a malformed request; the server
// never reads them — By is the token's client id and the recorded responder
// is always operator-via-berry.
type assistantResolveInput struct {
	RequestID string `json:"request_id"`
	Kind      string `json:"kind,omitempty"`
	Option    string `json:"option,omitempty"`
	Text      string `json:"text,omitempty"`
	Responder string `json:"responder,omitempty"`
	By        string `json:"by,omitempty"`
}

func (s Server) assistantDecisionResolve(w http.ResponseWriter, r *http.Request) {
	client, ok := s.auth(w, r)
	if !ok {
		return
	}
	defer r.Body.Close()
	var input assistantResolveInput
	if err := decode(r, &input, store.MaxBytes); err != nil {
		appErr(w, err)
		return
	}
	taskID, _, ok := store.ParseDecisionRequestID(input.RequestID)
	if !ok {
		appErr(w, store.ErrInvalidDecisionRequest)
		return
	}
	x, err := s.Service.Store.ResolveDecisionRequestAssistant(r.Context(), taskID, client, store.DecisionAssistantResolveInput{RequestID: input.RequestID, Kind: input.Kind, Option: input.Option, Text: input.Text})
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, x)
}
