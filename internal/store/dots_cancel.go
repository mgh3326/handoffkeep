package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

// A requester cancels its own dots request (operator decision 359, hk 4987).
// A comment does not settle the request: tasks leave a started state only
// through a transition, so the cancel is one move of the request's own state
// machine to dropped (the canceled category) with the reason recorded as the
// transition note and the closure evidence. The request then leaves
// dots_inbox (settled states are not listed) and every later dots reply is
// refused there by the settled-state check, exactly as on any settled task.
//
// The rule is the requester's own request: the request carries its requester
// lane in parent_lane, and a cancel naming another lane is refused with the
// rule, in store-authored text that never echoes the caller's lane. The
// operator keeps the generic task transition for anything outside this rule.

// DotsCancelInput is one requester cancel of a dots request.
type DotsCancelInput struct {
	// RequesterLane is the lane that filed the request; it must match the
	// request's parent_lane.
	RequesterLane string `json:"requester_lane"`
	Reason        string `json:"reason"`
}

// CancelDotsRequest settles one dots request as canceled by its requester
// lane, recording reason on the task.
func (s *Store) CancelDotsRequest(ctx context.Context, taskID int64, in DotsCancelInput) (Task, error) {
	lane := strings.TrimSpace(in.RequesterLane)
	reason := strings.TrimSpace(in.Reason)
	if taskID < 1 {
		return Task{}, ErrTaskNotFound
	}
	if !validName(lane) {
		return Task{}, dotsRequestInvalid("requester lane must match [A-Za-z0-9._-]{1,128}")
	}
	if reason == "" {
		return Task{}, dotsRequestInvalid("a cancel requires a reason")
	}
	// The reason becomes the dropped closure's evidence, so the closure bound
	// is the one that decides it; refusing it here names the bound instead of
	// failing later in applyLifecycle.
	if len(reason) > closureEvidenceMaxBytes || !validText(reason, closureEvidenceMaxBytes) {
		return Task{}, dotsRequestInvalid(fmt.Sprintf("reason must be 1..%d bytes", closureEvidenceMaxBytes))
	}
	if err := guard.Reject(reason); err != nil {
		return Task{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback(ctx)
	// The lane rule is checked under the same locks the transition takes, so
	// a relane or a concurrent transition cannot land between the check and
	// the write.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, err
	}
	var x Task
	if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, taskID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, err
	}
	if x.Lane != "dots" || !IsHKDotsRequest(x) {
		return Task{}, dotsRequestInvalid("this task is not an hk dots request")
	}
	if x.ParentLane != lane {
		return Task{}, fmt.Errorf("%w: only the request's requester lane may cancel it", ErrTaskConflict)
	}
	x, err = s.transitionTaskTx(ctx, tx, taskID, TransitionInput{To: StateDropped, By: lane, Note: reason}, false)
	if err != nil {
		return Task{}, err
	}
	return x, tx.Commit(ctx)
}
