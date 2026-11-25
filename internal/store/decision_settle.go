package store

import (
	"context"
	"time"
)

// DecisionWithdrawnTaskSettled is the withdrawn_reason of a request that its
// task settled before anyone answered it (hk 6339).
const DecisionWithdrawnTaskSettled = "task_settled"

// withdrawSettledRequestTx closes the open decision request of a task that
// settles in this write (hk 6339). refs.decision_request becomes withdrawn
// with withdrawn_reason, and its inbox row is superseded as a withdraw is, in
// the same transaction as the task row. A settled task keeps no open request,
// so drift and the pending list agree with the refs. A task that is not
// settled, or a request that is not open, is left as it is.
func withdrawSettledRequestTx(ctx context.Context, tx transaction, x *Task, at time.Time) error {
	r := x.Refs.DecisionRequest
	if r == nil || r.Status != DecisionRequestOpen || !IsSettledState(x.State) {
		return nil
	}
	closed := *r
	closed.Status = DecisionRequestWithdrawn
	closed.WithdrawnReason = DecisionWithdrawnTaskSettled
	x.Refs.DecisionRequest = &closed
	if dialectFor(tx) == mysqlDialect {
		return mysqlSupersedeDecisionTx(ctx, tx, requestSources, closed.ID, DecisionRequestWithdrawn, at)
	}
	return supersedeDecisionTx(ctx, pgxTx(tx), requestSources, closed.ID, DecisionRequestWithdrawn, at)
}
