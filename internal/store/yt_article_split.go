package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The store side of the article_split kind (hk 5085,
// design/2026-11-18/hk5085-yt-split): YTWriteLedger is a ytwrite.SplitLedger
// over the planned-op tables issue_links already keeps, yt_link_plans (one
// append-only row per plan; the plan column holds a ytwrite.SplitPlan, tagged
// by its kind) and yt_link_steps (the executor record), keyed by op. No
// migration: both tables exist on both dialects, and the index id is the op's
// own yt_result_id, written before the first part's request (MarkSplitStep)
// and kept on a rejected op (S9). The checks are ytwrite's (CheckLinkFence,
// CheckSplitPlan, CheckSplitMark, CheckSplitSettlement), the same the
// in-memory ledger applies. An applied split goes through Apply and its
// apply hook (yt_articles.go), which publishes the binding (S10).
//
// Lock order (lockorder.go): PlanSplit and SettleSplit take the op's lane
// lock, then FOR UPDATE on the op row, then write yt_link_plans or
// yt_link_steps rows and the op row, as PlanLinks and SettleLinks do.
// MarkSplitStep takes only FOR UPDATE on the op row, then inserts one
// yt_link_steps row and may set the op row's yt_result_id, as MarkLinkStep.

var _ ytwrite.SplitLedger = (*YTWriteLedger)(nil)

// ytSplitPlansTx reads op id's split plans, oldest first, each with how many
// steps were marked and the latest step's outcome.
func ytSplitPlansTx(ctx context.Context, q querier, id int64) ([]ytwrite.SplitPlanRecord, error) {
	rows, err := q.Query(ctx, sqlFor(q, `SELECT p.attempt, p.recover, p.plan,
			(SELECT count(*) FROM yt_link_steps s WHERE s.op_id=p.op_id AND s.attempt=p.attempt AND s.recover=p.recover),
			coalesce((SELECT s.outcome FROM yt_link_steps s WHERE s.op_id=p.op_id AND s.attempt=p.attempt AND s.recover=p.recover
				ORDER BY s.step DESC LIMIT 1), '')
		FROM yt_link_plans p WHERE p.op_id=$1 ORDER BY p.id`,
		`SELECT p.attempt, p.recover, p.plan,
			(SELECT count(*) FROM yt_link_steps s WHERE s.op_id=p.op_id AND s.attempt=p.attempt AND s.recover=p.recover),
			coalesce((SELECT s.outcome FROM yt_link_steps s WHERE s.op_id=p.op_id AND s.attempt=p.attempt AND s.recover=p.recover
				ORDER BY s.step DESC LIMIT 1), '')
		FROM yt_link_plans p WHERE p.op_id=? ORDER BY p.id`), id)
	if err != nil {
		return nil, err
	}
	return collectRows(rows, func(r row) (ytwrite.SplitPlanRecord, error) {
		var rec ytwrite.SplitPlanRecord
		var raw []byte
		var outcome string
		if err := r.Scan(&rec.Attempt, &rec.Recover, &raw, &rec.Marked, &outcome); err != nil {
			return rec, err
		}
		if _, err := scanJSONB(raw, &rec.Plan); err != nil {
			return rec, fmt.Errorf("split plan of op %d: %w", id, err)
		}
		if rec.Plan.Kind != ytwrite.SplitPlanTag {
			return rec, fmt.Errorf("plan %d.%d of op %d is not a split plan", rec.Attempt, rec.Recover, id)
		}
		rec.Outcome = ytwrite.StepOutcome(outcome)
		return rec, nil
	})
}

func (l *YTWriteLedger) SplitPlans(ctx context.Context, id int64) ([]ytwrite.SplitPlanRecord, error) {
	return ytSplitPlansTx(ctx, l.s.db(), id)
}

func (l *YTWriteLedger) PlanSplit(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, plan ytwrite.SplitPlan) error {
	return l.s.retryTx(ctx, func() error { return l.planSplitOnce(ctx, now, id, f, plan) })
}

// planSplitOnce is one attempt of PlanSplit's transaction.
func (l *YTWriteLedger) planSplitOnce(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, plan ytwrite.SplitPlan) error {
	now = now.UTC()
	tx, release, err := l.s.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	defer tx.Rollback(ctx)
	op, err := lockYTOpTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := ytwrite.CheckLinkFence(op, f, now); err != nil {
		return err
	}
	history, err := ytSplitPlansTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := ytwrite.CheckSplitPlan(op, history, f, plan); err != nil {
		return err
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sqlFor(tx, `INSERT INTO yt_link_plans(op_id, attempt, recover, plan, created_at) VALUES ($1,$2,$3,$4::jsonb,$5)`,
		`INSERT INTO yt_link_plans(op_id, attempt, recover, plan, created_at) VALUES (?,?,?,?,?)`),
		id, f.Attempt, f.Recover, string(raw), now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (l *YTWriteLedger) MarkSplitStep(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, step int, index string) error {
	return l.s.retryTx(ctx, func() error { return l.markSplitStepOnce(ctx, now, id, f, step, index) })
}

// markSplitStepOnce is one attempt of MarkSplitStep's transaction.
func (l *YTWriteLedger) markSplitStepOnce(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, step int, index string) error {
	tx, release, err := l.s.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	defer tx.Rollback(ctx)
	op, err := scanYTOp(tx.QueryRow(ctx, sqlFor(tx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=$1 FOR UPDATE`,
		`SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=? FOR UPDATE`), id))
	if errors.Is(err, ErrNoRows) {
		return ytwrite.ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := ytwrite.CheckLinkFence(op, f, now); err != nil {
		return err
	}
	history, err := ytSplitPlansTx(ctx, tx, id)
	if err != nil {
		return err
	}
	latest, found := ytwrite.LatestSplitPlan(history)
	if err := ytwrite.CheckSplitMark(op, latest, found, f, step, index); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sqlFor(tx, `INSERT INTO yt_link_steps(op_id, attempt, recover, step, marked_at) VALUES ($1,$2,$3,$4,$5)`,
		`INSERT INTO yt_link_steps(op_id, attempt, recover, step, marked_at) VALUES (?,?,?,?,?)`),
		id, f.Attempt, f.Recover, step, now.UTC()); err != nil {
		return err
	}
	if index != "" && op.YTResultID == "" {
		// The index id is durable before the request that depends on it, so
		// recovery reads the family by id, never by search (S1, S6).
		if _, err := tx.Exec(ctx, sqlFor(tx, `UPDATE yt_write_ops SET yt_result_id=$2 WHERE id=$1`,
			`UPDATE yt_write_ops SET yt_result_id=? WHERE id=?`), ytSplitIndexArgs(tx, id, index)...); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (l *YTWriteLedger) ApplySplit(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, st ytwrite.LinkSettlement, maps []ytwrite.MapRow) (ytwrite.Op, error) {
	var out ytwrite.Op
	err := l.s.retryTx(ctx, func() error {
		var err error
		out, err = l.applyOnce(ctx, now, id, st.YTID, maps, func(tx transaction, op ytwrite.Op) error {
			history, err := ytSplitPlansTx(ctx, tx, id)
			if err != nil {
				return err
			}
			return ytwrite.CheckSplitApply(op, history, f, st, now.UTC())
		})
		return err
	})
	return out, err
}

// ytSplitIndexArgs orders the index UPDATE's arguments: MySQL binds the SET
// value first.
func ytSplitIndexArgs(q any, id int64, index string) []any {
	if dialectFor(q) == mysqlDialect {
		return []any{index, id}
	}
	return []any{id, index}
}

func (l *YTWriteLedger) SettleSplit(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, st ytwrite.LinkSettlement) (ytwrite.Op, error) {
	var out ytwrite.Op
	err := l.s.retryTx(ctx, func() error {
		var err error
		out, err = l.settleSplitOnce(ctx, now, id, f, st)
		return err
	})
	return out, err
}

// settleSplitOnce is one attempt of SettleSplit's transaction.
func (l *YTWriteLedger) settleSplitOnce(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, st ytwrite.LinkSettlement) (ytwrite.Op, error) {
	now = now.UTC()
	tx, release, err := l.s.begin(ctx)
	if err != nil {
		return ytwrite.Op{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	op, err := lockYTOpTx(ctx, tx, id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	if err := ytwrite.CheckLinkFence(op, f, now); err != nil {
		return ytwrite.Op{}, err
	}
	if err := ytwrite.CheckSplitSettlement(op, st); err != nil {
		return ytwrite.Op{}, err
	}
	history, err := ytSplitPlansTx(ctx, tx, id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	latest, found := ytwrite.LatestSplitPlan(history)
	if f.Epoch == ytwrite.EpochOperator {
		if err := ytwrite.CheckSplitOperator(op, history, st); err != nil {
			return ytwrite.Op{}, err
		}
	}
	if found {
		if step, outcome, ok := st.StepRecord(latest.Marked); ok && latest.Attempt == f.Attempt && latest.Recover == f.Recover && step == latest.Marked-1 {
			if _, err := tx.Exec(ctx, sqlFor(tx, `UPDATE yt_link_steps SET outcome=$5 WHERE op_id=$1 AND attempt=$2 AND recover=$3 AND step=$4`,
				`UPDATE yt_link_steps SET outcome=? WHERE op_id=? AND attempt=? AND recover=? AND step=?`),
				ytLinkStepOutcomeArgs(tx, id, f, step, string(outcome))...); err != nil {
				return ytwrite.Op{}, err
			}
		}
	}
	index := op.YTResultID
	if st.YTID != "" {
		index = st.YTID
	}
	reason, detail := clipText(st.Reason, 200), clipText(st.Detail, 2000)
	switch st.To {
	case ytwrite.StateRejected:
		op, err = settleYTOpTx(ctx, tx, id, `UPDATE yt_write_ops SET state='rejected', reason=$2, last_error=$3, resolved_at=$4, yt_result_id=$5
			WHERE id=$1 RETURNING `+ytOpColumns,
			`UPDATE yt_write_ops SET state='rejected', reason=?, last_error=?, resolved_at=?, yt_result_id=?
			WHERE id=?`, []any{id, reason, detail, now, index}, []any{reason, detail, now, index, id})
	default: // prepared, remote_unknown (CheckSplitSettlement)
		op, err = settleYTOpTx(ctx, tx, id, `UPDATE yt_write_ops SET state=$2, next_attempt_at=$3, reason=$4, last_error=$5, yt_result_id=$6
			WHERE id=$1 RETURNING `+ytOpColumns,
			`UPDATE yt_write_ops SET state=?, next_attempt_at=?, reason=?, last_error=?, yt_result_id=?
			WHERE id=?`, []any{id, string(st.To), st.Next.UTC(), reason, detail, index}, []any{string(st.To), st.Next.UTC(), reason, detail, index, id})
	}
	if err != nil {
		return ytwrite.Op{}, err
	}
	return op, tx.Commit(ctx)
}
