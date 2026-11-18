package store

import (
	"context"
	"time"
)

// closeQuestionTaskTx closes a task filed only to carry a decision request
// (refs.question_task, set by `decisions ask --new-task`) when that request is
// answered, withdrawn or default-applied (operator decision 417, hk 5201).
//
// It closes to completed in the resolving transaction, with the request id as
// the closure evidence, so the record cannot show an answered question on an
// open task. A task with children or with a linked change is carrying work as
// well as the question and stays open, as does any other decide task. The
// caller holds the task row lock and the task-mutation lock.
func (s *Store) closeQuestionTaskTx(ctx context.Context, tx transaction, x *Task, requestID, by string, now time.Time) error {
	if x.Kind != "decide" || !x.Refs.QuestionTask || IsSettledState(x.State) {
		return nil
	}
	var carriesWork bool
	if err := tx.QueryRow(ctx, s.q(
		`SELECT EXISTS(SELECT 1 FROM tasks WHERE parent_id=$1) OR EXISTS(SELECT 1 FROM change_tasks WHERE task_id=$1)`,
		`SELECT EXISTS(SELECT 1 FROM tasks WHERE parent_id=?) OR EXISTS(SELECT 1 FROM change_tasks WHERE task_id=?)`),
		x.ID, x.ID).Scan(&carriesWork); err != nil {
		return err
	}
	if carriesWork {
		return nil
	}
	from := x.State
	x.State = StateCompleted
	x.AwaitingDecision = false
	x.UpdatedAt = now
	if err := applyLifecycle(x, from, StateCompleted, now, &TaskClosure{Type: "resolved", Evidence: requestID}); err != nil {
		return err
	}
	written, err := writeTaskTx(ctx, tx, *x)
	if err != nil {
		return err
	}
	*x = written
	if _, _, err = insertEvent(ctx, tx, taskEventInput{
		TaskID: x.ID, From: from, To: StateCompleted, By: by,
		Note: "completed by decision " + requestID, Refs: x.Refs, Into: x, At: now,
	}); err != nil {
		return err
	}
	return s.afterStateChangeTx(ctx, tx, *x, from, by, now)
}
