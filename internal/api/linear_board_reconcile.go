package api

import (
	"context"
	"errors"
	"fmt"
	"github.com/mgh3326/handoffkeep/internal/store"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var boardRecoveryStamp = regexp.MustCompile(`<!-- hk-linear-reconcile:[^>]* -->`)

// This explicit repair reads one deterministic issue id and changes only hk's
// marker. The resulting signed Issue webhook, not this command, writes the link.
func (a LinearAPI) ReconcileLinearBoardIssue(ctx context.Context, st LinearBoardStore, taskID int64) error {
	task, found, err := st.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if !found {
		return store.ErrTaskNotFound
	}
	stopped, err := st.LinearBoardDeliveryStopped(ctx, task.ID)
	if err != nil {
		return err
	}
	if stopped {
		return errors.New("inactive board task requires a separate unarchive decision")
	}
	target, err := a.BoardTarget(ctx)
	if err != nil {
		return err
	}
	id := store.LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", task.ID))
	issue, err := a.BoardIssue(ctx, id)
	if err != nil {
		return err
	}
	if issue.ID != id || issue.Team.ID != target.TeamID || issue.ArchivedAt != "" {
		return errors.New("manual board repair requires the active deterministic issue")
	}
	if err = st.EnsureLinearBoardOwnership(ctx, task.ID, id, issue.Identifier); err != nil {
		return err
	}
	if a.Admission == nil {
		return ErrLinearCapacity
	}
	if err = a.Admission.SerializeLinearCreate(ctx, func(lease store.LinearCreateLease) error {
		if _, err := lease.ReserveBoard(ctx, id, task.ID); err != nil {
			return err
		}
		return lease.AwaitBoardWebhook(ctx, id)
	}); err != nil {
		return err
	}
	description := boardRecoveryStamp.ReplaceAllString(issue.Description, "")
	marker := fmt.Sprintf("<!-- work-board:task:%d -->", task.ID)
	if !strings.Contains(description, marker) {
		description += "\n" + marker
	}
	description += "\n<!-- hk-linear-reconcile:" + time.Now().UTC().Format(time.RFC3339Nano) + " -->"
	var out struct{ IssueUpdate struct{ Success bool } }
	if err = a.graphql(ctx, linearWriteTimeout, linearIssueUpdateMutation, map[string]any{"id": id, "input": map[string]any{"description": description}}, &out); err != nil {
		return err
	}
	if !out.IssueUpdate.Success {
		return errors.New("linear board repair marker refused")
	}
	return nil
}
func (s Server) linearBoardReconcile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	if s.LinearBoard == nil {
		writeError(w, http.StatusServiceUnavailable, "linear_board_disabled")
		return
	}
	var body struct {
		TaskID int64 `json:"task_id"`
	}
	if err := decode(r, &body, 1024); err != nil || body.TaskID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	if err := s.LinearBoard.ReconcileLinearBoardIssue(r.Context(), s.Service.Store, body.TaskID); err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]string{"status": "awaiting_webhook"})
}
