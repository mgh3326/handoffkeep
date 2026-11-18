package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mgh3326/handoffkeep/internal/guard"
)

// A task's close rule (hk 5112, track B-2) says who may close it.
//
// CloseRuleAuto is the default: a merge of its last open change, or its last
// unsettled child, closes it. CloseRuleExplicit is for a programme task whose
// pieces are not all filed up front: a merge or a parent's rollup records the
// event and the PR, but does not move the task to merged, deployed or
// completed. Only a transition that carries evidence closes it, and drift
// reports it (explicit_all_settled) once its pieces are all settled.
const (
	CloseRuleAuto     = "auto"
	CloseRuleExplicit = "explicit"
)

// ErrInvalidCloseRule refuses a close rule other than auto or explicit.
var ErrInvalidCloseRule = errors.New("invalid_close_rule")

// explicitCloseReason is the hold recorded when an event or a rollup would
// close an explicit task. The same text names the hold in the drift report.
const explicitCloseReason = "the task closes by hand (close_rule explicit); the state does not move"

// explicitClosedStates are the states a close moves a task to. A forward move
// to any other state still applies to an explicit task.
var explicitClosedStates = states(StateMerged, StateDeployed, StateCompleted)

// validCloseRule reports whether rule is one of the two close rules.
func validCloseRule(rule string) bool {
	return rule == CloseRuleAuto || rule == CloseRuleExplicit
}

// explicitHoldsClose reports whether an explicit task keeps its state when a
// move to state would close it.
func explicitHoldsClose(x Task, state string) bool {
	return x.CloseRule == CloseRuleExplicit && explicitClosedStates[state]
}

// SetTaskCloseRule sets a task's close rule and records the change as a
// metadata event. It returns the task and whether the rule changed; setting the
// rule it already has writes nothing. Any task state may change its rule, since
// the rule only governs later closes.
func (s *Store) SetTaskCloseRule(ctx context.Context, id int64, rule, by, reason string) (Task, bool, error) {
	if id < 1 || !validCloseRule(rule) || by == "" || !validText(by, 128) || !validText(reason, MaxBytes) {
		return Task{}, false, ErrInvalidCloseRule
	}
	if err := guard.Reject(reason); err != nil {
		return Task{}, false, err
	}
	var out Task
	var changed bool
	err := s.retryTx(ctx, func() (err error) {
		out, changed, err = s.setTaskCloseRuleOnce(ctx, id, rule, by, reason)
		return err
	})
	if err != nil {
		return Task{}, false, err
	}
	return out, changed, nil
}

// setTaskCloseRuleOnce is one attempt of SetTaskCloseRule, in its own transaction.
func (s *Store) setTaskCloseRuleOnce(ctx context.Context, id int64, rule, by, reason string) (Task, bool, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer release()
	defer tx.Rollback(ctx)
	if err = s.lockTx(ctx, tx, flowTaskMutationLockID); err != nil {
		return Task{}, false, err
	}
	var x Task
	if err = scanTask(tx.QueryRow(ctx, s.q(`SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, `SELECT `+taskColumns+` FROM tasks WHERE id=? FOR UPDATE`), id), &x); err != nil {
		if errors.Is(err, ErrNoRows) {
			return Task{}, false, ErrTaskNotFound
		}
		return Task{}, false, err
	}
	if x.CloseRule == rule {
		return x, false, tx.Commit(ctx)
	}
	if err = setTaskCloseRuleTx(ctx, tx, &x, rule, by, reason, "", ""); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

// setTaskCloseRuleTx writes a close-rule change and its event inside the
// caller's transaction. The caller has locked the task row and loaded it into
// x; on return x carries the new rule and updated_at.
func setTaskCloseRuleTx(ctx context.Context, tx transaction, x *Task, rule, by, reason, source, key string) error {
	if !validCloseRule(rule) {
		return ErrInvalidCloseRule
	}
	now := time.Now().UTC()
	update := sqlFor(tx, `UPDATE tasks SET close_rule=$2,updated_at=$3 WHERE id=$1`, `UPDATE tasks SET close_rule=?,updated_at=? WHERE id=?`)
	args := []any{x.ID, rule, now}
	if dialectFor(tx) == mysqlDialect {
		args = []any{rule, now, x.ID}
	}
	if _, err := tx.Exec(ctx, update, args...); err != nil {
		return err
	}
	note := fmt.Sprintf("close rule %s → %s", x.CloseRule, rule)
	if reason != "" {
		note += ": " + reason
	}
	if _, _, err := insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: "metadata", From: x.State, To: x.State, By: by, Note: note, Refs: x.Refs, At: now, Source: source, SourceRef: key, Field: "close_rule", FromValue: x.CloseRule, ToValue: rule}); err != nil {
		return err
	}
	x.CloseRule, x.UpdatedAt = rule, now
	return nil
}
