package store

import (
	"context"
	"errors"
	"log"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The moved-comment requeue (hk 4569, redesign of hk 4457). A comment queued
// behind its task's issue_create (hk 4263) is rejected at its claim with
// ambiguous_issue when the binding the create published disagrees with a
// local source; the usual case is a task that changed project while its
// create was in flight, so the issue landed in the project the task left.
// Such a comment used to stay rejected for good.
//
// The first design (PR #383) kept the comment prepared until an issue_move
// put the issue in the task's project. That move is prepared after the
// comment, on the same strictly ordered lane, so the comment waited for a
// later op and the lane could cycle (review r4561433128, r4563234442,
// r4564127096, r4564745703). This design never holds anything: the claim
// rejects the comment as before, which keeps every lane dependency pointing
// at a smaller op id, and the comment worker's pass below copies it again,
// as a new op at the tail of the lane, once the binding agrees with every
// local source (the move applied, or the task came back). The new op takes
// over the comment's yt_comment_write_links row, so the comment has one
// owning op as before.

// YTRequeueMax bounds the comments one requeue pass copies again.
const YTRequeueMax = 500

// ytRequeueKeyInfix separates a requeued op's key from the comment key it
// extends: YTCommentKey(comment) + infix + the rejected op's id. One key per
// rejected op, so two workers (blue-green) prepare one op.
const ytRequeueKeyInfix = ":requeue:"

// ytRequeueKey is the idempotency key of the op that copies comment again
// after op fromOp was rejected.
func ytRequeueKey(commentID, fromOp int64) string {
	return YTCommentKey(commentID) + ytRequeueKeyInfix + strconv.FormatInt(fromOp, 10)
}

// YTRequeued is one comment a requeue pass copied again: the rejected op it
// replaced (From) and the new op (Op).
type YTRequeued struct {
	CommentID int64
	From      int64
	Op        ytwrite.Op
}

// ytRequeueSelectSQL lists the candidates of a requeue pass, oldest comment
// first: a comment_append op of the comment principal ($1) on tenant $2,
// rejected ambiguous_issue ($3) at its claim with no target and no attempt
// (a queued comment that was never sent), that still owns its comment
// (yt_comment_write_links; the comment exists on the op's task), on a task
// whose binding is the one the task's own
// applied issue_create op ($4 principal, $5 kind) published on the op's
// tenant, above the binding's comments_from, with no issue_move on the
// binding's issue still unresolved and comments not canonical in YouTrack,
// and that every local source agrees with now. The agreement is ytBindingSourcesSQL's, written over the
// binding's columns so a binding that still disagrees is never listed: it
// would otherwise stay at the head of the limit ($6) and starve the rest.
// The pass checks all of it again under the lane lock (ytBindingDisagreement,
// ytPendingMoveTx, the authority row).
const ytRequeueSelectSQL = `SELECT o.id, w.comment_id, b.task_id
	FROM yt_write_ops o
	JOIN yt_comment_write_links w ON w.op_id = o.id
	JOIN task_comments tc ON tc.id = w.comment_id AND tc.task_id::text = o.entity_id
	JOIN yt_task_bindings b ON b.task_id::text = o.entity_id AND b.tenant = o.tenant
	WHERE o.principal = $1 AND o.kind = '` + ytwrite.KindCommentAppend + `' AND o.tenant = $2 AND o.entity_kind = 'task'
	  AND o.state = 'rejected' AND o.reason = $3 AND o.yt_target_id = '' AND o.attempts = 0
	  AND w.comment_id > b.comments_from
	  AND b.origin = '` + YTBindingOpApplied + `'
	  AND EXISTS (SELECT 1 FROM yt_write_ops i WHERE i.id = b.op_id AND i.principal = $4 AND i.kind = $5
		AND i.state = 'applied' AND i.entity_kind = 'task' AND i.entity_id = o.entity_id)
	  AND NOT EXISTS (SELECT 1 FROM task_origins x WHERE x.task_id = b.task_id AND x.provider = 'youtrack'
		AND (coalesce(x.external_id,'') <> b.issue_id OR (coalesce(x.tenant,'') <> '' AND x.tenant <> b.tenant)))
	  AND NOT EXISTS (SELECT 1 FROM yt_entity_map m WHERE m.entity_kind = 'task' AND m.hk_key = o.entity_id AND m.yt_id <> b.issue_id)
	  AND NOT EXISTS (SELECT 1 FROM yt_entity_map m WHERE m.entity_kind = 'task' AND (m.tenant = b.tenant OR m.tenant = '')
		AND m.yt_id = b.issue_id AND m.hk_key <> o.entity_id)
	  AND NOT EXISTS (SELECT 1 FROM yt_comment_canary c WHERE c.task_id = b.task_id AND (c.tenant <> b.tenant OR c.issue_id <> b.issue_id))
	  AND NOT EXISTS (SELECT 1 FROM yt_comment_canary c WHERE c.tenant = b.tenant AND c.issue_id = b.issue_id AND c.task_id <> b.task_id)
	  AND NOT EXISTS (SELECT 1 FROM yt_task_bindings ob WHERE ob.tenant = b.tenant AND ob.issue_id = b.issue_id AND ob.task_id <> b.task_id)
	  AND (EXISTS (SELECT 1 FROM tasks t JOIN youtrack_projects p ON p.hk_project = t.project WHERE t.id = b.task_id AND p.yt_project_id = b.project_id)
		OR (EXISTS (SELECT 1 FROM tasks t WHERE t.id = b.task_id AND coalesce(t.project,'') = '')
			AND NOT EXISTS (SELECT 1 FROM youtrack_projects p WHERE p.yt_project_id = b.project_id)))
	  AND NOT EXISTS (SELECT 1 FROM yt_write_ops mv WHERE mv.kind = '` + ytwrite.KindIssueMove + `' AND mv.entity_kind = 'task'
		AND mv.entity_id = o.entity_id AND mv.yt_target_id = b.issue_id AND mv.state NOT IN ('applied','rejected'))
	  AND NOT EXISTS (SELECT 1 FROM yt_authority a WHERE a.kind = '` + string(YTAuthorityComments) + `' AND a.authority = '` + string(YTAuthorityYouTrack) + `')
	ORDER BY w.comment_id, o.id LIMIT $6`

// RequeueYTMovedComments copies again, for the worker of tenant, each comment
// whose queued op was rejected ambiguous_issue at its claim and whose task's
// binding every local source agrees with now: up to limit comments, oldest
// first, each in its own transaction. Each gets a new comment_append op that
// targets the binding's issue (key ytRequeueKey), prepared at the tail of the
// task's lane, so it waits only for ops with smaller ids, and the comment's
// link moves from the rejected op to it. The rejected op is not changed.
// Outside comments mode on it does nothing.
//
// It is idempotent: under the lane lock it reads the link again and moves it
// only from the rejected op, and the new op's key is fixed by the rejected
// op, so two workers (blue-green) prepare one op and move the link once.
//
// Locks (lockorder.go): per comment, the comments authority row FOR SHARE,
// then the task's lane lock, then plain reads of the link, the binding, its
// sources, the lane's pending moves and the comment, then a new yt_write_ops row (prepareYTOpTx) and an
// UPDATE of the comment's yt_comment_write_links row.
func (s *Store) RequeueYTMovedComments(ctx context.Context, tenant string, limit int) ([]YTRequeued, error) {
	out := []YTRequeued{}
	if s.ytCommentsModeNow() != YTCommentsOn || tenant == "" || limit < 1 {
		return out, nil
	}
	if limit > YTRequeueMax {
		limit = YTRequeueMax
	}
	type cand struct{ op, comment, task int64 }
	rows, err := s.pool.Query(ctx, ytRequeueSelectSQL, YTCommentPrincipal, tenant, ytwrite.ReasonAmbiguousIssue,
		YTIssuePrincipal, ytwrite.KindIssueCreate, limit)
	if err != nil {
		return out, err
	}
	cands := []cand{}
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.op, &c.comment, &c.task); err != nil {
			rows.Close()
			return out, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	// A candidate that fails is logged and passed over, so one bad row
	// (listed first on every pass, by comment id) never stops the rest; the
	// pass returns the first error after trying them all.
	var first error
	for _, c := range cands {
		r, done, err := s.requeueYTComment(ctx, c.op, c.comment, c.task)
		if err != nil {
			if ctx.Err() != nil {
				return out, err
			}
			log.Printf("ytwrite.comment_requeue_failed op_id=%d task_id=%d comment_id=%d err=%q", c.op, c.task, c.comment, err.Error())
			if first == nil {
				first = err
			}
			continue
		}
		if done {
			out = append(out, r)
		}
	}
	return out, first
}

// requeueYTComment copies comment again after its op fromOp was rejected, in
// its own transaction. done is false when nothing was written: the link no
// longer names fromOp (another worker moved it), or the binding is not the
// create's or a source disagrees with it now.
func (s *Store) requeueYTComment(ctx context.Context, fromOp, commentID, taskID int64) (YTRequeued, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return YTRequeued{}, false, err
	}
	defer tx.Rollback(ctx)
	// The authority row first, as the catch-up takes it: a switch commits
	// before this read or after this transaction. Rejected ops do not hold a
	// switch to youtrack, so a rejected comment can still be here after it;
	// with comments canonical in YouTrack nothing is copied there again
	// (review r4573479593), and the link stays on the rejected op.
	side, err := ytAuthorityShareTx(ctx, tx, YTAuthorityComments)
	if err != nil {
		return YTRequeued{}, false, err
	}
	if side == YTAuthorityYouTrack {
		return YTRequeued{}, false, nil
	}
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(taskID, 10)); err != nil {
		return YTRequeued{}, false, err
	}
	var linked int64
	err = tx.QueryRow(ctx, `SELECT op_id FROM yt_comment_write_links WHERE comment_id=$1`, commentID).Scan(&linked)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTRequeued{}, false, nil
	}
	if err != nil {
		return YTRequeued{}, false, err
	}
	if linked != fromOp {
		return YTRequeued{}, false, nil
	}
	from, err := scanYTOp(tx.QueryRow(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE id=$1`, fromOp))
	if err != nil {
		return YTRequeued{}, false, err
	}
	if from.State != ytwrite.StateRejected || from.Reason != ytwrite.ReasonAmbiguousIssue || from.YTTargetID != "" ||
		from.Attempts != 0 || from.Principal != YTCommentPrincipal || from.Kind != ytwrite.KindCommentAppend ||
		from.EntityKind != "task" || from.EntityID != strconv.FormatInt(taskID, 10) {
		return YTRequeued{}, false, nil
	}
	b, found, err := readYTBinding(ctx, tx, taskID)
	if err != nil || !found {
		return YTRequeued{}, false, err
	}
	var createID int64
	err = tx.QueryRow(ctx, `SELECT id FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND idem_key=$3 AND state='applied'`,
		YTIssuePrincipal, ytwrite.KindIssueCreate, ytwrite.IssueKey(taskID)).Scan(&createID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return YTRequeued{}, false, err
	}
	if b.Origin != YTBindingOpApplied || b.OpID == 0 || b.OpID != createID || b.Tenant != from.Tenant || commentID <= b.CommentsFrom {
		return YTRequeued{}, false, nil
	}
	source, err := ytBindingDisagreement(ctx, tx, b)
	if err != nil {
		return YTRequeued{}, false, err
	}
	if source != "" {
		log.Printf("ytwrite.comment_requeue_skipped op_id=%d task_id=%d comment_id=%d source=%s", fromOp, taskID, commentID, source)
		return YTRequeued{}, false, nil
	}
	// The binding's project is where the issue is now, not where it will be:
	// a move still unresolved on the lane would apply before the copy and
	// take the issue elsewhere (review r4573479597; the task left for B and
	// came back to A before the move to B applied). The copy waits for a
	// later pass: once the move applies the binding disagrees with the task
	// until the move back applies too. Nothing waits on the lane for it.
	pending, err := ytPendingMoveTx(ctx, tx, taskID, b.IssueID)
	if err != nil {
		return YTRequeued{}, false, err
	}
	if pending != "" {
		log.Printf("ytwrite.comment_requeue_skipped op_id=%d task_id=%d comment_id=%d source=pending_move", fromOp, taskID, commentID)
		return YTRequeued{}, false, nil
	}
	var c TaskComment
	if err := scanTaskComment(tx.QueryRow(ctx, `SELECT `+taskCommentColumns+` FROM task_comments WHERE id=$1`, commentID), &c); err != nil {
		return YTRequeued{}, false, err
	}
	n, err := ytwrite.CommentOp(ytwrite.CommentRequest{Principal: YTCommentPrincipal, Key: ytRequeueKey(commentID, fromOp),
		Actor: from.Actor, TaskID: taskID, YTIssueID: b.IssueID, Tenant: b.Tenant, Body: c.Body})
	if err != nil {
		return YTRequeued{}, false, err
	}
	op, _, err := prepareYTOpTx(ctx, tx, time.Now().UTC(), n)
	if err != nil {
		return YTRequeued{}, false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE yt_comment_write_links SET op_id=$3 WHERE comment_id=$1 AND op_id=$2`, commentID, fromOp, op.ID)
	if err != nil {
		return YTRequeued{}, false, err
	}
	if tag.RowsAffected() != 1 {
		return YTRequeued{}, false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return YTRequeued{}, false, err
	}
	log.Printf("ytwrite.comment_requeued op_id=%d from_op_id=%d task_id=%d comment_id=%d issue_id=%q", op.ID, fromOp, taskID, commentID, b.IssueID)
	return YTRequeued{CommentID: commentID, From: fromOp, Op: op}, true, nil
}
