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
// on) would otherwise stay in PG only: the legacy push and the import are
// gone (hk 4426). Before each Drive pass the comment worker prepares the
// missing ops here, with the enqueue's own rules (ytCommentOnOpTx). Nothing
// here calls YouTrack, and while comments are canonical in YouTrack the pass
// prepares nothing (hk 4122).

// YTCatchUpMax bounds the comments one catch-up pass considers.
const YTCatchUpMax = 500

// YTCatchUp is what one catch-up pass did: the ops it prepared, and the
// comments it skipped because a local source disagrees with their task's
// binding (each logged once per pass). Deferred is a pass that found
// comments but left them because another holder had the sync lock.
type YTCatchUp struct {
	Prepared []ytwrite.Op
	Skipped  []int64
	Deferred bool
}

// ytCatchUpSelectSQL lists the comments a catch-up prepares an op for, oldest
// first: a comment on a bound task above the binding's comments_from, written
// through an hk entry point (not the YouTrack activity pull, whose comments
// are never copied, and not one already mirrored — youtrack_comment_links is
// frozen since the legacy push's removal but its rows still count), with no
// ownership link and no op under its key. $1 the pull's author, $2 the
// principal, $3 the kind, $4 the key prefix (YTCommentKey), $5 comment ids to
// leave out, $6 the limit, $7 true for only the bindings that publish
// without a comment at write time: an issue_create op's (origin op_applied)
// and the automatic binder's (origin auto_verified, hk 4492). A comment
// committed between the binder's listing and its publication got no op, and
// is above the binding's comments_from, so this pass copies it even when the
// general catch-up is off.
const ytCatchUpSelectSQL = `SELECT c.id, c.task_id, c.body, c.author, c.created_at
	FROM yt_task_bindings b JOIN task_comments c ON c.task_id = b.task_id AND c.id > b.comments_from
	WHERE c.author <> $1 AND (NOT $7 OR b.origin IN ('op_applied','auto_verified'))
	  AND NOT EXISTS (SELECT 1 FROM youtrack_comment_links l WHERE l.hk_comment_id = c.id)
	  AND NOT EXISTS (SELECT 1 FROM yt_comment_write_links w WHERE w.comment_id = c.id)
	  AND NOT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.principal = $2 AND o.kind = $3 AND o.idem_key = $4 || c.id::text)
	  AND NOT (c.id = ANY($5::bigint[]))
	ORDER BY c.id LIMIT $6`

// ytCatchUpSelectMySQL is ytCatchUpSelectSQL on MySQL. youtrack_comment_links
// is an empty table the port leaves out (see migration
// 20261118-mysql-yt-write-ops), so its clause goes too.
const ytCatchUpSelectMySQLHead = `SELECT c.id, c.task_id, c.body, c.author, c.created_at
	FROM yt_task_bindings b JOIN task_comments c ON c.task_id = b.task_id AND c.id > b.comments_from
	WHERE c.author <> ? AND (NOT ? OR b.origin IN ('op_applied','auto_verified'))
	  AND NOT EXISTS (SELECT 1 FROM yt_comment_write_links w WHERE w.comment_id = c.id)
	  AND NOT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.principal = ? AND o.kind = ? AND o.idem_key = CONCAT(?, CAST(c.id AS CHAR)))
	  AND NOT (c.id IN (`
const ytCatchUpSelectMySQLTail = `))
	ORDER BY c.id LIMIT ?`

// ytCatchUpArgs orders the candidate scan's arguments for each dialect.
func ytCatchUpArgs(s *Store, skip []int64, limit int, issueOnly bool) []any {
	if s.dialect == mysqlDialect {
		args := []any{YouTrackOwnedActor, issueOnly, YTCommentPrincipal, ytwrite.KindCommentAppend, ytCommentKeyPrefix}
		for _, id := range skip {
			args = append(args, id)
		}
		return append(args, limit)
	}
	return []any{YouTrackOwnedActor, YTCommentPrincipal, ytwrite.KindCommentAppend, ytCommentKeyPrefix, skip, limit, issueOnly}
}

// CatchUpYTComments prepares, in mode on, the op of each comment that the
// enqueue would have copied but that has none: up to limit comments, oldest
// first, each in its own transaction. skip lists comment ids to leave out
// (the worker passes the ones an earlier pass skipped as ambiguous, so they
// are logged once and never crowd out the others).
//
// No legacy push runs any more (hk 4426), but a pass that finds comments
// still works only while it holds the sync lock (TryYouTrackSyncLock): the
// lock is what an authority switch to youtrack tries first
// (SetYTAuthority), so no switch commits in the middle of a pass, and each
// comment's transaction reads the authority row FOR SHARE besides. A lock
// another holder has defers the pass (Deferred, nothing written); the
// per-comment read under the lane lock re-checks the comment's link and op,
// so a writer that finished after the scan is seen. Any other mode, and
// comments canonical in YouTrack, do nothing. It is idempotent: under the
// task's lane lock it reads the comment's link and its op again, and
// prepareYTOpTx keeps one op per (principal, kind, key), so two workers
// (blue-green) prepare one op.
//
// Locks (lockorder.go): the session-scoped sync lock, only tried, never
// waited for; then per comment the comments authority row FOR SHARE, the
// task's lane lock, and what ytCommentOnOpTx reads and writes. It never takes
// taskCommentInsertLock or a task row.
func (s *Store) CatchUpYTComments(ctx context.Context, limit int, skip []int64) (YTCatchUp, error) {
	return s.catchUpYTComments(ctx, limit, skip, false)
}

