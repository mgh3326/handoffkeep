package store

import (
	"context"
	"errors"
	"time"
)

// The MySQL side of the task write path (hk 5074, B2): the helpers every task
// writer shares (insertEvent, writeTaskTx) and the seams to areas that are not
// ported yet. PostgreSQL never reaches this file except through the two
// seam methods at the bottom, which call the PostgreSQL helpers unchanged.

// flowTaskMutationLockID is the task-mutation lock (lockorder.go level 2).
const flowTaskMutationLockID = 8241873001

// flowTransaction is the transaction an insertEvent or writeTaskTx call on
// MySQL runs in. Both helpers take the narrow rowQuerier so their callers
// stay as they are, but MySQL needs InsertID and Exec, which only a
// transaction has; every caller on MySQL passes one.
func flowTransaction(q rowQuerier) (transaction, error) {
	tx, ok := q.(transaction)
	if !ok {
		return nil, errors.New("store: a task write on MySQL needs a transaction")
	}
	return tx, nil
}

// mysqlInsertEvent is insertEvent on MySQL. The PostgreSQL statement is
// INSERT ... ON CONFLICT (task_id,source,source_ref) DO NOTHING RETURNING id,
// on the partial index over rows with a source and a source_ref. MySQL's
// task_events_source_ref key is NULL for a row without both, so it conflicts only
// for the rows PostgreSQL's partial index covers. A do-nothing upsert would
// run the append-only BEFORE UPDATE guard, so the insert is plain: a unique
// violation, confirmed by re-reading the row, is the redelivered event, and
// returns the same (0, false, nil) as ON CONFLICT DO NOTHING.
func mysqlInsertEvent(ctx context.Context, q rowQuerier, e taskEventInput, refs string) (id int64, moved bool, err error) {
	tx, err := flowTransaction(q)
	if err != nil {
		return 0, false, err
	}
	id, err = tx.InsertID(ctx, "INSERT INTO task_events(task_id,`from`,`to`,`by`,note,refs,at,kind,no_job,source,source_ref,field,from_value,to_value,auto,applied) "+
		"VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		e.TaskID, e.From, e.To, e.By, e.Note, refs, e.At, e.Kind, e.NoJob, e.Source, e.SourceRef, e.Field, e.FromValue, e.ToValue, e.Auto, !e.NotApplied)
	if err != nil {
		if errors.Is(err, errUniqueViolation) && e.Source != "" && e.SourceRef != "" {
			var existing int64
			rerr := tx.QueryRow(ctx, `SELECT id FROM task_events WHERE task_id=? AND source=? AND source_ref=?`, e.TaskID, e.Source, e.SourceRef).Scan(&existing)
			if rerr == nil {
				return 0, false, nil
			}
			if !errors.Is(rerr, ErrNoRows) {
				return 0, false, rerr
			}
		}
		return 0, false, err
	}
	if !isActivityEvent(e.Auto, e.Source) {
		return id, false, nil
	}
	// UPDATE ... WHERE last_activity_at < at matches a row only when the clock
	// moves (ClientFoundRows counts matched rows).
	n, err := tx.Exec(ctx, `UPDATE tasks SET last_activity_at = GREATEST(last_activity_at, CAST(? AS DATETIME(6))) WHERE id = ? AND last_activity_at < ?`, e.At, e.TaskID, e.At)
	if err != nil {
		return id, false, err
	}
	if n == 0 {
		return id, false, nil
	}
	if e.Into != nil {
		e.Into.LastActivityAt = e.At.UTC().Truncate(time.Microsecond)
	}
	return id, true, nil
}

// mysqlWriteTask is writeTaskTx on MySQL: the UPDATE has no RETURNING, so the
// row is read back in the same transaction (the caller holds its row lock).
func mysqlWriteTask(ctx context.Context, q rowQuerier, x Task, refs string) (Task, error) {
	tx, err := flowTransaction(q)
	if err != nil {
		return Task{}, err
	}
	if _, err = tx.Exec(ctx, `UPDATE tasks SET state=?,refs=?,updated_at=?,claimed_by=?,awaiting_decision=?,started_at=?,completed_at=?,canceled_at=?,triaged_at=?,started_triage_at=? WHERE id=?`,
		x.State, refs, x.UpdatedAt, x.ClaimedBy, x.AwaitingDecision, x.StartedAt, x.CompletedAt, x.CanceledAt, x.TriagedAt, x.StartedTriageAt, x.ID); err != nil {
		return Task{}, err
	}
	var out Task
	err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=?`, x.ID), &out)
	return out, err
}

// mirrorTaskDecisionsFlowTx is mirrorTaskDecisionsTx for a task write. The
// decision records are the decisions area's (B6) and the helper still takes
// pgx.Tx, so on MySQL it is skipped exactly where it can write nothing: no
// request before or after, no awaiting flag before or after, and a state that
// does not end the task. Any other write is refused before the call.
func (s *Store) mirrorTaskDecisionsFlowTx(ctx context.Context, tx transaction, before taskDecisionBefore, after Task, by, note string, flagEvent int64, at time.Time) error {
	if s.dialect == mysqlDialect {
		if before.flagged || before.request != nil || after.AwaitingDecision || after.Refs.DecisionRequest != nil {
			// B6 dependency: mirrorTaskDecisionsTx needs the decisions area on MySQL.
			return ErrDialectUnsupported
		}
		if DecisionEndsTask(after.State) {
			// Ending a task supersedes its decision records. Until B6 creates
			// decision_requests there can be none; once it exists the helper is needed.
			has, err := s.mysqlTableExists(ctx, tx, "decision_requests")
			if err != nil {
				return err
			}
			if has {
				return ErrDialectUnsupported
			}
		}
		return nil
	}
	return mirrorTaskDecisionsTx(ctx, pgxTx(tx), before, after, by, note, flagEvent, at)
}

// supersedeDotsRunTokensFlowTx ends a reopened request's dots run token. Only
// an hk dots request ever has one (ReplyDots refuses any other task), so on
// MySQL a task that is not one has nothing to supersede; for a dots request the
// call waits for the dots area (B6), whose helper still takes pgx.Tx.
func (s *Store) supersedeDotsRunTokensFlowTx(ctx context.Context, tx transaction, x Task, now time.Time) error {
	if s.dialect == mysqlDialect {
		if IsHKDotsRequest(x) {
			// B6 dependency: supersedeDotsRunTokensTx needs the dots area on MySQL.
			return ErrDialectUnsupported
		}
		return nil
	}
	_, err := supersedeDotsRunTokensTx(ctx, pgxTx(tx), x.ID, now)
	return err
}
