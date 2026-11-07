package ui

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"regexp"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
)

var taskCommandPathRE = regexp.MustCompile(`^/ui/api/tasks/([1-9][0-9]{0,14})/commands$`)

// taskCommand records one operator command from the console. It is a command
// to an outside executor, so the response is the recorded ladder row, never
// an execution result.
func (h *Handler) taskCommand(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	outcome := writeOutcome{action: "task-command", target: "-", eventID: "-", result: "invalid"}
	defer func() {
		h.audit(apiIdentity(identity), outcome.action, outcome.target, outcome.eventID, outcome.result)
	}()
	var input struct {
		Kind     string `json:"kind"`
		Text     string `json:"text"`
		Intent   string `json:"intent"`
		EventKey string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, &outcome, &input, &input.EventKey) {
		return
	}
	id, ok := pathTaskID(taskCommandPathRE, r.URL.Path)
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	outcome.target = strconv.FormatInt(id, 10)
	payload, err := json.Marshal(store.WorkerMessagePayload{Text: input.Text, Intent: input.Intent})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	command, err := h.store.RecordOperatorCommand(r.Context(), store.OperatorCommandInput{
		CommandKey: input.EventKey,
		Kind:       input.Kind,
		TaskID:     &id,
		Payload:    payload,
		Actor:      "operator:" + identity.Email,
		Channel:    "console",
	})
	if err != nil {
		outcome.result = "invalid"
		switch {
		case errors.Is(err, store.ErrOperatorCommandNoExecutor):
			writeJSONError(w, http.StatusConflict, "operator_command_no_executor")
		case errors.Is(err, store.ErrOperatorCommandNotExecutable):
			writeJSONError(w, http.StatusBadRequest, "operator_command_not_executable")
		case errors.Is(err, store.ErrOperatorCommandInvalid):
			writeJSONError(w, http.StatusBadRequest, "invalid_request")
		case errors.Is(err, store.ErrTaskNotFound):
			writeJSONError(w, http.StatusNotFound, "not_found")
		default:
			if _, ok := guard.SecretPattern(err); ok {
				writeJSONError(w, http.StatusBadRequest, "secret_like_content")
				return
			}
			log.Printf("ui.task_command_failed err=%q", err.Error())
			writeJSONError(w, http.StatusInternalServerError, "internal_error")
			outcome.result = "error"
		}
		return
	}
	outcome.result = "ok"
	writeBoardJSONStatus(w, http.StatusCreated, projectBoardCommand(command))
}