// CatchUpYTIssueComments is CatchUpYTComments for the bindings that publish
// without a comment at write time: the ones an issue_create op published
// (hk 3914) and the automatic binder's auto_verified ones (hk 4492). Such a
// task's comments written while its op was pending, or between the binder's
// YouTrack listing and its publication, got no op (the task had no binding
// yet), and the binding's comments_from (0 for a created issue, the highest
// comment the binder found present, the latest comment for an adopted one)
// says which of them the issue still lacks. The write worker runs it in mode
// on even when the general catch-up (HANDOFFKEEP_YT_COMMENTS_CATCHUP) is off.
func (s *Store) CatchUpYTIssueComments(ctx context.Context, limit int, skip []int64) (YTCatchUp, error) {
	return s.catchUpYTComments(ctx, limit, skip, true)
}

func (s *Store) catchUpYTComments(ctx context.Context, limit int, skip []int64, issueOnly bool) (YTCatchUp, error) {
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
	// Comments canonical in YouTrack get no PG→YouTrack copy (hk 4122): the
	// pass prepares nothing. This read is unlocked; each comment's
	// transaction reads the row FOR SHARE and checks again, so a switch
	// committing mid-pass still stops it.
	side, err := s.YTAuthorityOf(ctx, YTAuthorityComments)
	if err != nil {
		return out, err
	}
	if side == YTAuthorityYouTrack {
		return out, nil
	}
	selectSQL := ytCatchUpSelectSQL
	if s.dialect == mysqlDialect {
		selectSQL = ytCatchUpSelectMySQLHead + placeholders(len(skip)) + ytCatchUpSelectMySQLTail
	}
	rows, err := s.db().Query(ctx, selectSQL, ytCatchUpArgs(s, skip, limit, issueOnly)...)
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
	if len(comments) == 0 {
		return out, nil
	}
	lock, held, err := s.TryYouTrackSyncLock(ctx)
	if err != nil {
		return out, err
	}
	if !held {
		log.Printf("ytwrite.comment_catchup_deferred comments=%d reason=sync_running", len(comments))
		out.Deferred = true
		return out, nil
	}
	defer func() {
		if err := lock.Release(context.Background()); err != nil {
			log.Printf("ytwrite.comment_catchup_unlock_failed err=%q", err.Error())
		}
	}()
	// The comments' transactions run on the lock's own connection: the pass
	// needs no second pool connection, so it cannot wait on itself in a
	// pool of one.
	for _, c := range comments {
		op, prepared, source, err := s.catchUpYTComment(ctx, lock, c)
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

// catchUpYTComment is one comment of a catch-up pass, in its own transaction
// on the lock's own connection (the pass holds the sync lock: MySQL's is a
// session lock, so the transaction must run on that session).
func (s *Store) catchUpYTComment(ctx context.Context, lock *YouTrackSyncLock, c TaskComment) (op ytwrite.Op, prepared bool, source string, err error) {
	if lock.mysqlConn != nil {
		err = mysqlWriteTransactionOnce(ctx, lock.mysqlConn, func(tx transaction) error {
			var e error
			op, prepared, source, e = catchUpYTCommentTx(ctx, tx, c)
			return e
		})
		return op, prepared, source, err
	}
	tx, err := lock.conn.Begin(ctx)
	if err != nil {
		return ytwrite.Op{}, false, "", err
	}
	defer tx.Rollback(ctx)
	op, prepared, source, err = catchUpYTCommentTx(ctx, pgTransaction{tx}, c)
	if err != nil {
		return ytwrite.Op{}, false, "", err
	}
	return op, prepared, source, tx.Commit(ctx)
}

// catchUpYTCommentTx prepares comment c's op in the caller's transaction.
// prepared is false when comments are canonical in YouTrack, when another
// writer linked the comment first (an op link or a youtrack_comment_links
// row) or the task has no binding; source is the
// disagreeing source when the binding is ambiguous.
func catchUpYTCommentTx(ctx context.Context, tx transaction, c TaskComment) (ytwrite.Op, bool, string, error) {
	// FOR SHARE on the comments authority row, as the audit enqueue reads it
	// (EnqueueYTAuditComment): a switch to youtrack commits before this read
	// or after this transaction, never inside it.
	side, err := ytAuthorityShareTx(ctx, tx, YTAuthorityComments)
	if err != nil {
		return ytwrite.Op{}, false, "", err
	}
	if side == YTAuthorityYouTrack {
		return ytwrite.Op{}, false, "", nil
	}
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(c.TaskID, 10)); err != nil {
		return ytwrite.Op{}, false, "", err
	}
	// Read again under the lane lock, with the sync lock held: another
	// worker's op and link, and a youtrack_comment_links row another writer
	// left after the scan.
	var owned bool
	if err := tx.QueryRow(ctx, sqlFor(tx, `SELECT EXISTS(SELECT 1 FROM yt_comment_write_links WHERE comment_id=$1)
		OR EXISTS(SELECT 1 FROM yt_write_ops WHERE principal=$2 AND kind=$3 AND idem_key=$4)
		OR EXISTS(SELECT 1 FROM youtrack_comment_links WHERE hk_comment_id=$1)`,
		`SELECT EXISTS(SELECT 1 FROM yt_comment_write_links WHERE comment_id=?)
		OR EXISTS(SELECT 1 FROM yt_write_ops WHERE principal=? AND kind=? AND idem_key=?)`),
		c.ID, YTCommentPrincipal, ytwrite.KindCommentAppend, YTCommentKey(c.ID)).Scan(&owned); err != nil {
		return ytwrite.Op{}, false, "", err
	}
	if owned {
		return ytwrite.Op{}, false, "", nil
	}
	op, prepared, source, err := ytCommentOnOpTx(ctx, tx, c, time.Now().UTC())
	return op, prepared, source, err
}
