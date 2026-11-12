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

// Document copies (hk 4xxx, docs/youtrack-write-protocol.md "Document
// comments"): after the import ended, a task-tied report or brief reaches its
// task's YouTrack issue only through this path. The change-feed consumer
// task_docs reads every document change from sync_log and, in mode on, for
// each document whose key is a report or a brief (ytwrite.DocComments) and
// whose task is bound to its issue, prepares one comment_append op per part.
// The op key is doc-comment:<doc id>:<sha>[:part:n], so a changed document
// (a new sha) gets new comments, and a change that leaves the sha as it was
// prepares nothing.
//
// The ops are comment_append ops under their own principal, so the comment
// gate (ytCommentGateSQL) admits them only while this mode is on, and the
// hk-comment modes never admit them.

// YTDocCommentsMode gates the document copies (HANDOFFKEEP_YT_DOC_COMMENTS_MODE).
type YTDocCommentsMode string

const (
	// YTDocCommentsOff prepares no document op and admits none (the
	// default). The consumer's cursor still advances: a document changed
	// while off is not copied later unless it changes again.
	YTDocCommentsOff YTDocCommentsMode = "off"
	// YTDocCommentsOn prepares the comment ops of each changed task-tied
	// report or brief and admits document ops bound to their task's issue.
	YTDocCommentsOn YTDocCommentsMode = "on"
)

// ytDocCommentPrincipal owns (and is the actor of the protocol's keys for)
// the document copies.
const ytDocCommentPrincipal = "hk:doc-comments"

// ytDocCommentsConsumer is the change-feed consumer's name (its cursor's key).
const ytDocCommentsConsumer = "task_docs"

// SetYTDocCommentsMode sets the document-copy gate for this process. serve
// calls it once at start.
func (s *Store) SetYTDocCommentsMode(m YTDocCommentsMode) { s.ytDocComments.Store(m) }

// ytDocCommentsNow is the gate; unset, and any value that is not on, is off.
func (s *Store) ytDocCommentsNow() YTDocCommentsMode {
	switch m, _ := s.ytDocComments.Load().(YTDocCommentsMode); m {
	case YTDocCommentsOn:
		return m
	default:
		return YTDocCommentsOff
	}
}

func init() {
	registerYTFeedStoreConsumer(ytDocCommentsConsumer, []string{"document"}, ytDocCommentsHandler)
}

// ytDocCommentGateSQL admits a document op (ytCommentGateSQL): mode on, and
// o is a document copy whose target is the issue its task is bound to on o's
// tenant now. Mode off admits nothing.
func ytDocCommentGateSQL(s *Store) string {
	if s.ytDocCommentsNow() != YTDocCommentsOn {
		return `FALSE`
	}
	return `o.principal = '` + ytDocCommentPrincipal + `' AND o.entity_kind = 'task' AND EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)`
}

// ytDocRow is one document as the copy reads it. taskID is 0 for a document
// with no task.
type ytDocRow struct {
	id                      int64
	key, kind, body, sha256 string
	taskID                  int64
	createdBy               string
}

