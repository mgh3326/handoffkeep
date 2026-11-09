package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Slice b1a (hk 3739, docs/youtrack-write-protocol.md): a new hk comment on a
// task enrolled in the canary also gets a protocol-tracked copy on the task's
// YouTrack issue. PG stays canonical for comments; the comment row is written
// exactly as before, and its op, its ownership link and (for the first op) the
// task's map row are written in the same transaction. Nothing here calls
// YouTrack: the issue was verified at enrollment, and the worker in serve
// sends the op (ytwrite.Protocol.Drive).

// YTCommentsMode gates slice b1a (HANDOFFKEEP_YT_COMMENTS_MODE).
type YTCommentsMode string

const (
	// YTCommentsOff writes no op row (the default).
	YTCommentsOff YTCommentsMode = "off"
	// YTCommentsCanary prepares one comment_append op for each new comment
	// on a task in yt_comment_canary, in the comment's own transaction.
	YTCommentsCanary YTCommentsMode = "canary"
)

// YTCommentPrincipal owns the comment-copy ops; their idempotency key is
// YTCommentKey(comment id), so one persisted comment has at most one op.
const YTCommentPrincipal = "hk:task-comments"

// YTCommentKey is the idempotency key of the op that copies one comment.
func YTCommentKey(commentID int64) string { return "task-comment:" + strconv.FormatInt(commentID, 10) }

// SetYTCommentsMode sets the gate for this process. serve calls it once at
// start, before any request.
func (s *Store) SetYTCommentsMode(m YTCommentsMode) { s.ytCommentsCanary.Store(m == YTCommentsCanary) }

