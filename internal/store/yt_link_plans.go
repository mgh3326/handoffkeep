package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The Postgres side of the issue_links kind (hk 4638 S3; design
// 2026-11-13/yt-issue-links §5.1, §5.4): YTWriteLedger is a
// ytwrite.LinksLedger over the S2 tables yt_link_plans, yt_link_owned and
// yt_link_unowned. The plan itself is ytwrite.PlanIssueLinks, the same pure
// function the in-memory ledger calls; this file reads its inputs and writes
// its result in one transaction.
//
// Lock order (lockorder.go): PlanLinks and ApplyLinks take the op's lane lock
// first, then FOR UPDATE on the op row, then write yt_link_owned,
// yt_link_unowned and yt_link_plans rows (no other writer locks them) and
// read task_relations and yt_task_bindings without a lock. They take no task
// row.

// The kind is registered so the store knows it, but no gate admits it until
// its consumer and mode land (S4): the worker's ledger never claims one.
func init() { registerYTKindGate(ytwrite.KindIssueLinks, func(*Store) string { return `FALSE` }) }

var _ ytwrite.LinksLedger = (*YTWriteLedger)(nil)

const ytLinkOwnedColumns = `id, tenant, link_type, from_issue, to_issue, writer_task_id, target_task_id, kind,
	intended_op_id, intended_at, coalesce(released_op_id, 0), released_at`

func scanYTLinkOwned(r pgx.Row) (ytwrite.OwnedLink, error) {
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

func scanYTLinkUnowned(r pgx.Row) (ytwrite.UnownedLink, error) {
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

// lockYTOpTx takes op id's lane lock, then FOR UPDATE on its row.
func lockYTOpTx(ctx context.Context, tx pgx.Tx, id int64) (ytwrite.Op, error) {
	var kind, entity string
	err := tx.QueryRow(ctx, `SELECT entity_kind, entity_id FROM yt_write_ops WHERE id=$1`, id).Scan(&kind, &entity)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Op{}, ytwrite.ErrNotFound
	}
	if err != nil {
		return ytwrite.Op{}, err
	}
	if err := lockYTEntity(ctx, tx, kind, entity); err != nil {
		return ytwrite.Op{}, err
	}
	return scanYTOp(tx.QueryRow(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=$1 FOR UPDATE`, id))
}

func latestYTLinkPlan(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id int64) (ytwrite.LinkPlanRecord, bool, error) {
	var rec ytwrite.LinkPlanRecord
	var raw []byte
	err := q.QueryRow(ctx, `SELECT attempt, recover, plan, created_at FROM yt_link_plans WHERE op_id=$1 ORDER BY id DESC LIMIT 1`, id).
		Scan(&rec.Attempt, &rec.Recover, &raw, &rec.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.LinkPlanRecord{}, false, nil
	}
	if err != nil {
		return ytwrite.LinkPlanRecord{}, false, err
	}
	if _, err := scanJSONB(raw, &rec.Plan); err != nil {
		return ytwrite.LinkPlanRecord{}, false, fmt.Errorf("plan of op %d: %w", id, err)
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	return rec, true, nil
}

// ytLinkRowsTx reads the ownership rows naming issue on tenant (on either
// side) and the unowned rows seen from it, oldest first; open only, or all.
func ytLinkRowsTx(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, tenant, issue string, openOnly bool) ([]ytwrite.OwnedLink, []ytwrite.UnownedLink, error) {
	rows, err := q.Query(ctx, `SELECT `+ytLinkOwnedColumns+` FROM yt_link_owned
		WHERE tenant=$1 AND (from_issue=$2 OR to_issue=$2) AND (NOT $3 OR released_at IS NULL) ORDER BY id`, tenant, issue, openOnly)
	if err != nil {
		return nil, nil, err
	}
	owned, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ytwrite.OwnedLink, error) { return scanYTLinkOwned(r) })
	if err != nil {
		return nil, nil, err
	}
	rows, err = q.Query(ctx, `SELECT `+ytLinkUnownedColumns+` FROM yt_link_unowned
		WHERE tenant=$1 AND issue_id=$2 AND (NOT $3 OR gone_at IS NULL) ORDER BY id`, tenant, issue, openOnly)
	if err != nil {
		return nil, nil, err
	}
	unowned, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ytwrite.UnownedLink, error) { return scanYTLinkUnowned(r) })
	if err != nil {
		return nil, nil, err
	}
	return owned, unowned, nil
}

// ytLinkPeerEdgesTx lists the relates links of writer's issue that another
// task writes through a live edge: related edges where writer is the higher
// id (dst_id), as the src's bound issue on tenant.
func ytLinkPeerEdgesTx(ctx context.Context, tx pgx.Tx, tenant string, writer int64) ([]ytwrite.IssueLink, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT b.issue_id FROM task_relations r
		JOIN yt_task_bindings b ON b.task_id = r.src_id AND b.tenant = $2
		WHERE r.dst_id = $1 AND r.kind = 'related' AND r.archived_at IS NULL ORDER BY 1`, writer, tenant)
	if err != nil {
		return nil, err
	}
	issues, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make([]ytwrite.IssueLink, 0, len(issues))
	for _, id := range issues {
		out = append(out, ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: id})
	}
	return out, nil
}

func (l *YTWriteLedger) PlanLinks(ctx context.Context, now time.Time, id int64, attempt, recover int, cur []ytwrite.IssueLink) (ytwrite.LinkPlan, error) {
	now = now.UTC()
	tx, err := l.s.pool.Begin(ctx)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	defer tx.Rollback(ctx)
	op, err := lockYTOpTx(ctx, tx, id)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	latest, found, err := latestYTLinkPlan(ctx, tx, id)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	if err := ytwrite.CheckLinkPlanStep(op, attempt, recover, latest, found); err != nil {
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
		if _, err := tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=$2 WHERE id = ANY($1)`, ch.Release, id); err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	if len(ch.Unrelease) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=NULL WHERE id = ANY($1)`, ch.Unrelease); err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	if len(ch.Close) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=coalesce(released_op_id, $2), released_at=$3
			WHERE id = ANY($1)`, ch.Close, id, now); err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	// Ownership of each link about to be added commits with the plan, before
	// the request that adds it (I6).
	for _, n := range ch.Insert {
		if _, err := tx.Exec(ctx, `INSERT INTO yt_link_owned(tenant, link_type, from_issue, to_issue, writer_task_id,
				target_task_id, kind, intended_op_id, intended_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			n.Tenant, string(n.LinkType), n.From, n.To, n.WriterTask, n.TargetTask, n.Edge, id, now); err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	for _, u := range plan.Unowned {
		if _, err := tx.Exec(ctx, `INSERT INTO yt_link_unowned(tenant, issue_id, link_type, side, other_issue,
				first_seen_op_id, last_seen_op_id, first_seen_at, last_seen_at)
			VALUES ($1,$2,$3,$4,$5,$6,$6,$7,$7)
			ON CONFLICT (tenant, issue_id, link_type, side, other_issue) WHERE gone_at IS NULL
			DO UPDATE SET last_seen_op_id = GREATEST(yt_link_unowned.last_seen_op_id, EXCLUDED.last_seen_op_id),
				last_seen_at = EXCLUDED.last_seen_at`,
			op.Tenant, op.YTTargetID, string(u.Kind), ytwrite.UnownedSide(u.Kind), u.Target, id, now); err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	if _, gone := ytwrite.ClosedByRead(op, nil, unowned, cur); len(gone) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE yt_link_unowned SET gone_at=$2 WHERE id = ANY($1)`, gone, now); err != nil {
			return ytwrite.LinkPlan{}, err
		}
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yt_link_plans(op_id, attempt, recover, plan, created_at) VALUES ($1,$2,$3,$4::jsonb,$5)`,
		id, attempt, recover, string(raw), now); err != nil {
		return ytwrite.LinkPlan{}, err
	}
	return plan, tx.Commit(ctx)
}

func (l *YTWriteLedger) LatestLinkPlan(ctx context.Context, id int64) (ytwrite.LinkPlanRecord, bool, error) {
	return latestYTLinkPlan(ctx, l.s.pool, id)
}

func (l *YTWriteLedger) ApplyLinks(ctx context.Context, now time.Time, id int64, ytID string, cur []ytwrite.IssueLink) (ytwrite.Op, error) {
	now = now.UTC()
	tx, err := l.s.pool.Begin(ctx)
	if err != nil {
		return ytwrite.Op{}, err
	}
	defer tx.Rollback(ctx)
	op, err := lockYTOpTx(ctx, tx, id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	switch op.State {
	case ytwrite.StateApplied:
		if op.YTResultID == ytID {
			return op, nil
		}
		return ytwrite.Op{}, ytwrite.ErrConflictingResult
	case ytwrite.StateDispatched, ytwrite.StateRemoteUnknown:
	default:
		return ytwrite.Op{}, fmt.Errorf("%w: apply from %s", ytwrite.ErrState, op.State)
	}
	owned, unowned, err := ytLinkRowsTx(ctx, tx, op.Tenant, op.YTTargetID, true)
	if err != nil {
		return ytwrite.Op{}, err
	}
	closeOwned, gone := ytwrite.ClosedByRead(op, owned, unowned, cur)
	if len(closeOwned) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE yt_link_owned SET released_at=$2 WHERE id = ANY($1)`, closeOwned, now); err != nil {
			return ytwrite.Op{}, err
		}
	}
	if len(gone) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE yt_link_unowned SET gone_at=$2 WHERE id = ANY($1)`, gone, now); err != nil {
			return ytwrite.Op{}, err
		}
	}
	op, err = scanYTOp(tx.QueryRow(ctx, `UPDATE yt_write_ops SET state='applied', yt_result_id=$2, reason='', last_error='', resolved_at=$3
		WHERE id=$1 RETURNING `+ytOpColumns, id, ytID, now))
	if err != nil {
		return ytwrite.Op{}, err
	}
	return op, tx.Commit(ctx)
}

func (l *YTWriteLedger) LeaseLinkRecovery(ctx context.Context, now time.Time, id int64, reconciles int, until time.Time) (bool, error) {
	tag, err := l.s.pool.Exec(ctx, `UPDATE yt_write_ops SET next_attempt_at=$4
		WHERE id=$1 AND state='remote_unknown' AND next_attempt_at <= $2 AND reconciles=$3`, id, now.UTC(), reconciles, until.UTC())
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (l *YTWriteLedger) ReleaseUnsentLinks(ctx context.Context, now time.Time, id int64, links []ytwrite.IssueLink) error {
	tx, err := l.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	op, err := lockYTOpTx(ctx, tx, id)
	if err != nil {
		return err
	}
	writer, err := ytwrite.LinkWriter(op)
	if err != nil {
		return err
	}
	for _, link := range links {
		if _, err := tx.Exec(ctx, `UPDATE yt_link_owned SET released_op_id=$1, released_at=$2
			WHERE tenant=$3 AND from_issue=$4 AND writer_task_id=$5 AND link_type=$6 AND to_issue=$7
				AND released_op_id IS NULL AND released_at IS NULL`,
			id, now.UTC(), op.Tenant, op.YTTargetID, writer, string(link.Kind), link.Target); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (l *YTWriteLedger) LinkRecords(ctx context.Context, tenant, issueID string) (ytwrite.IssueLinkRecords, error) {
	owned, unowned, err := ytLinkRowsTx(ctx, l.s.pool, tenant, issueID, false)
	if err != nil {
		return ytwrite.IssueLinkRecords{}, err
	}
	return ytwrite.IssueLinkRecords{Owned: owned, Unowned: unowned}, nil
}
