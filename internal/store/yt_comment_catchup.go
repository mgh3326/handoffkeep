package store

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The comment catch-up (hk 3897). In mode on the enqueue copies a comment
// only when its task is bound at comment time and the writer enqueues at all.
// A comment written after the binding that got no op (a writer that does not
// enqueue, such as a dots reply, or a comment written while the mode was not
// on) would otherwise reach YouTrack only through the legacy push and the
// import. Before each Drive pass the comment worker prepares the missing ops
// here, with the enqueue's own rules (ytCommentOnOpTx). Nothing here calls
// YouTrack.

// YTCatchUpMax bounds the comments one catch-up pass considers.
const YTCatchUpMax = 500

// YTCatchUp is what one catch-up pass did: the ops it prepared, and the
// comments it skipped because a local source disagrees with their task's
// binding (each logged once per pass).
type YTCatchUp struct {
	Prepared []ytwrite.Op
	Skipped  []int64
}

// ytCatchUpSelectSQL lists the comments a catch-up prepares an op for, oldest
// first: a comment on a bound task above the binding's comments_from, written
// through an hk entry point (not the YouTrack activity pull, whose comments
// are never copied, and not one the legacy push already mirrored), with no
// ownership link and no op under its key. $1 the pull's author, $2 the
// principal, $3 the kind, $4 the key prefix (YTCommentKey), $5 comment ids to
// leave out, $6 the limit.
const ytCatchUpSelectSQL = `SELECT c.id, c.task_id, c.body, c.author, c.created_at
	FROM yt_task_bindings b JOIN task_comments c ON c.task_id = b.task_id AND c.id > b.comments_from
	WHERE c.author <> $1
	  AND NOT EXISTS (SELECT 1 FROM youtrack_comment_links l WHERE l.hk_comment_id = c.id)
	  AND NOT EXISTS (SELECT 1 FROM yt_comment_write_links w WHERE w.comment_id = c.id)
	  AND NOT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.principal = $2 AND o.kind = $3 AND o.idem_key = $4 || c.id::text)
	  AND NOT (c.id = ANY($5::bigint[]))
	ORDER BY c.id LIMIT $6`

// CatchUpYTComments prepares, in mode on, the op of each comment that the
// enqueue would have copied but that has none: up to limit comments, oldest
// first, each in its own transaction. skip lists comment ids to leave out
// (the worker passes the ones an earlier pass skipped as ambiguous, so they
// are logged once and never crowd out the others). Any other mode does
// nothing. It is idempotent: under the task's lane lock it reads the
// comment's link and its op again, and prepareYTOpTx keeps one op per
// (principal, kind, key), so two workers (blue-green) prepare one op.
//
// Locks, per comment (lockorder.go): the task's lane lock as the first lock
// of the transaction, then what ytCommentOnOpTx reads and writes. It never
// takes taskCommentInsertLock or a task row.
func (s *Store) CatchUpYTComments(ctx context.Context, limit int, skip []int64) (YTCatchUp, error) {
	out := YTCatchUp{Prepared: []ytwrite.Op{}, Skipped: []int64{}}
	if s.ytCommentsModeNow() != YTCommentsOn || limit < 1 {
		return out, nil
	}
	if limit > YTCatchUpMax {
		limit = YTCatchUpMax
	}
	if skip == nil {
		skip = []int64{}
	}
	rows, err := s.pool.Query(ctx, ytCatchUpSelectSQL, YouTrackOwnedActor, YTCommentPrincipal, ytwrite.KindCommentAppend, ytCommentKeyPrefix, skip, limit)
	if err != nil {
		return out, err
	}
	comments := []TaskComment{}
	for rows.Next() {
		var c TaskComment
		if err := scanTaskComment(rows, &c); err != nil {
			rows.Close()
			return out, err
		}
		comments = append(comments, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}
	for _, c := range comments {
		op, prepared, source, err := s.catchUpYTComment(ctx, c)
		if err != nil {
			return out, err
		}
		switch {
		case source != "":
			log.Printf("ytwrite.comment_catchup_skipped_ambiguous task_id=%d comment_id=%d source=%s", c.TaskID, c.ID, source)
			out.Skipped = append(out.Skipped, c.ID)
		case prepared:
			log.Printf("ytwrite.comment_catchup task_id=%d comment_id=%d op_id=%d state=%s reason=%q", c.TaskID, c.ID, op.ID, op.State, op.Reason)
			out.Prepared = append(out.Prepared, op)
		}
	}
	return out, nil
}

// catchUpYTComment prepares comment c's op in its own transaction. prepared
// is false when another writer linked the comment first (an op link or a
// legacy push link) or the task has no binding; source is the disagreeing
// source when the binding is ambiguous.
func (s *Store) catchUpYTComment(ctx context.Context, c TaskComment) (ytwrite.Op, bool, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ytwrite.Op{}, false, "", err
	}
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(c.TaskID, 10)); err != nil {
		return ytwrite.Op{}, false, "", err
	}
	// Read again under the lane lock: another worker's op and link, and a
	// legacy push link committed since the scan (a push from before this
	// release, or one that read the comment while its binding disagreed).
	var owned bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_comment_write_links WHERE comment_id=$1)
		OR EXISTS(SELECT 1 FROM yt_write_ops WHERE principal=$2 AND kind=$3 AND idem_key=$4)
		OR EXISTS(SELECT 1 FROM youtrack_comment_links WHERE hk_comment_id=$1)`,
		c.ID, YTCommentPrincipal, ytwrite.KindCommentAppend, YTCommentKey(c.ID)).Scan(&owned); err != nil {
		return ytwrite.Op{}, false, "", err
	}
	if owned {
		return ytwrite.Op{}, false, "", nil
	}
	op, prepared, source, err := ytCommentOnOpTx(ctx, tx, c, time.Now().UTC())
	if err != nil || !prepared {
		return ytwrite.Op{}, false, source, err
	}
	return op, true, "", tx.Commit(ctx)
}