// enqueueYTCommentTx stores the op that copies comment c to YouTrack, inside
// the comment's transaction (after its insert, before its commit). It writes
// nothing when the gate is off or the task is not enrolled. A comment PG
// accepts but the protocol cannot send (a body quoting the op marker, an
// author the provenance line cannot carry) and a task whose map row names
// another issue get a rejected op: the comment is untouched and nothing is
// ever sent. Any database error fails the whole comment.
//
// Locks, in this order after the comment insert lock and the comment's KEY
// SHARE on its task: the op's entity lane lock (lockYTEntity), then new rows
// of yt_write_ops, yt_comment_write_links and yt_entity_map (lockorder.go).
func (s *Store) enqueueYTCommentTx(ctx context.Context, tx pgx.Tx, c TaskComment) (ytwrite.Op, bool, error) {
	if !s.ytCommentsCanary.Load() {
		return ytwrite.Op{}, false, nil
	}
	var issueID, projectID string
	err := tx.QueryRow(ctx, `SELECT issue_id, project_id FROM yt_comment_canary WHERE task_id=$1`, c.TaskID).Scan(&issueID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Op{}, false, nil
	}
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	req := ytwrite.CommentRequest{Principal: YTCommentPrincipal, Key: YTCommentKey(c.ID), Actor: c.Author, TaskID: c.TaskID, YTIssueID: issueID, Body: c.Body}
	reason, detail := "", ""
	if cerr := ytwrite.CheckComment(req); cerr != nil {
		reason, detail = ytwrite.ReasonInvalidComment, cerr.Error()
	}
	n, err := ytwrite.CommentOp(req)
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	now := c.CreatedAt.UTC()
	op, _, err := prepareYTOpTx(ctx, tx, now, n)
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yt_comment_write_links(comment_id, op_id, created_at) VALUES ($1,$2,$3)`, c.ID, op.ID, now); err != nil {
		return ytwrite.Op{}, false, err
	}
	if reason == "" {
		// The first op of a task anchors its map row; every later op must
		// agree with it. ON CONFLICT without a target also covers the issue
		// being mapped to another task.
		taskKey := strconv.FormatInt(c.TaskID, 10)
		tag, err := tx.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind, hk_key, yt_id, yt_parent_id, op_id, created_at)
			VALUES ('task',$1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, taskKey, issueID, projectID, op.ID, now)
		if err != nil {
			return ytwrite.Op{}, false, err
		}
		if tag.RowsAffected() == 0 {
			var have string
			err := tx.QueryRow(ctx, `SELECT yt_id FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$1`, taskKey).Scan(&have)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				reason, detail = ytwrite.ReasonAmbiguousIssue, "issue "+issueID+" is mapped to another task"
			case err != nil:
				return ytwrite.Op{}, false, err
			case have != issueID:
				reason, detail = ytwrite.ReasonAmbiguousIssue, "task is mapped to issue "+have+", canary names "+issueID
			}
		}
	}
	if reason != "" {
		if op, err = rejectYTOpTx(ctx, tx, now, op.ID, reason, detail); err != nil {
			return ytwrite.Op{}, false, err
		}
	}
	return op, true, nil
}

func logYTCommentEnqueued(c TaskComment, op ytwrite.Op, enqueued bool) {
	if !enqueued {
		return
	}
	log.Printf("ytwrite.comment_enqueued op_id=%d task_id=%d comment_id=%d state=%s reason=%q", op.ID, c.TaskID, c.ID, op.State, op.Reason)
}

// YTCommentCanary is one task in the canary set with the issue it was bound
// to at enrollment: the tenant and the stable internal issue id (not the
// readable id, which changes when an issue moves project).
type YTCommentCanary struct {
	TaskID        int64     `json:"task_id"`
	Tenant        string    `json:"tenant"`
	IssueID       string    `json:"issue_id"`
	IssueReadable string    `json:"issue_readable"`
	ProjectID     string    `json:"project_id"`
	AddedBy       string    `json:"added_by"`
	AddedAt       time.Time `json:"added_at"`
}

// YTCanaryCandidate is what hk already keeps about a task's YouTrack issue,
// read in one statement for a canary enrollment.
type YTCanaryCandidate struct {
	TaskID int64
	// Project is the task's hk project ("" when it has none); ShortName and
	// ProjectID its exact youtrack_projects row ("" when unmapped: there is no
	// OTHER fallback here).
	Project   string
	ShortName string
	ProjectID string
	// MirrorIssueID and MirrorTenant are the old mirror's task_origins link.
	MirrorIssueID string
	MirrorTenant  string
	// MappedIssueID is the yt_entity_map task row.
	MappedIssueID string
}

// YTCanaryCandidate reads a task's mappings. A missing task is ErrTaskNotFound.
func (s *Store) YTCanaryCandidate(ctx context.Context, taskID int64) (YTCanaryCandidate, error) {
	c := YTCanaryCandidate{TaskID: taskID}
	err := s.pool.QueryRow(ctx, `SELECT coalesce(t.project,''), coalesce(p.short_name,''), coalesce(p.yt_project_id,''),
			coalesce(o.external_id,''), coalesce(o.tenant,''), coalesce(m.yt_id,'')
		FROM tasks t
		LEFT JOIN youtrack_projects p ON p.hk_project = t.project
		LEFT JOIN task_origins o ON o.task_id = t.id AND o.provider = 'youtrack'
		LEFT JOIN yt_entity_map m ON m.entity_kind = 'task' AND m.hk_key = t.id::text
		WHERE t.id = $1`, taskID).Scan(&c.Project, &c.ShortName, &c.ProjectID, &c.MirrorIssueID, &c.MirrorTenant, &c.MappedIssueID)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTCanaryCandidate{}, ErrTaskNotFound
	}
	return c, err
}

// ErrYTCanaryConflict is an enrollment that disagrees with a binding or map
// row already stored.
var ErrYTCanaryConflict = errors.New("yt_canary_conflict")

const ytCanaryColumns = `task_id, tenant, issue_id, issue_readable, project_id, added_by, added_at`

func scanYTCanary(row interface{ Scan(...any) error }, c *YTCommentCanary) error {
	err := row.Scan(&c.TaskID, &c.Tenant, &c.IssueID, &c.IssueReadable, &c.ProjectID, &c.AddedBy, &c.AddedAt)
	c.AddedAt = c.AddedAt.UTC()
	return err
}

// AddYTCommentCanary puts a verified binding in the canary set. Adding the
// same binding again is a no-op; another binding for an enrolled task, an
// issue another task is enrolled with or mapped to, or a task mapped to
// another issue, is ErrYTCanaryConflict. Nothing else is written: an
// enrollment creates no op row.
func (s *Store) AddYTCommentCanary(ctx context.Context, c YTCommentCanary) (YTCommentCanary, error) {
	if c.TaskID < 1 || c.Tenant == "" || len(c.Tenant) > 255 || c.IssueID == "" || len(c.IssueID) > 64 ||
		len(c.IssueReadable) > 64 || len(c.ProjectID) > 64 || c.AddedBy == "" || !validText(c.AddedBy, 200) {
		return YTCommentCanary{}, errors.New("invalid canary binding")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return YTCommentCanary{}, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, c.TaskID).Scan(&exists); err != nil {
		return YTCommentCanary{}, err
	}
	if !exists {
		return YTCommentCanary{}, ErrTaskNotFound
	}
	var mapped bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_entity_map WHERE entity_kind='task'
		AND ((hk_key=$1 AND yt_id<>$2) OR (hk_key<>$1 AND yt_id=$2)))`, strconv.FormatInt(c.TaskID, 10), c.IssueID).Scan(&mapped); err != nil {
		return YTCommentCanary{}, err
	}
	if mapped {
		return YTCommentCanary{}, fmt.Errorf("%w: yt_entity_map maps task %d or issue %s elsewhere", ErrYTCanaryConflict, c.TaskID, c.IssueID)
	}
	var got YTCommentCanary
	err = scanYTCanary(tx.QueryRow(ctx, `INSERT INTO yt_comment_canary(`+ytCanaryColumns+`) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT DO NOTHING RETURNING `+ytCanaryColumns,
		c.TaskID, c.Tenant, c.IssueID, c.IssueReadable, c.ProjectID, c.AddedBy, time.Now().UTC()), &got)
	if errors.Is(err, pgx.ErrNoRows) {
		err = scanYTCanary(tx.QueryRow(ctx, `SELECT `+ytCanaryColumns+` FROM yt_comment_canary WHERE task_id=$1`, c.TaskID), &got)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (got.Tenant != c.Tenant || got.IssueID != c.IssueID)) {
			return YTCommentCanary{}, fmt.Errorf("%w: task %d or issue %s is already enrolled with another binding", ErrYTCanaryConflict, c.TaskID, c.IssueID)
		}
	}
	if err != nil {
		return YTCommentCanary{}, err
	}
	return got, tx.Commit(ctx)
}

