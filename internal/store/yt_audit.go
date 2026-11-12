package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The comment audit (hk 3899, `handoffkeep youtrack audit`,
// docs/youtrack-write-protocol.md): for each bound task, compare its hk
// comments with its YouTrack issue's comments; with --enqueue, prepare the
// missing ones' ops through the enqueue path. Every read here is a plain
// read, so a read-only store (OpenReadOnly) can run the audit.

// YTAuditTaskPageMax bounds one page of YTAuditTasks.
const YTAuditTaskPageMax = 1000

// YTAuditTasks lists, in id order, a page of the tasks an audit covers:
// every task, or only project's when project is not "", or only task taskID
// when it is not 0; those with id greater than afterID, at most limit. The
// caller pages with the last id until a page comes back short.
func (s *Store) YTAuditTasks(ctx context.Context, project string, taskID, afterID int64, limit int) ([]YTCommentCandidate, error) {
	if taskID < 0 || afterID < 0 || limit < 1 || limit > YTAuditTaskPageMax {
		return nil, errors.New("invalid audit task filter")
	}
	rows, err := s.pool.Query(ctx, `SELECT id, coalesce(project,'') FROM tasks
		WHERE ($1 = '' OR project = $1) AND ($2 = 0 OR id = $2) AND id > $3 ORDER BY id LIMIT $4`, project, taskID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTCommentCandidate{}
	for rows.Next() {
		var c YTCommentCandidate
		if err := rows.Scan(&c.TaskID, &c.Project); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// YTAuditComments is what the audit compares for one task.
type YTAuditComments struct {
	// Comments are the task's comments that belong on its issue, in id
	// order: every comment except the ones the YouTrack activity pull wrote
	// (author YouTrackOwnedActor), which the enqueue never copies either.
	// OpID is the comment's comment_append op when that op targets issueID.
	Comments []ytwrite.AuditComment
	// Links are the comments' youtrack_comment_links values, as stored: a
	// YouTrack id the legacy mirror push posted, or one of its synthetic
	// markers (youtrack.CommentLinkIsID tells them apart).
	Links map[int64]string
	// PulledYTIDs are the YouTrack comments the pull copied into this task:
	// youtrack_comment_links rows whose hk comment the pull wrote. They came
	// from YouTrack and are not hk's to account for. A link whose hk comment
	// any other writer made is the legacy push's (Links).
	PulledYTIDs map[string]bool
}

// YTAuditTaskComments reads task taskID's comments for the audit against
// issue issueID.
func (s *Store) YTAuditTaskComments(ctx context.Context, taskID int64, issueID string) (YTAuditComments, error) {
	out := YTAuditComments{Comments: []ytwrite.AuditComment{}, Links: map[int64]string{}, PulledYTIDs: map[string]bool{}}
	rows, err := s.pool.Query(ctx, `SELECT c.id, c.body, c.author,
			coalesce((SELECT o.id FROM yt_comment_write_links w JOIN yt_write_ops o ON o.id = w.op_id
				WHERE w.comment_id = c.id AND o.kind = $2 AND o.yt_target_id = $3), 0),
			coalesce((SELECT l.youtrack_comment_id FROM youtrack_comment_links l WHERE l.hk_comment_id = c.id), '')
		FROM task_comments c WHERE c.task_id = $1 ORDER BY c.id`, taskID, ytwrite.KindCommentAppend, issueID)
	if err != nil {
		return YTAuditComments{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var c ytwrite.AuditComment
		var author, linked string
		if err := rows.Scan(&c.ID, &c.Body, &author, &c.OpID, &linked); err != nil {
			return YTAuditComments{}, err
		}
		if author == YouTrackOwnedActor {
			if linked != "" {
				out.PulledYTIDs[linked] = true
			}
			continue
		}
		if linked != "" {
			out.Links[c.ID] = linked
		}
		out.Comments = append(out.Comments, c)
	}
	return out, rows.Err()
}

// What EnqueueYTAuditComment did with one comment.
const (
	// YTAuditPrepared: a new op (prepared, or rejected locally when the
	// protocol cannot send the comment, as at any enqueue).
	YTAuditPrepared = "prepared"
	// YTAuditLinked: the comment already has its op; nothing was written.
	YTAuditLinked = "linked"
	// YTAuditSkipped: the task has no binding, or a source disagrees with
	// it; nothing was written (the enqueue's own rule).
	YTAuditSkipped = "skipped"
	// YTAuditAuthority: comments are canonical in YouTrack (hk 4122), so no
	// repair goes from PG to YouTrack; nothing was written.
	YTAuditAuthority = "authority_youtrack"
)

// EnqueueYTAuditComment prepares the op that copies comment commentID to its
// task's bound issue, as the comment's own enqueue in mode on would have
// (enqueueYTCommentOnTx): the same principal, the key YTCommentKey(comment
// id), the binding's tenant and issue, the comment's author and time, its
// ownership link and the task's map anchor. A comment that already has an op
// is left as it is (YTAuditLinked), so a second run writes nothing. A comment
// the YouTrack activity pull wrote is refused: it came from YouTrack.
//
// While comments are canonical in YouTrack it writes nothing
// (YTAuditAuthority).
//
// Locks, as the enqueue takes them after its comment insert (lockorder.go):
// KEY SHARE on the comment's task row, FOR SHARE on the comments authority
// row, the task's lane lock, then the new rows
// of yt_write_ops, yt_comment_write_links and yt_entity_map.
func (s *Store) EnqueueYTAuditComment(ctx context.Context, commentID int64) (string, ytwrite.Op, error) {
	if commentID < 1 {
		return "", ytwrite.Op{}, ErrTaskCommentInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", ytwrite.Op{}, err
	}
	defer tx.Rollback(ctx)
	var c TaskComment
	err = scanTaskComment(tx.QueryRow(ctx, `SELECT `+taskCommentColumns+` FROM task_comments WHERE id=$1`, commentID), &c)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ytwrite.Op{}, fmt.Errorf("comment %d does not exist", commentID)
	}
	if err != nil {
		return "", ytwrite.Op{}, err
	}
	if c.Author == YouTrackOwnedActor {
		return "", ytwrite.Op{}, fmt.Errorf("%w: comment %d came from YouTrack", ErrTaskCommentInvalid, commentID)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM tasks WHERE id=$1 FOR KEY SHARE`, c.TaskID); err != nil {
		return "", ytwrite.Op{}, err
	}
	// Read in this transaction and held FOR SHARE until its commit, so an
	// audit that started before a switch prepares nothing after it.
	side, err := ytAuthorityTx(ctx, tx, YTAuthorityComments, " FOR SHARE")
	if err != nil {
		return "", ytwrite.Op{}, err
	}
	if side == YTAuthorityYouTrack {
		return YTAuditAuthority, ytwrite.Op{}, nil
	}
	// The link is written only under the lane lock, so it cannot appear
	// between this check and the enqueue.
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(c.TaskID, 10)); err != nil {
		return "", ytwrite.Op{}, err
	}
	op, err := scanYTOp(tx.QueryRow(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops
		WHERE id = (SELECT op_id FROM yt_comment_write_links WHERE comment_id=$1)`, commentID))
	switch {
	case err == nil:
		return YTAuditLinked, op, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return "", ytwrite.Op{}, err
	}
	op, enqueued, err := enqueueYTCommentOnTx(ctx, tx, c)
	if err != nil {
		return "", ytwrite.Op{}, err
	}
	if !enqueued {
		return YTAuditSkipped, ytwrite.Op{}, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return "", ytwrite.Op{}, err
	}
	logYTCommentEnqueued(c, op, true)
	return YTAuditPrepared, op, nil
}
