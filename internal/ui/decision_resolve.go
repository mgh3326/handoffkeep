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

	var input struct {
		RequestID string `json:"request_id"`
		Option    string `json:"option"`
		Text      string `json:"text"`
		EventKey  string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, &outcome, &input, &input.EventKey) {
		return
	}
	outcome.target = auditTarget(input.RequestID)

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

// decisionDeny records the operator's [아님] for one pending_confirmation
// attempt of the current request (hk 3163): a denied attempt of its own, the
// request left open. Body: {request_id, attempt_id, receipt, event_key};
// receipt is the operator's optional reason. Guards and the status table are
// those of decisionResolve; a deny answers 200 result "denied".
func (h *Handler) decisionDeny(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	outcome := writeOutcome{action: "decision-deny", target: "-", eventID: "-", result: "invalid"}
	defer func() {
		h.audit(apiIdentity(identity), outcome.action, outcome.target, outcome.eventID, outcome.result)
	}()

	var input struct {
		RequestID string `json:"request_id"`
		AttemptID int64  `json:"attempt_id"`
		Receipt   string `json:"receipt"`
		EventKey  string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, &outcome, &input, &input.EventKey) {
		return
	}
	outcome.target = auditTarget(input.RequestID)

	out, err := h.store.ResolveDecision(r.Context(), store.ResolveDecisionInput{
		RequestID: input.RequestID, Kind: store.DecisionKindDeny, DeniesAttemptID: input.AttemptID, Receipt: input.Receipt,
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

// decodeOperatorWrite applies the console operator routes' guards (an Access
// email identity, same origin, header CSRF) and decodes the size-capped body
// into input, rejecting unknown fields and trailing data. It answers the
// refusal itself and reports whether the handler may go on.
func (h *Handler) decodeOperatorWrite(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity, outcome *writeOutcome, input any, eventKey *string) bool {
	return h.decodeOperatorWriteLimited(w, r, identity, outcome, input, eventKey, decisionResolveBodyLimit)
}

// decodeOperatorWriteLimited is decodeOperatorWrite with the body cap chosen
// by the route: the shared 4 KiB default unless a batch route needs more.
func (h *Handler) decodeOperatorWriteLimited(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity, outcome *writeOutcome, input any, eventKey *string, limit int64) bool {
	if identity.ServiceName != "" {
		outcome.result = "service_reject"
		writeJSONError(w, http.StatusForbidden, "operator_only")
		return false
	}
	if !sameOrigin(r) {
		outcome.result = "origin_reject"
		writeJSONError(w, http.StatusForbidden, "origin_rejected")
		return false
	}
	if !h.validHeaderCSRF(r, identity.Email) {
		outcome.result = "csrf_reject"
		writeJSONError(w, http.StatusForbidden, "csrf_rejected")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(input); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	if *eventKey == "" {
		writeJSONError(w, http.StatusBadRequest, "event_key_required")
		return false
	}
	outcome.eventID = auditTarget(*eventKey)
	return true
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
	case errors.Is(err, store.ErrDecisionAttemptNotFound):
		writeJSONError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, store.ErrDecisionAttemptNotPending):
		writeJSONError(w, http.StatusConflict, "decision_attempt_not_pending")
	case errors.Is(err, store.ErrDecisionAttemptDenied):
		writeJSONError(w, http.StatusConflict, "decision_attempt_denied")
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
