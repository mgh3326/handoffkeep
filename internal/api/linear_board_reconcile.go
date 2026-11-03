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
func (a LinearAPI) ReconcileLinearBoardIssue(ctx context.Context, st LinearBoardStore, taskID int64) (string, error) {
	task, found, err := st.GetTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", store.ErrTaskNotFound
	}
	stopped, err := st.LinearBoardDeliveryStopped(ctx, task.ID)
	if err != nil {
		return "", err
	}
	if stopped {
		return "", errors.New("inactive board task requires a separate unarchive decision")
	}
	target, err := a.BoardTarget(ctx)
	if err != nil {
		return "", err
	}
	id := store.LinearDecisionUUID(fmt.Sprintf("work-board:task:%d", task.ID))
	if a.Admission == nil {
		return "", ErrLinearCapacity
	}
	var issue BoardIssue
	foundIssue := false
	err = a.Admission.SerializeLinearCreate(ctx, func(lease store.LinearCreateLease) error {
		var err error
		issue, foundIssue, err = a.findBoardIssueForRepair(ctx, id)
		if err != nil {
			return err
		}
		if !foundIssue {
			if !a.CapacityScopeVerified {
				return ErrLinearCapacity
			}
			return lease.ReleaseAbsent(ctx, id)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !foundIssue {
		return "released", nil
	}
	if issue.ID != id || issue.Team.ID != target.TeamID || issue.ArchivedAt != "" {
		return "", errors.New("manual board repair requires the active deterministic issue")
	}
	if err = st.EnsureLinearBoardOwnership(ctx, task.ID, id, issue.Identifier); err != nil {
		return "", err
	}
	if a.Admission == nil {
		return "", ErrLinearCapacity
	}
	if err = a.Admission.SerializeLinearCreate(ctx, func(lease store.LinearCreateLease) error {
		if _, err := lease.ReserveBoard(ctx, id, task.ID); err != nil {
			return err
		}
		return lease.AwaitBoardWebhook(ctx, id)
	}); err != nil {
		return "", err
	}
	description := boardRecoveryStamp.ReplaceAllString(issue.Description, "")
	marker := fmt.Sprintf("<!-- work-board:task:%d -->", task.ID)
	if !strings.Contains(description, marker) {
		description += "\n" + marker
	}
	description += "\n<!-- hk-linear-reconcile:" + time.Now().UTC().Format(time.RFC3339Nano) + " -->"
	var out struct{ IssueUpdate struct{ Success bool } }
	if err = a.graphql(ctx, linearWriteTimeout, linearIssueUpdateMutation, map[string]any{"id": id, "input": map[string]any{"description": description}}, &out); err != nil {
		return "", err
	}
	if !out.IssueUpdate.Success {
		return "", errors.New("linear board repair marker refused")
	}
	return "awaiting_webhook", nil
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
	status, err := s.LinearBoard.ReconcileLinearBoardIssue(r.Context(), s.Service.Store, body.TaskID)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]string{"status": status})
}

// Used only by explicit repair. Including archived issues prevents normal list
// omission from being interpreted as absence.
const boardRepairQuery = `query HkBoardRepair($id:ID!){issues(first:2,includeArchived:true,filter:{id:{eq:$id}}){nodes{id identifier archivedAt description team{id}}pageInfo{hasNextPage}}}`

func (a LinearAPI) findBoardIssueForRepair(ctx context.Context, id string) (BoardIssue, bool, error) {
	var out struct {
		Issues *struct {
			Nodes    []BoardIssue
			PageInfo *struct{ HasNextPage *bool }
		}
	}
	if err := a.graphql(ctx, linearWriteTimeout, boardRepairQuery, map[string]any{"id": id}, &out); err != nil {
		return BoardIssue{}, false, err
	}
	if out.Issues == nil || out.Issues.Nodes == nil || out.Issues.PageInfo == nil || out.Issues.PageInfo.HasNextPage == nil || *out.Issues.PageInfo.HasNextPage || len(out.Issues.Nodes) > 1 {
		return BoardIssue{}, false, errors.New("incomplete exact board repair read")
	}
	if len(out.Issues.Nodes) == 0 {
		return BoardIssue{}, false, nil
	}
	issue := out.Issues.Nodes[0]
	if issue.ID != id {
		return BoardIssue{}, false, errors.New("board repair identity mismatch")
	}
	return issue, true, nil
}

func (a LinearAPI) ReconcileLinearReservation(ctx context.Context, id string) (string, error) {
	if a.Admission == nil || !a.CapacityScopeVerified {
		return "", ErrLinearCapacity
	}
	status := "released"
	err := a.Admission.SerializeLinearCreate(ctx, func(lease store.LinearCreateLease) error {
		_, found, err := a.admissionExisting(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			return lease.ReleaseAbsent(ctx, id)
		}
		board, err := lease.IsBoardReservation(ctx, id)
		if err != nil {
			return err
		}
		if board {
			return errors.New("existing board issue requires linear board reconcile --task")
		}
		status = "completed"
		return lease.Complete(ctx, id)
	})
	return status, err
}
func (s Server) linearReservationReconcile(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.auth(w, r); !ok {
		return
	}
	if s.LinearBoard == nil {
		writeError(w, http.StatusServiceUnavailable, "linear_board_disabled")
		return
	}
	var body struct {
		IssueID string `json:"issue_id"`
	}
	if err := decode(r, &body, 1024); err != nil || !linearReservationID.MatchString(body.IssueID) {
		writeError(w, http.StatusBadRequest, "invalid_context")
		return
	}
	status, err := s.LinearBoard.ReconcileLinearReservation(r.Context(), body.IssueID)
	if err != nil {
		appErr(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]string{"status": status})
}

var linearReservationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
