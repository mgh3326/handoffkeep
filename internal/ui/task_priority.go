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

var taskPriorityPathRE = regexp.MustCompile(`^/ui/api/tasks/([1-9][0-9]{0,14})/priority$`)

func (h *Handler) taskPriority(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	outcome := writeOutcome{action: "task-priority", target: "-", eventID: "-", result: "invalid"}
	defer func() {
		h.audit(apiIdentity(identity), outcome.action, outcome.target, outcome.eventID, outcome.result)
	}()
	var input struct {
		Priority *int   `json:"priority"`
		Reason   string `json:"reason"`
		EventKey string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, &outcome, &input, &input.EventKey) {
		return
	}
	if input.Priority == nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	id, ok := pathTaskID(taskPriorityPathRE, r.URL.Path)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	outcome.target = strconv.FormatInt(id, 10)
	// PG is the execution value. The YouTrack export is queued after the
	// write, so a YouTrack outage never blocks the priority change.
	result, err := h.store.SetTaskPriority(r.Context(), id, *input.Priority, apiIdentity(identity), input.Reason, input.EventKey)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidTaskPriority):
			writeJSONError(w, http.StatusBadRequest, "invalid_request")
		case errors.Is(err, store.ErrTaskNotFound):
			writeJSONError(w, http.StatusNotFound, "not_found")
		case errors.Is(err, store.ErrTaskTerminal):
			writeJSONError(w, http.StatusConflict, "task_terminal")
		default:
			if _, ok := guard.SecretPattern(err); ok {
				writeJSONError(w, http.StatusBadRequest, "secret_like_content")
				return
			}
			log.Printf("ui.task_priority_failed err=%q", err.Error())
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			outcome.result = "error"
		}
		return
	}
	// A replayed event key wrote nothing. The recorded value may be older than
	// the row, so exporting the request's priority would overwrite a newer
	// write in YouTrack.
	if h.youTrack != nil && !result.Replayed {
		h.youTrack.RecordPriority(r.Context(), id, *input.Priority)
	}
	outcome.result = "ok"
	writeBoardJSON(w, result)
}
