package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The store side of the issue_links kind (hk 4638 S3; design
// 2026-11-13/yt-issue-links §5.1, §5.4): YTWriteLedger is a
// ytwrite.LinksLedger over the S2 tables yt_link_plans, yt_link_owned and
// yt_link_unowned and the executor record yt_link_steps. The plan itself is
// ytwrite.PlanIssueLinks, the same pure function the in-memory ledger calls,
// and so are the fence (CheckLinkFence) and the release rule
// (UnsentLinkAdds); this file reads their inputs and writes their result in
// one transaction. Each statement has a PostgreSQL and a MySQL text: MySQL has
// no RETURNING (a settlement reads its row back) and no = ANY(array) (an id
// list is an IN list), and the two partial unique indexes over open rows are
// functional keys there, so their ON CONFLICT upserts are ON DUPLICATE KEY
// UPDATE (hk 5077).
//
// Lock order (lockorder.go): PlanLinks and SettleLinks take the op's lane
// lock first, then FOR UPDATE on the op row, then write yt_link_owned,
// yt_link_unowned, yt_link_plans and yt_link_steps rows (no other writer
// locks them) and read task_relations and yt_task_bindings without a lock.
// MarkLinkStep takes only FOR UPDATE on the op row, then writes one
// yt_link_steps row. None takes a task row.

// The kind's admission gate is ytLinksGateSQL (yt_links.go): the worker's
// ledger claims an issue_links op only while the mode is on and the op's
// target is its writer's binding.

var _ ytwrite.LinksLedger = (*YTWriteLedger)(nil)

const ytLinkOwnedColumns = `id, tenant, link_type, from_issue, to_issue, writer_task_id, target_task_id, kind,
	intended_op_id, intended_at, coalesce(released_op_id, 0), released_at`

func scanYTLinkOwned(r row) (ytwrite.OwnedLink, error) {
	var o ytwrite.OwnedLink
	var linkType string
	var releasedAt *time.Time
	if err := r.Scan(&o.ID, &o.Tenant, &linkType, &o.From, &o.To, &o.WriterTask, &o.TargetTask, &o.Edge,
		&o.IntendedOp, &o.IntendedAt, &o.ReleasedOp, &releasedAt); err != nil {
		return ytwrite.OwnedLink{}, err
	}
	o.LinkType, o.IntendedAt = ytwrite.LinkKind(linkType), o.IntendedAt.UTC()
	if releasedAt != nil {
		o.ReleasedAt = releasedAt.UTC()
	}
	return o, nil
}

const ytLinkUnownedColumns = `id, tenant, issue_id, link_type, side, other_issue, first_seen_op_id, last_seen_op_id,
	first_seen_at, last_seen_at, gone_at`

func scanYTLinkUnowned(r row) (ytwrite.UnownedLink, error) {
	var u ytwrite.UnownedLink
	var linkType string
	var goneAt *time.Time
	if err := r.Scan(&u.ID, &u.Tenant, &u.IssueID, &linkType, &u.Side, &u.OtherIssue, &u.FirstSeenOp, &u.LastSeenOp,
		&u.FirstSeenAt, &u.LastSeenAt, &goneAt); err != nil {
		return ytwrite.UnownedLink{}, err
	}
	u.LinkType, u.FirstSeenAt, u.LastSeenAt = ytwrite.LinkKind(linkType), u.FirstSeenAt.UTC(), u.LastSeenAt.UTC()
	if goneAt != nil {
		u.GoneAt = goneAt.UTC()
	}
	return u, nil
}

// ytLinkIDList renders an `IN (...)` list of ids for MySQL, which has no
// = ANY(array) (hk 5077), with its arguments.
func ytLinkIDList(ids []int64) (string, []any) {
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i], args[i] = "?", id
	}
	return strings.Join(marks, ", "), args
}

