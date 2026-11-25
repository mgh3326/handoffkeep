package store

import (
	"context"
	"fmt"
	"time"
)

// DecisionWithdrawnTaskSettled is the withdrawn_reason of a request that its
// task settled before anyone answered it (hk 6339).
const DecisionWithdrawnTaskSettled = "task_settled"

// withdrawSettledRequestTx closes the open decision request of a task that
// settles in this write (hk 6339). refs.decision_request becomes withdrawn
// with withdrawn_reason, and its inbox row is superseded as a withdraw is.
// A block still set on the task is cleared, its task_flag rows are
// superseded, and the dots operator questions of the task are closed, so no
// row of a settled task reads as pending. All of it runs in the task write's
// transaction. A task that is not settled, or a request that is not open, is
// left as it is.
func withdrawSettledRequestTx(ctx context.Context, tx transaction, x *Task, at time.Time) error {
	r := x.Refs.DecisionRequest
	if r == nil || r.Status != DecisionRequestOpen || !IsSettledState(x.State) {
		return nil
	}
	closed := *r
	closed.Status = DecisionRequestWithdrawn
	closed.WithdrawnReason = DecisionWithdrawnTaskSettled
	x.Refs.DecisionRequest = &closed
	by := fmt.Sprintf("task:%d:%s", x.ID, x.State)
	mysql := dialectFor(tx) == mysqlDialect
	if x.AwaitingDecision {
		x.AwaitingDecision = false
		// The task's own flag rows close as a settle closes them (see
		// supersedeTaskDecisionsTx). MySQL's placeholders are positional.
		if mysql {
			if _, err := tx.Exec(ctx, `UPDATE decision_requests SET status='superseded',superseded_by=?,updated_at=? WHERE task_id=? AND source='task_flag' AND status='open'`, by, at, x.ID); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE decision_requests SET status='superseded',superseded_by=$2,updated_at=$3 WHERE task_id=$1 AND source='task_flag' AND status='open'`, x.ID, by, at); err != nil {
			return err
		}
	}
	// Only a dots request's ask_operator questions carry these rows, and no
	// other close of the request leaves one open (dots answer, resolve).
	if mysql {
		_, err := tx.Exec(ctx, `UPDATE dots_results SET answered_at=?,answered_by=? WHERE task_id=? AND kind='QUESTION' AND ask_operator AND answered_at IS NULL`, at, DecisionWithdrawnTaskSettled, x.ID)
		if err != nil {
			return err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE dots_results SET answered_at=$2,answered_by=$3 WHERE task_id=$1 AND kind='QUESTION' AND ask_operator AND answered_at IS NULL`, x.ID, at, DecisionWithdrawnTaskSettled); err != nil {
		return err
	}
	if mysql {
		return mysqlSupersedeDecisionTx(ctx, tx, requestSources, closed.ID, DecisionRequestWithdrawn, at)
	}
	return supersedeDecisionTx(ctx, pgxTx(tx), requestSources, closed.ID, DecisionRequestWithdrawn, at)
}
