package ui

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const decisionResolveBodyLimit = 4 * 1024

// decisionResolve answers a structured decision request from the console
// (hk 3061). Only an Access email identity is a verified operator path, so
// the answer is operator_direct; a service identity is refused. Body:
// {request_id, option, text, event_key}. event_key is required: a click that
// is submitted twice replays the first result instead of answering again.
// The status and result table is api.DecisionResolveResponse, shared with the
// bearer routes. The page that calls this route is hk 3063.
func (h *Handler) decisionResolve(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	outcome := writeOutcome{action: "decision-resolve", target: "-", eventID: "-", result: "invalid"}
	defer func() {
		h.audit(apiIdentity(identity), outcome.action, outcome.target, outcome.eventID, outcome.result)
	}()

	if identity.ServiceName != "" {
		outcome.result = "service_reject"
		writeJSONError(w, http.StatusForbidden, "operator_only")
		return
	}
	if !sameOrigin(r) {
		outcome.result = "origin_reject"
		writeJSONError(w, http.StatusForbidden, "origin_rejected")
		return
	}
	if !h.validHeaderCSRF(r, identity.Email) {
		outcome.result = "csrf_reject"
		writeJSONError(w, http.StatusForbidden, "csrf_rejected")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, decisionResolveBodyLimit)
	var input struct {
		RequestID string `json:"request_id"`
		Option    string `json:"option"`
		Text      string `json:"text"`
		EventKey  string `json:"event_key"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	outcome.target = auditTarget(input.RequestID)
	if input.EventKey == "" {
		writeJSONError(w, http.StatusBadRequest, "event_key_required")
		return
	}
	outcome.eventID = auditTarget(input.EventKey)

	out, err := h.store.ResolveDecision(r.Context(), store.ResolveDecisionInput{
		RequestID: input.RequestID, Kind: store.DecisionRequestAnswered, Option: input.Option, Text: input.Text,
		EventKey: input.EventKey, Channel: store.DecisionChannelConsole, Actor: apiIdentity(identity), Verified: true,
	})
	if err != nil {
		outcome.result = decisionResolveError(w, err)
		return
	}
	outcome.result = out.Result
	status, body := api.DecisionResolveResponse(out)
	writeBoardJSONStatus(w, status, body)
}

// auditTarget bounds a caller-supplied value before it reaches the audit line.
func auditTarget(value string) string {
	if len(value) > 128 {
		return value[:128]
	}
	return value
}

// decisionResolveError answers a failure of ResolveDecision and returns the
// audit result. A rejected request keeps the store's code; anything else is
// an infrastructure failure, logged and answered 500.
func decisionResolveError(w http.ResponseWriter, err error) string {
	switch {
	case uiWrkError(w, err):
		return "wrk_reject"
	case errors.Is(err, store.ErrTaskNotFound):
		writeJSONError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, store.ErrDecisionEventKeyConflict):
		writeJSONError(w, http.StatusConflict, "decision_event_key_conflict")
	case errors.Is(err, store.ErrInvalidDecisionRequest):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	default:
		if _, ok := guard.SecretPattern(err); ok {
			writeJSONError(w, http.StatusBadRequest, "secret_like_content")
			return "invalid"
		}
		log.Printf("ui.decision_resolve_failed err=%q", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return "error"
	}
	return "invalid"
}
