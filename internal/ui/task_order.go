package ui

import (
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// taskOrderPathRE is the console's "set going" action on one task (hk 5125,
// docs/task-orders.md). It is an order path, not a decision feature: it records
// the operator's words and queues one notice to the owning design lane.
var taskOrderPathRE = regexp.MustCompile(`^/ui/api/tasks/([1-9][0-9]{0,14})/order$`)

// taskOrder records an order from a verified Access operator. The channel is
// the console and the order is verified: the identity comes from Cloudflare
// Access, never from the body, and service identities are refused by
// decodeOperatorWrite.
func (h *Handler) taskOrder(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	outcome := writeOutcome{action: "task-order", target: "-", eventID: "-", result: "invalid"}
	defer func() {
		h.audit(apiIdentity(identity), outcome.action, outcome.target, outcome.eventID, outcome.result)
	}()
	var input struct {
		Words    string `json:"words"`
		EventKey string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, &outcome, &input, &input.EventKey) {
		return
	}
	id, ok := pathTaskID(taskOrderPathRE, r.URL.Path)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	outcome.target = strconv.FormatInt(id, 10)
	result, err := h.store.CreateTaskOrder(r.Context(), store.CreateTaskOrderInput{
		TaskID: id, Words: input.Words, Actor: apiIdentity(identity), Channel: store.TaskOrderChannelConsole, Verified: true, Key: input.EventKey,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrTaskNotFound):
			writeJSONError(w, http.StatusNotFound, "not_found")
		case errors.Is(err, store.ErrOrderTaskStarted):
			writeJSONError(w, http.StatusConflict, "order_task_started")
		case errors.Is(err, store.ErrOrderNoLane):
			writeJSONError(w, http.StatusConflict, "order_no_lane")
		case errors.Is(err, store.ErrOrderKeyConflict):
			writeJSONError(w, http.StatusConflict, "order_key_conflict")
		default:
			if _, ok := guard.SecretPattern(err); ok {
				writeJSONError(w, http.StatusBadRequest, "secret_like_content")
				return
			}
			if errors.Is(err, store.ErrOrderInvalid) {
				writeJSONError(w, http.StatusBadRequest, "invalid_request")
				return
			}
			log.Printf("ui.task_order_failed err=%q", err.Error())
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			outcome.result = "error"
		}
		return
	}
	outcome.result = "ok"
	if result.Duplicate {
		outcome.result = "replay"
	}
	writeBoardJSON(w, result)
}