// RemoveYTCommentCanary takes a task out of the canary set; removed is false
// when it was not in it. Ops already prepared are not touched: the worker
// holds them (YTCommentAdmitted) until the task is enrolled again.
func (s *Store) RemoveYTCommentCanary(ctx context.Context, taskID int64) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM yt_comment_canary WHERE task_id=$1`, taskID)
	return tag.RowsAffected() == 1, err
}

// ListYTCommentCanary lists the canary set by task id.
func (s *Store) ListYTCommentCanary(ctx context.Context) ([]YTCommentCanary, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+ytCanaryColumns+` FROM yt_comment_canary ORDER BY task_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTCommentCanary{}
	for rows.Next() {
		var c YTCommentCanary
		if err := scanYTCanary(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// YTCommentAdmitted reports whether the worker may send an op for a task to
// this issue on this tenant: the gate is on and the task is still enrolled
// with that binding. Mode off or a removal freezes sends without rejecting.
func (s *Store) YTCommentAdmitted(ctx context.Context, taskID int64, issueID, tenant string) (bool, error) {
	if !s.ytCommentsCanary.Load() {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_comment_canary WHERE task_id=$1 AND issue_id=$2 AND tenant=$3)`, taskID, issueID, tenant).Scan(&ok)
	return ok, err
}

// YTOpFilter selects ops for the operator's view. State is a lifecycle state,
// "pending" (prepared, dispatched or remote_unknown; the default) or "all".
type YTOpFilter struct {
	State   string
	TaskID  int64
	AfterID int64
	Limit   int
}

// YTOpListMax bounds one page of the op view.
const YTOpListMax = 500

// ListYTWriteOps lists ops in id order for the operator's view.
func (s *Store) ListYTWriteOps(ctx context.Context, f YTOpFilter) ([]ytwrite.Op, error) {
	if f.Limit == 0 {
		f.Limit = 100
	}
	if f.Limit < 1 || f.Limit > YTOpListMax || f.TaskID < 0 || f.AfterID < 0 {
		return nil, errors.New("invalid yt op filter")
	}
	var states []string
	switch strings.TrimSpace(f.State) {
	case "", "pending":
		states = []string{string(ytwrite.StatePrepared), string(ytwrite.StateDispatched), string(ytwrite.StateRemoteUnknown)}
	case "all":
		states = []string{}
	default:
		st := ytwrite.State(f.State)
		if !st.Valid() {
			return nil, fmt.Errorf("unknown yt op state %q", f.State)
		}
		states = []string{string(st)}
	}
	task := ""
	if f.TaskID > 0 {
		task = strconv.FormatInt(f.TaskID, 10)
	}
	return s.YTWriteLedger().list(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops
		WHERE (cardinality($1::text[]) = 0 OR state = ANY($1::text[]))
		AND ($2 = '' OR (entity_kind = 'task' AND entity_id = $2))
		AND id > $3 ORDER BY id LIMIT $4`, states, task, f.AfterID, f.Limit)
}