// lockYTOpTx takes op id's lane lock, then FOR UPDATE on its row.
func lockYTOpTx(ctx context.Context, tx transaction, id int64) (ytwrite.Op, error) {
	var kind, entity string
	err := tx.QueryRow(ctx, sqlFor(tx, `SELECT entity_kind, entity_id FROM yt_write_ops WHERE id=$1`,
		`SELECT entity_kind, entity_id FROM yt_write_ops WHERE id=?`), id).Scan(&kind, &entity)
	if errors.Is(err, ErrNoRows) {
		return ytwrite.Op{}, ytwrite.ErrNotFound
	}
	if err != nil {
		return ytwrite.Op{}, err
	}
	if err := lockYTEntity(ctx, tx, kind, entity); err != nil {
		return ytwrite.Op{}, err
	}
	return scanYTOp(tx.QueryRow(ctx, sqlFor(tx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=$1 FOR UPDATE`,
		`SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=? FOR UPDATE`), id))
}

func latestYTLinkPlan(ctx context.Context, q querier, id int64) (ytwrite.LinkPlanRecord, bool, error) {
	var rec ytwrite.LinkPlanRecord
	var raw []byte
	var outcome string
	// Steps are marked in order from 0, so their count is the next step.
	err := q.QueryRow(ctx, sqlFor(q, `SELECT p.attempt, p.recover, p.plan, p.created_at,
			(SELECT count(*) FROM yt_link_steps s WHERE s.op_id=p.op_id AND s.attempt=p.attempt AND s.recover=p.recover),
			coalesce((SELECT s.outcome FROM yt_link_steps s WHERE s.op_id=p.op_id AND s.attempt=p.attempt AND s.recover=p.recover
				ORDER BY s.step DESC LIMIT 1), '')
		FROM yt_link_plans p WHERE p.op_id=$1 ORDER BY p.id DESC LIMIT 1`,
		`SELECT p.attempt, p.recover, p.plan, p.created_at,
			(SELECT count(*) FROM yt_link_steps s WHERE s.op_id=p.op_id AND s.attempt=p.attempt AND s.recover=p.recover),
			coalesce((SELECT s.outcome FROM yt_link_steps s WHERE s.op_id=p.op_id AND s.attempt=p.attempt AND s.recover=p.recover
				ORDER BY s.step DESC LIMIT 1), '')
		FROM yt_link_plans p WHERE p.op_id=? ORDER BY p.id DESC LIMIT 1`), id).
		Scan(&rec.Attempt, &rec.Recover, &raw, &rec.CreatedAt, &rec.Marked, &outcome)
	if errors.Is(err, ErrNoRows) {
		return ytwrite.LinkPlanRecord{}, false, nil
	}
	if err != nil {
		return ytwrite.LinkPlanRecord{}, false, err
	}
	if _, err := scanJSONB(raw, &rec.Plan); err != nil {
		return ytwrite.LinkPlanRecord{}, false, fmt.Errorf("plan of op %d: %w", id, err)
	}
	rec.CreatedAt, rec.Outcome = rec.CreatedAt.UTC(), ytwrite.StepOutcome(outcome)
	return rec, true, nil
}

// releaseUnsentTx closes the writer's open, unreleased rows of rec's unsent
// adds (ytwrite.UnsentLinkAdds) for op.
func releaseUnsentTx(ctx context.Context, tx transaction, op ytwrite.Op, rec ytwrite.LinkPlanRecord, now time.Time) error {
	links := ytwrite.UnsentLinkAdds(rec)
	if len(links) == 0 {
		return nil
	}
	writer, err := ytwrite.LinkWriter(op)
	if err != nil {
		return err
	}
	for _, link := range links {
		if _, err := tx.Exec(ctx, sqlFor(tx, `UPDATE yt_link_owned SET released_op_id=$1, released_at=$2
			WHERE tenant=$3 AND from_issue=$4 AND writer_task_id=$5 AND link_type=$6 AND to_issue=$7
				AND released_op_id IS NULL AND released_at IS NULL`,
			`UPDATE yt_link_owned SET released_op_id=?, released_at=?
			WHERE tenant=? AND from_issue=? AND writer_task_id=? AND link_type=? AND to_issue=?
				AND released_op_id IS NULL AND released_at IS NULL`),
			op.ID, now, op.Tenant, op.YTTargetID, writer, string(link.Kind), link.Target); err != nil {
			return err
		}
	}
	return nil
}

// ytLinkRowsTx reads the ownership rows naming issue on tenant (on either
// side) and the unowned rows seen from it, oldest first; open only, or all.
func ytLinkRowsTx(ctx context.Context, q querier, tenant, issue string, openOnly bool) ([]ytwrite.OwnedLink, []ytwrite.UnownedLink, error) {
	rows, err := q.Query(ctx, sqlFor(q, `SELECT `+ytLinkOwnedColumns+` FROM yt_link_owned
		WHERE tenant=$1 AND (from_issue=$2 OR to_issue=$2) AND (NOT $3 OR released_at IS NULL) ORDER BY id`,
		`SELECT `+ytLinkOwnedColumns+` FROM yt_link_owned
		WHERE tenant=? AND (from_issue=? OR to_issue=?) AND (? = 0 OR released_at IS NULL) ORDER BY id`),
		ytLinkRowArgs(q, tenant, issue, openOnly)...)
	if err != nil {
		return nil, nil, err
	}
	owned, err := collectRows(rows, scanYTLinkOwned)
	if err != nil {
		return nil, nil, err
	}
	rows, err = q.Query(ctx, sqlFor(q, `SELECT `+ytLinkUnownedColumns+` FROM yt_link_unowned
		WHERE tenant=$1 AND issue_id=$2 AND (NOT $3 OR gone_at IS NULL) ORDER BY id`,
		`SELECT `+ytLinkUnownedColumns+` FROM yt_link_unowned
		WHERE tenant=? AND issue_id=? AND (? = 0 OR gone_at IS NULL) ORDER BY id`), tenant, issue, openOnly)
	if err != nil {
		return nil, nil, err
	}
	unowned, err := collectRows(rows, scanYTLinkUnowned)
	if err != nil {
		return nil, nil, err
	}
	return owned, unowned, nil
}

// ytLinkRowArgs orders ytLinkRowsTx's arguments: the owned rows name the
// issue on both sides, so MySQL repeats it (hk 5077).
func ytLinkRowArgs(q any, tenant, issue string, openOnly bool) []any {
	if dialectFor(q) == mysqlDialect {
		return []any{tenant, issue, issue, openOnly}
	}
	return []any{tenant, issue, openOnly}
}

// ytLinkPeerEdgesTx lists the relates links of writer's issue that another
// task writes through a live edge: related edges where writer is the higher
// id (dst_id), as the src's bound issue on tenant.
func ytLinkPeerEdgesTx(ctx context.Context, tx transaction, tenant string, writer int64) ([]ytwrite.IssueLink, error) {
	args := []any{writer, tenant}
	if dialectFor(tx) == mysqlDialect {
		args = []any{tenant, writer}
	}
	rows, err := tx.Query(ctx, sqlFor(tx, `SELECT DISTINCT b.issue_id FROM task_relations r
		JOIN yt_task_bindings b ON b.task_id = r.src_id AND b.tenant = $2
		WHERE r.dst_id = $1 AND r.kind = 'related' AND r.archived_at IS NULL ORDER BY 1`,
		`SELECT DISTINCT b.issue_id FROM task_relations r
		JOIN yt_task_bindings b ON b.task_id = r.src_id AND b.tenant = ?
		WHERE r.dst_id = ? AND r.kind = 'related' AND r.archived_at IS NULL ORDER BY 1`), args...)
	if err != nil {
		return nil, err
	}
	issues, err := collectRows(rows, rowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]ytwrite.IssueLink, 0, len(issues))
	for _, id := range issues {
		out = append(out, ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: id})
	}
	return out, nil
}

func (l *YTWriteLedger) PlanLinks(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, cur []ytwrite.IssueLink) (ytwrite.LinkPlan, error) {
	var out ytwrite.LinkPlan
	err := l.s.retryTx(ctx, func() error {
		var err error
		out, err = l.planLinksOnce(ctx, now, id, f, cur)
		return err
	})
	return out, err
}

// planLinksOnce is one attempt of PlanLinks's transaction.
func (l *YTWriteLedger) planLinksOnce(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, cur []ytwrite.IssueLink) (ytwrite.LinkPlan, error) {
	now = now.UTC()
	tx, release, err := l.s.begin(ctx)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	op, err := lockYTOpTx(ctx, tx, id)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	if err := ytwrite.CheckLinkFence(op, f, now); err != nil {
		return ytwrite.LinkPlan{}, err
	}
	latest, found, err := latestYTLinkPlan(ctx, tx, id)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	if err := ytwrite.CheckLinkPlanStep(op, f.Attempt, f.Recover, latest, found); err != nil {
		return ytwrite.LinkPlan{}, err
	}
	snap, err := ytwrite.IssueLinksPayload(op)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	writer, err := ytwrite.LinkWriter(op)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	// The plan this one supersedes: ownership of its unsent adds closes
	// first, so the new plan sees only links hk may have made.
	if found {
		if err := releaseUnsentTx(ctx, tx, op, latest, now); err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	owned, unowned, err := ytLinkRowsTx(ctx, tx, op.Tenant, op.YTTargetID, true)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	peers, err := ytLinkPeerEdgesTx(ctx, tx, op.Tenant, writer)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	plan, ch, err := ytwrite.PlanIssueLinks(op, snap, cur, owned, peers)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	if len(ch.Release) > 0 {
		if dialectFor(tx) == mysqlDialect {
			list, args := ytLinkIDList(ch.Release)
			_, err = tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=? WHERE id IN (`+list+`)`, append([]any{id}, args...)...)
		} else {
			_, err = tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=$2 WHERE id = ANY($1)`, ch.Release, id)
		}
		if err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	if len(ch.Unrelease) > 0 {
		if dialectFor(tx) == mysqlDialect {
			list, args := ytLinkIDList(ch.Unrelease)
			_, err = tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=NULL WHERE id IN (`+list+`)`, args...)
		} else {
			_, err = tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=NULL WHERE id = ANY($1)`, ch.Unrelease)
		}
		if err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	if len(ch.Close) > 0 {
		if dialectFor(tx) == mysqlDialect {
			list, args := ytLinkIDList(ch.Close)
			_, err = tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=coalesce(released_op_id, ?), released_at=?
				WHERE id IN (`+list+`)`, append([]any{id, now}, args...)...)
		} else {
			_, err = tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=coalesce(released_op_id, $2), released_at=$3
				WHERE id = ANY($1)`, ch.Close, id, now)
		}
		if err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	// Ownership of each link about to be added commits with the plan, before
	// the request that adds it (I6). yt_link_owned_live is a partial index on
	// PostgreSQL and a functional key that is all NULL once a row is
	// released on MySQL, so released rows never collide with the live one
	// (hk 5077).
	for _, n := range ch.Insert {
		if _, err := tx.Exec(ctx, sqlFor(tx, `INSERT INTO yt_link_owned(tenant, link_type, from_issue, to_issue, writer_task_id,
				target_task_id, kind, intended_op_id, intended_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			`INSERT INTO yt_link_owned(tenant, link_type, from_issue, to_issue, writer_task_id,
				target_task_id, kind, intended_op_id, intended_at)
			VALUES (?,?,?,?,?,?,?,?,?)`),
			n.Tenant, string(n.LinkType), n.From, n.To, n.WriterTask, n.TargetTask, n.Edge, id, now); err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	for _, u := range plan.Unowned {
		pgArgs := []any{op.Tenant, op.YTTargetID, string(u.Kind), ytwrite.UnownedSide(u.Kind), u.Target, id, now}
		myArgs := []any{op.Tenant, op.YTTargetID, string(u.Kind), ytwrite.UnownedSide(u.Kind), u.Target, id, id, now, now}
		args := pgArgs
		if dialectFor(tx) == mysqlDialect {
			args = myArgs
		}
		if _, err := tx.Exec(ctx, sqlFor(tx, `INSERT INTO yt_link_unowned(tenant, issue_id, link_type, side, other_issue,
				first_seen_op_id, last_seen_op_id, first_seen_at, last_seen_at)
			VALUES ($1,$2,$3,$4,$5,$6,$6,$7,$7)
			ON CONFLICT (tenant, issue_id, link_type, side, other_issue) WHERE gone_at IS NULL
			DO UPDATE SET last_seen_op_id = GREATEST(yt_link_unowned.last_seen_op_id, EXCLUDED.last_seen_op_id),
				last_seen_at = EXCLUDED.last_seen_at`,
			`INSERT INTO yt_link_unowned(tenant, issue_id, link_type, side, other_issue,
				first_seen_op_id, last_seen_op_id, first_seen_at, last_seen_at)
			VALUES (?,?,?,?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE last_seen_op_id = GREATEST(yt_link_unowned.last_seen_op_id, VALUES(last_seen_op_id)),
				last_seen_at = VALUES(last_seen_at)`),
			args...); err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	if _, gone := ytwrite.ClosedByRead(op, nil, unowned, cur); len(gone) > 0 {
		if dialectFor(tx) == mysqlDialect {
			list, args := ytLinkIDList(gone)
			_, err = tx.Exec(ctx, `UPDATE yt_link_unowned SET gone_at=? WHERE id IN (`+list+`)`, append([]any{now}, args...)...)
		} else {
			_, err = tx.Exec(ctx, `UPDATE yt_link_unowned SET gone_at=$2 WHERE id = ANY($1)`, gone, now)
		}
		if err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	// yt_link_plans is append-only on both dialects (triggers from the feed
	// migration on MySQL), so a plan is a plain INSERT and never an upsert.
	if _, err := tx.Exec(ctx, sqlFor(tx, `INSERT INTO yt_link_plans(op_id, attempt, recover, plan, created_at) VALUES ($1,$2,$3,$4::jsonb,$5)`,
		`INSERT INTO yt_link_plans(op_id, attempt, recover, plan, created_at) VALUES (?,?,?,?,?)`),
		id, f.Attempt, f.Recover, string(raw), now); err != nil {
		return ytwrite.LinkPlan{}, err
	}
	return plan, tx.Commit(ctx)
}

func (l *YTWriteLedger) LatestLinkPlan(ctx context.Context, id int64) (ytwrite.LinkPlanRecord, bool, error) {
	return latestYTLinkPlan(ctx, l.s.db(), id)
}

func (l *YTWriteLedger) MarkLinkStep(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, step int) error {
	return l.s.retryTx(ctx, func() error {
		return l.markLinkStepOnce(ctx, now, id, f, step)
	})
}

// markLinkStepOnce is one attempt of MarkLinkStep's transaction.
func (l *YTWriteLedger) markLinkStepOnce(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, step int) error {
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
	latest, found, err := latestYTLinkPlan(ctx, tx, id)
	if err != nil {
		return err
	}
	if !found || latest.Attempt != f.Attempt || latest.Recover != f.Recover || step != latest.Marked || step >= len(ytwrite.ExecuteOrder(latest.Plan)) {
		return fmt.Errorf("%w: mark step %d of plan %d.%d; latest is %d.%d (found %v), marked %d",
			ytwrite.ErrState, step, f.Attempt, f.Recover, latest.Attempt, latest.Recover, found, latest.Marked)
	}
	if _, err := tx.Exec(ctx, sqlFor(tx, `INSERT INTO yt_link_steps(op_id, attempt, recover, step, marked_at) VALUES ($1,$2,$3,$4,$5)`,
		`INSERT INTO yt_link_steps(op_id, attempt, recover, step, marked_at) VALUES (?,?,?,?,?)`),
		id, f.Attempt, f.Recover, step, now.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (l *YTWriteLedger) SettleLinks(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, st ytwrite.LinkSettlement) (ytwrite.Op, error) {
	var out ytwrite.Op
	err := l.s.retryTx(ctx, func() error {
		var err error
		out, err = l.settleLinksOnce(ctx, now, id, f, st)
		return err
	})
	return out, err
}

// settleLinksOnce is one attempt of SettleLinks's transaction.
func (l *YTWriteLedger) settleLinksOnce(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, st ytwrite.LinkSettlement) (ytwrite.Op, error) {
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
	if err := ytwrite.CheckLinkSettlement(op, st); err != nil {
		return ytwrite.Op{}, err
	}
	latest, found, err := latestYTLinkPlan(ctx, tx, id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	if f.Epoch == ytwrite.EpochOperator {
		if err := ytwrite.CheckOperatorSaw(st, latest, found); err != nil {
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
			latest.Outcome = outcome
		}
		if err := releaseUnsentTx(ctx, tx, op, latest, now); err != nil {
			return ytwrite.Op{}, err
		}
	}
	reason, detail := clipText(st.Reason, 200), clipText(st.Detail, 2000)
	switch st.To {
	case ytwrite.StateApplied:
		if st.Read {
			owned, unowned, err := ytLinkRowsTx(ctx, tx, op.Tenant, op.YTTargetID, true)
			if err != nil {
				return ytwrite.Op{}, err
			}
			closeOwned, gone := ytwrite.ClosedByRead(op, owned, unowned, st.Cur)
			if len(closeOwned) > 0 {
				if dialectFor(tx) == mysqlDialect {
					list, args := ytLinkIDList(closeOwned)
					_, err = tx.Exec(ctx, `UPDATE yt_link_owned SET released_at=? WHERE id IN (`+list+`)`, append([]any{now}, args...)...)
				} else {
					_, err = tx.Exec(ctx, `UPDATE yt_link_owned SET released_at=$2 WHERE id = ANY($1)`, closeOwned, now)
				}
				if err != nil {
					return ytwrite.Op{}, err
				}
			}
			if len(gone) > 0 {
				if dialectFor(tx) == mysqlDialect {
					list, args := ytLinkIDList(gone)
					_, err = tx.Exec(ctx, `UPDATE yt_link_unowned SET gone_at=? WHERE id IN (`+list+`)`, append([]any{now}, args...)...)
				} else {
					_, err = tx.Exec(ctx, `UPDATE yt_link_unowned SET gone_at=$2 WHERE id = ANY($1)`, gone, now)
				}
				if err != nil {
					return ytwrite.Op{}, err
				}
			}
		}
		op, err = settleYTOpTx(ctx, tx, id, `UPDATE yt_write_ops SET state='applied', yt_result_id=$2, reason='', last_error='', resolved_at=$3
			WHERE id=$1 RETURNING `+ytOpColumns,
			`UPDATE yt_write_ops SET state='applied', yt_result_id=?, reason='', last_error='', resolved_at=?
			WHERE id=?`, []any{id, st.YTID, now}, []any{st.YTID, now, id})
	case ytwrite.StateRejected:
		op, err = settleYTOpTx(ctx, tx, id, `UPDATE yt_write_ops SET state='rejected', reason=$2, last_error=$3, resolved_at=$4
			WHERE id=$1 RETURNING `+ytOpColumns,
			`UPDATE yt_write_ops SET state='rejected', reason=?, last_error=?, resolved_at=?
			WHERE id=?`, []any{id, reason, detail, now}, []any{reason, detail, now, id})
	default: // prepared, remote_unknown (CheckLinkSettlement)
		op, err = settleYTOpTx(ctx, tx, id, `UPDATE yt_write_ops SET state=$2, next_attempt_at=$3, reason=$4, last_error=$5
			WHERE id=$1 RETURNING `+ytOpColumns,
			`UPDATE yt_write_ops SET state=?, next_attempt_at=?, reason=?, last_error=?
			WHERE id=?`, []any{id, string(st.To), st.Next.UTC(), reason, detail}, []any{string(st.To), st.Next.UTC(), reason, detail, id})
	}
	if err != nil {
		return ytwrite.Op{}, err
	}
	return op, tx.Commit(ctx)
}

// ytLinkStepOutcomeArgs orders the step-outcome UPDATE's arguments: MySQL binds
// the outcome first (the SET precedes the WHERE), PostgreSQL numbers it $5.
func ytLinkStepOutcomeArgs(q any, id int64, f ytwrite.LinkFence, step int, outcome string) []any {
	if dialectFor(q) == mysqlDialect {
		return []any{outcome, id, f.Attempt, f.Recover, step}
	}
	return []any{id, f.Attempt, f.Recover, step, outcome}
}

// settleYTOpTx moves op id's row and returns it: the PostgreSQL statement has
// RETURNING, MySQL needs the read-back in the same transaction (hk 5077).
func settleYTOpTx(ctx context.Context, tx transaction, id int64, postgres, mysql string, pgArgs, myArgs []any) (ytwrite.Op, error) {
	if dialectFor(tx) == mysqlDialect {
		if _, err := tx.Exec(ctx, mysql, myArgs...); err != nil {
			return ytwrite.Op{}, err
		}
		return scanYTOp(tx.QueryRow(ctx, sqlFor(tx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=$1`,
			`SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=?`), id))
	}
	return scanYTOp(tx.QueryRow(ctx, postgres, pgArgs...))
}

func (l *YTWriteLedger) LeaseLinkRecovery(ctx context.Context, now time.Time, id int64, reconciles int, until time.Time) (int, bool, error) {
	var epoch int
	var leased bool
	err := l.s.retryTx(ctx, func() error {
		if l.s.dialect == mysqlDialect {
			// No RETURNING: the increment and the read of its value share one
			// transaction, so they see the same row (hk 5077).
			tx, release, err := l.s.begin(ctx)
			if err != nil {
				return err
			}
			defer release()
			defer tx.Rollback(ctx)
			var n int64
			n, err = tx.Exec(ctx, `UPDATE yt_write_ops SET next_attempt_at=?, reconciles=reconciles+1, reason=?
				WHERE id=? AND state='remote_unknown' AND next_attempt_at <= ? AND reconciles=?`,
				until.UTC(), ytwrite.ReasonLinkRecovery, id, now.UTC(), reconciles)
			if err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			if err := tx.QueryRow(ctx, `SELECT reconciles FROM yt_write_ops WHERE id=?`, id).Scan(&epoch); err != nil {
				return err
			}
			leased = true
			return tx.Commit(ctx)
		}
		err := l.s.db().QueryRow(ctx, `UPDATE yt_write_ops SET next_attempt_at=$4, reconciles=reconciles+1, reason=$5
			WHERE id=$1 AND state='remote_unknown' AND next_attempt_at <= $2 AND reconciles=$3 RETURNING reconciles`,
			id, now.UTC(), reconciles, until.UTC(), ytwrite.ReasonLinkRecovery).Scan(&epoch)
		if errors.Is(err, ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		leased = true
		return nil
	})
	return epoch, leased, err
}

func (l *YTWriteLedger) LinkRecords(ctx context.Context, tenant, issueID string) (ytwrite.IssueLinkRecords, error) {
	owned, unowned, err := ytLinkRowsTx(ctx, l.s.db(), tenant, issueID, false)
	if err != nil {
		return ytwrite.IssueLinkRecords{}, err
	}
	return ytwrite.IssueLinkRecords{Owned: owned, Unowned: unowned}, nil
}