// readYTDocRows reads the documents in ids, by id; a deleted one is absent.
func readYTDocRows(ctx context.Context, q poolQuerier, ids []int64) (map[int64]ytDocRow, error) {
	rows, err := q.Query(ctx, `SELECT id, key, kind, body, sha256, coalesce(task_id,0), created_by
		FROM documents WHERE id = ANY($1::bigint[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]ytDocRow, len(ids))
	for rows.Next() {
		var d ytDocRow
		if err := rows.Scan(&d.id, &d.key, &d.kind, &d.body, &d.sha256, &d.taskID, &d.createdBy); err != nil {
			return nil, err
		}
		out[d.id] = d
	}
	return out, rows.Err()
}

// ytDocCommentKey is the idempotency key of part n (0-based) of a document
// at sha: doc-comment:<id>:<sha>, then :part:<n+1> for the parts after the
// first, as the import named its comments (hk-doc-comment:<id>[:part:n]).
func ytDocCommentKey(docID int64, sha string, part int) string {
	key := "doc-comment:" + strconv.FormatInt(docID, 10) + ":" + sha
	if part > 0 {
		key += ":part:" + strconv.Itoa(part+1)
	}
	return key
}

// ytDocResult is what one document did: the ops it prepared, the parts whose
// op already existed (the same sha), the parts whose key was taken by an
// op with another payload (conflicts), and, when it copied nothing, why
// (reason).
type ytDocResult struct {
	prepared, unchanged, conflicts int
	reason                         string
}

// Reasons a document copies nothing (ytDocResult.reason).
const (
	ytDocDeleted  = "deleted"
	ytDocNotTask  = "not_task_doc"
	ytDocUnbound  = "unbound"
	ytDocDisagree = "disagree"
	ytDocInvalid  = "invalid"
)

// prepareYTDocTx prepares, in tx at now, the comment ops of document d (found
// false: deleted). A document with no task, or whose key is not a report or
// a brief, is skipped. So is one whose task has no binding (unbound), or
// whose binding a local source disagrees with (disagree); the audit lists
// those. A part the protocol cannot send gets a rejected op, as a comment
// does. A key already taken by an op with another payload (the author or the
// binding's target changed since) is counted and logged, never fatal: a
// failure here would stop the feed on this row.
func prepareYTDocTx(ctx context.Context, tx pgx.Tx, now time.Time, d ytDocRow, found bool) (ytDocResult, error) {
	switch {
	case !found:
		return ytDocResult{reason: ytDocDeleted}, nil
	case d.taskID == 0 || d.sha256 == "":
		return ytDocResult{reason: ytDocNotTask}, nil
	}
	bodies, ok, err := ytwrite.DocComments(d.key, d.kind, d.body)
	if err != nil {
		log.Printf("ytwrite.doc_comments_skipped doc_id=%d task_id=%d reason=invalid err=%q", d.id, d.taskID, err.Error())
		return ytDocResult{reason: ytDocInvalid}, nil
	}
	if !ok {
		return ytDocResult{reason: ytDocNotTask}, nil
	}
	b, bound, err := readYTBinding(ctx, tx, d.taskID)
	if err != nil {
		return ytDocResult{}, err
	}
	if !bound {
		return ytDocResult{reason: ytDocUnbound}, nil
	}
	source, err := ytBindingDisagreement(ctx, tx, b)
	if err != nil {
		return ytDocResult{}, err
	}
	if source != "" {
		log.Printf("ytwrite.doc_comments_skipped doc_id=%d task_id=%d reason=disagree source=%s", d.id, d.taskID, source)
		return ytDocResult{reason: ytDocDisagree}, nil
	}
	var res ytDocResult
	for i, body := range bodies {
		req := ytwrite.CommentRequest{Principal: ytDocCommentPrincipal, Key: ytDocCommentKey(d.id, d.sha256, i), Actor: d.createdBy,
			TaskID: d.taskID, YTIssueID: b.IssueID, Tenant: b.Tenant, Body: body}
		reason, detail := "", ""
		if cerr := ytwrite.CheckDocComment(req); cerr != nil {
			reason, detail = ytwrite.ReasonInvalidComment, cerr.Error()
		}
		n, err := ytwrite.CommentOp(req)
		if err != nil {
			return res, err
		}
		op, made, err := prepareYTOpTx(ctx, tx, now.UTC(), n)
		if errors.Is(err, ytwrite.ErrKeyConflict) {
			res.conflicts++
			log.Printf("ytwrite.doc_comment_key_conflict doc_id=%d task_id=%d key=%q", d.id, d.taskID, n.Key)
			continue
		}
		if err != nil {
			return res, err
		}
		if !made {
			res.unchanged++
			continue
		}
		if reason != "" {
			if _, err := rejectYTOpTx(ctx, tx, now.UTC(), op.ID, reason, detail); err != nil {
				return res, err
			}
		}
		res.prepared++
	}
	return res, nil
}

// ytDocCommentsHandler is the task_docs consumer's handler for one pass at
// now. Mode off does nothing (the cursor advances). Otherwise each changed
// document, in sync_id order, goes through prepareYTDocTx, and the pass logs
// one ytwrite.doc_comments_feed line with the counts.
func ytDocCommentsHandler(s *Store, now time.Time) FeedHandler {
	return func(ctx context.Context, tx pgx.Tx, rows []FeedRow) error {
		if s.ytDocCommentsNow() != YTDocCommentsOn {
			return nil
		}
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			if id, err := strconv.ParseInt(r.ModelID, 10, 64); err == nil && id > 0 && r.Model == "document" {
				ids = append(ids, id)
			}
		}
		docs, err := readYTDocRows(ctx, tx, ids)
		if err != nil {
			return err
		}
		var total ytDocResult
		counts := map[string]int{}
		for _, id := range ids {
			d, found := docs[id]
			res, err := prepareYTDocTx(ctx, tx, now, d, found)
			if err != nil {
				return err
			}
			total.prepared += res.prepared
			total.unchanged += res.unchanged
			total.conflicts += res.conflicts
			if res.reason != "" {
				counts[res.reason]++
			}
		}
		log.Printf("ytwrite.doc_comments_feed documents=%d prepared=%d unchanged=%d conflicts=%d deleted=%d not_task_doc=%d unbound=%d disagree=%d invalid=%d",
			len(ids), total.prepared, total.unchanged, total.conflicts, counts[ytDocDeleted], counts[ytDocNotTask],
			counts[ytDocUnbound], counts[ytDocDisagree], counts[ytDocInvalid])
		return nil
	}
}
