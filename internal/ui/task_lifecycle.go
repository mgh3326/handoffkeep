package ui

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/guard"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// The console's task lifecycle writes: one route per registry action, all
// through decodeOperatorWrite (operator identity, same origin, header CSRF,
// size-capped body, required idempotency key).
var taskLifecyclePathRE = regexp.MustCompile(`^/ui/api/tasks/([1-9][0-9]{0,14})/(close-merged|close-completed|drop|hold|reopen|relane)$`)

// lifecycleRouteName is the static telemetry name of a lifecycle path, or ""
// when path is not one.
func lifecycleRouteName(path string) string {
	m := taskLifecyclePathRE.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return "/ui/api/tasks/{id}/" + m[2]
}

// lifecycleWriteAction is writeAction's name for a lifecycle path.
func lifecycleWriteAction(path string) string {
	m := taskLifecyclePathRE.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	return "task-" + m[2]
}

func (h *Handler) taskLifecycle(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity) {
	m := taskLifecyclePathRE.FindStringSubmatch(r.URL.Path)
	if m == nil {
		writeJSONError(w, http.StatusNotFound, "not_found")
		return
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || id < 1 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	outcome := writeOutcome{action: "task-" + m[2], target: m[1], eventID: "-", result: "invalid"}
	defer func() {
		h.audit(apiIdentity(identity), outcome.action, outcome.target, outcome.eventID, outcome.result)
	}()
	switch m[2] {
	case "close-merged":
		h.closeMerged(w, r, identity, id, &outcome)
	case "close-completed":
		h.closeCompleted(w, r, identity, id, &outcome)
	case "drop":
		h.dropTask(w, r, identity, id, &outcome)
	case "hold":
		h.holdTask(w, r, identity, id, &outcome)
	case "reopen":
		h.reopenTask(w, r, identity, id, &outcome)
	case "relane":
		h.relaneTask(w, r, identity, id, &outcome)
	default:
		writeJSONError(w, http.StatusNotFound, "not_found")
	}
}

// closeMerged records reviewing -> merged, or researching/developing ->
// reviewing -> merged, in the store's one call.
func (h *Handler) closeMerged(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity, id int64, outcome *writeOutcome) {
	var input struct {
		Evidence string `json:"evidence"`
		Reason   string `json:"reason"`
		EventKey string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, outcome, &input, &input.EventKey) {
		return
	}
	task, path, err := h.store.CloseTaskMerged(r.Context(), id, input.Evidence, apiIdentity(identity), input.Reason, input.EventKey)
	if err != nil {
		outcome.result = lifecycleError(w, err)
		return
	}
	outcome.result = "ok"
	writeBoardJSON(w, map[string]any{"state": task.State, "path": path})
}

func (h *Handler) closeCompleted(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity, id int64, outcome *writeOutcome) {
	var input struct {
		Evidence string `json:"evidence"`
		Reason   string `json:"reason"`
		EventKey string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, outcome, &input, &input.EventKey) {
		return
	}
	task, err := h.store.CloseTaskCompleted(r.Context(), id, input.Evidence, apiIdentity(identity), input.Reason, input.EventKey)
	if err != nil {
		outcome.result = lifecycleError(w, err)
		return
	}
	outcome.result = "ok"
	writeBoardJSON(w, map[string]any{"state": task.State})
}

// dropTask closes a task with the picker's closure type. The "대체됨" choice
// is the duplicate state and needs the surviving task id.
func (h *Handler) dropTask(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity, id int64, outcome *writeOutcome) {
	var input struct {
		Closure     string `json:"closure"`
		Reason      string `json:"reason"`
		DuplicateOf int64  `json:"duplicate_of"`
		EventKey    string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, outcome, &input, &input.EventKey) {
		return
	}
	if strings.TrimSpace(input.Reason) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	switch input.Closure {
	case "cancelled", "obsolete":
		task, err := h.store.DropTask(r.Context(), id, input.Closure, input.Reason, apiIdentity(identity), input.EventKey)
		if err != nil {
			outcome.result = lifecycleError(w, err)
			return
		}
		outcome.result = "ok"
		writeBoardJSON(w, map[string]any{"state": task.State, "closure_type": input.Closure})
	case "superseded":
		if input.DuplicateOf < 1 || input.DuplicateOf == id {
			writeJSONError(w, http.StatusBadRequest, "duplicate_of_required")
			return
		}
		task, err := h.store.DuplicateTask(r.Context(), id, input.DuplicateOf, input.Reason, apiIdentity(identity), input.EventKey)
		if err != nil {
			outcome.result = lifecycleError(w, err)
			return
		}
		outcome.result = "ok"
		writeBoardJSON(w, map[string]any{"state": task.State, "closure_type": input.Closure, "duplicate_of": input.DuplicateOf})
	default:
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
	}
}

func (h *Handler) holdTask(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity, id int64, outcome *writeOutcome) {
	var input struct {
		Held     *bool  `json:"held"`
		To       string `json:"to"`
		EventKey string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, outcome, &input, &input.EventKey) {
		return
	}
	if input.Held == nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	note := "보류"
	if !*input.Held {
		note = "보류 해제"
	}
	result, err := h.store.HoldTask(r.Context(), id, *input.Held, input.To, apiIdentity(identity), note, input.EventKey)
	if err != nil {
		outcome.result = lifecycleError(w, err)
		return
	}
	outcome.result = "ok"
	writeBoardJSON(w, result)
}

func (h *Handler) reopenTask(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity, id int64, outcome *writeOutcome) {
	var input struct {
		Reason   string `json:"reason"`
		EventKey string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, outcome, &input, &input.EventKey) {
		return
	}
	if strings.TrimSpace(input.Reason) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	task, err := h.store.ReopenTaskBy(r.Context(), id, apiIdentity(identity), input.Reason, input.EventKey)
	if err != nil {
		outcome.result = lifecycleError(w, err)
		return
	}
	outcome.result = "ok"
	writeBoardJSON(w, map[string]any{"state": task.State})
}

// relaneTask moves a task and appends one lane.event notice to the lane it
// left and one to the lane it joined. The event ids derive from the
// idempotency key, so a replayed request is deduplicated by the relay.
func (h *Handler) relaneTask(w http.ResponseWriter, r *http.Request, identity cfaccess.Identity, id int64, outcome *writeOutcome) {
	var input struct {
		Lane     string `json:"lane"`
		Reason   string `json:"reason"`
		EventKey string `json:"event_key"`
	}
	if !h.decodeOperatorWrite(w, r, identity, outcome, &input, &input.EventKey) {
		return
	}
	note := strings.TrimSpace(input.Reason)
	if note == "" {
		note = "콘솔에서 레인 이동"
	}
	result, err := h.store.OperatorRelaneTask(r.Context(), id, input.Lane, apiIdentity(identity), note, input.EventKey)
	if err != nil {
		outcome.result = lifecycleError(w, err)
		return
	}
	relay := "none"
	if result.Changed {
		relay = h.emitRelaneNotices(r.Context(), store.Task{ID: id, Title: result.Title, Lane: result.Lane}, result.Previous, input.EventKey)
	}
	outcome.result = "ok"
	if relay == "failed" {
		outcome.result = "hub_error"
	}
	writeBoardJSON(w, map[string]any{"previous": result.Previous, "lane": result.Lane, "relay": relay, "changed": result.Changed})
}

// emitRelaneNotices sends the two Korean lane notices and reports "sent",
// "duplicate", "unconfigured" or "failed". A relay failure is reported, never
// rolled back: the lane move itself already committed.
func (h *Handler) emitRelaneNotices(ctx context.Context, task store.Task, from, key string) string {
	if h.hub == nil || !h.hub.configured() {
		return "unconfigured"
	}
	title := []rune(strings.TrimSpace(task.Title))
	if len(title) > 60 {
		title = append(title[:60], '…')
	}
	notices := []struct{ lane, text, suffix string }{
		{from, fmt.Sprintf("[relane] #%d %s 작업을 %s에서 %s 레인으로 옮겼습니다 (운영자 콘솔)", task.ID, string(title), from, task.Lane), "-old"},
		{task.Lane, fmt.Sprintf("[relane] #%d %s 작업이 %s에서 %s 레인으로 들어왔습니다 (운영자 콘솔)", task.ID, string(title), from, task.Lane), "-new"},
	}
	status := "sent"
	for _, notice := range notices {
		duplicate, err := h.hub.emitLaneEvent(ctx, notice.lane, "web-relane-"+key+notice.suffix, notice.text)
		switch {
		case err != nil:
			status = "failed"
		case duplicate && status == "sent":
			status = "duplicate"
		}
	}
	return status
}

// lifecycleError answers a refused lifecycle write and returns the audit
// result. Store-authored sentinels keep their own wire code; a plain
// validation refusal is invalid_request; anything else is logged and 500.
func lifecycleError(w http.ResponseWriter, err error) string {
	switch {
	case uiWrkError(w, err):
		return "wrk_reject"
	case errors.Is(err, store.ErrTaskNotFound):
		writeJSONError(w, http.StatusNotFound, "not_found")
	case errors.Is(err, store.ErrTaskMergePath):
		writeJSONError(w, http.StatusConflict, "task_merge_path")
	case errors.Is(err, store.ErrTaskConflict):
		writeJSONError(w, http.StatusConflict, "task_conflict")
	case errors.Is(err, store.ErrTaskTerminal):
		writeJSONError(w, http.StatusConflict, "task_terminal")
	case errors.Is(err, store.ErrClosureInvalid):
		writeJSONError(w, http.StatusBadRequest, "closure_invalid")
	case errors.Is(err, store.ErrDuplicateOfRequired):
		writeJSONError(w, http.StatusBadRequest, "duplicate_of_required")
	case errors.Is(err, store.ErrTaskLaneUnknown):
		writeJSONError(w, http.StatusBadRequest, "unknown_lane")
	case errors.Is(err, store.ErrDispositionOperatorOnly):
		writeJSONError(w, http.StatusConflict, "disposition_operator_only")
	case errors.Is(err, store.ErrDecisionRequestOpen):
		writeJSONError(w, http.StatusConflict, "decision_request_open")
	case errors.Is(err, store.ErrTaskJobRequired):
		writeJSONError(w, http.StatusConflict, "task_job_required")
	case strings.HasPrefix(err.Error(), "invalid task "):
		writeJSONError(w, http.StatusBadRequest, "invalid_request")
	default:
		if _, ok := guard.SecretPattern(err); ok {
			writeJSONError(w, http.StatusBadRequest, "secret_like_content")
			return "invalid"
		}
		log.Printf("ui.task_lifecycle_failed err=%q", err.Error())
		writeJSONError(w, http.StatusInternalServerError, "internal_error")
		return "error"
	}
	return "invalid"
}
