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

// Slice b1a (hk 3739) and comment mode on (hk 3810),
// docs/youtrack-write-protocol.md: a new hk comment on a task enrolled in the
// canary (mode canary) or bound to its issue (mode on) also gets a
// protocol-tracked copy on the task's YouTrack issue. PG stays canonical for
// comments; the comment row is written exactly as before, and its op, its
// ownership link and (for the first op) the task's map row are written in the
// same transaction. Nothing here calls YouTrack: the issue was verified at
// enrollment or binding, and the worker in serve sends the op
// (ytwrite.Protocol.Drive).

// YTCommentsMode gates the comment copy (HANDOFFKEEP_YT_COMMENTS_MODE).
type YTCommentsMode string

const (
	// YTCommentsOff writes no op row (the default).
	YTCommentsOff YTCommentsMode = "off"
	// YTCommentsCanary prepares one comment_append op for each new comment
	// on a task in yt_comment_canary, in the comment's own transaction.
	YTCommentsCanary YTCommentsMode = "canary"
	// YTCommentsOn prepares one comment_append op for each new comment on a
	// task with a consistent yt_task_bindings row.
	YTCommentsOn YTCommentsMode = "on"
)

// YTCommentPrincipal owns the comment-copy ops; their idempotency key is
// YTCommentKey(comment id), so one persisted comment has at most one op.
const YTCommentPrincipal = "hk:task-comments"

// YTCommentKey is the idempotency key of the op that copies one comment.
func YTCommentKey(commentID int64) string { return "task-comment:" + strconv.FormatInt(commentID, 10) }

// SetYTCommentsMode sets the gate for this process. serve calls it once at
// start, before any request.
func (s *Store) SetYTCommentsMode(m YTCommentsMode) { s.ytCommentsMode.Store(m) }

// ytCommentsModeNow is the gate; unset, and any value that is not canary or
// on, is off.
func (s *Store) ytCommentsModeNow() YTCommentsMode {
	switch m, _ := s.ytCommentsMode.Load().(YTCommentsMode); m {
	case YTCommentsCanary, YTCommentsOn:
		return m
	default:
		return YTCommentsOff
	}
}

// enqueueYTCommentTx stores the op that copies comment c to YouTrack, inside
// the comment's transaction (after its insert, before its commit). It writes
// nothing when the gate is off. Mode on is enqueueYTCommentOnTx; what follows
// is mode canary, which writes nothing when the task is not enrolled. A
// comment PG accepts but the protocol cannot send (a body quoting the op
// marker, an author the provenance line cannot carry) and a task whose map row
// names another issue get a rejected op: the comment is untouched and nothing
// is ever sent. Any database error fails the whole comment.
//
// Locks, in this order after the comment insert lock and the comment's KEY
// SHARE on its task: the op's entity lane lock (lockYTEntity), then new rows
// of yt_write_ops, yt_comment_write_links and yt_entity_map (lockorder.go).
func (s *Store) enqueueYTCommentTx(ctx context.Context, tx pgx.Tx, c TaskComment) (ytwrite.Op, bool, error) {
	switch s.ytCommentsModeNow() {
	case YTCommentsOn:
		return enqueueYTCommentOnTx(ctx, tx, c)
	case YTCommentsCanary:
	default:
		return ytwrite.Op{}, false, nil
	}
	var issueID, projectID, tenant string
	err := tx.QueryRow(ctx, `SELECT issue_id, project_id, tenant FROM yt_comment_canary WHERE task_id=$1`, c.TaskID).Scan(&issueID, &projectID, &tenant)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytwrite.Op{}, false, nil
	}
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	// The op is bound to the tenant the task is enrolled on now; a later
	// re-enrollment elsewhere never retargets it.
	req := ytwrite.CommentRequest{Principal: YTCommentPrincipal, Key: YTCommentKey(c.ID), Actor: c.Author, TaskID: c.TaskID, YTIssueID: issueID, Tenant: tenant, Body: c.Body}
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

// Projection statuses a comment answer reports.
const (
	// YTProjectionNone: no op (the mode is off or the task is not enrolled).
	YTProjectionNone = "none"
)

// YTProjection is what a comment answer says about the comment's YouTrack
// copy: the op that will make it and where it stands. Status is "none" (no
// op), or the op's ytwrite.Status: "pending" (prepared, dispatched or
// remote_unknown), "rejected" (Reason is the op's stable reason code, never
// its free-form error) or, on an MCP replay of a comment already copied,
// "applied". A fresh comment is never reported applied: the local commit is
// not a YouTrack write.
type YTProjection struct {
	Status string `json:"status"`
	OpID   int64  `json:"op_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ytProjectionOf is the projection of op (enqueued false: no op).
func ytProjectionOf(op ytwrite.Op, enqueued bool) YTProjection {
	if !enqueued {
		return YTProjection{Status: YTProjectionNone}
	}
	p := YTProjection{Status: string(ytwrite.StatusOf(op.State)), OpID: op.ID}
	if op.State == ytwrite.StateRejected {
		p.Reason = op.Reason
	}
	return p
}

// ytCommentProjectionTx reads the projection of an existing comment (an MCP
// replay).
func ytCommentProjectionTx(ctx context.Context, tx pgx.Tx, commentID int64) (YTProjection, error) {
	op, err := scanYTOp(tx.QueryRow(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops
		WHERE id = (SELECT op_id FROM yt_comment_write_links WHERE comment_id=$1)`, commentID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ytProjectionOf(ytwrite.Op{}, false), nil
	}
	if err != nil {
		return YTProjection{}, err
	}
	return ytProjectionOf(op, true), nil
}

// CreatedTaskComment is the answer to a comment create (REST, CLI): the
// comment with its keys unchanged, plus its YouTrack projection.
type CreatedTaskComment struct {
	TaskComment
	YouTrackProjection *YTProjection `json:"youtrack_projection,omitempty"`
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
	return readYTCanaryCandidate(ctx, s.pool, taskID, "")
}

type ytRowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readYTCanaryCandidate reads the mappings VerifyCanary checks. lock is ""
// or " FOR SHARE", applied to the youtrack_projects and task_origins rows.
func readYTCanaryCandidate(ctx context.Context, q ytRowQuerier, taskID int64, lock string) (YTCanaryCandidate, error) {
	c := YTCanaryCandidate{TaskID: taskID}
	optional := func(err error) error {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	err := q.QueryRow(ctx, `SELECT coalesce(project,'') FROM tasks WHERE id=$1`, taskID).Scan(&c.Project)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTCanaryCandidate{}, ErrTaskNotFound
	}
	if err != nil {
		return YTCanaryCandidate{}, err
	}
	if c.Project != "" {
		err := q.QueryRow(ctx, `SELECT coalesce(short_name,''), coalesce(yt_project_id,'') FROM youtrack_projects WHERE hk_project=$1`+lock,
			c.Project).Scan(&c.ShortName, &c.ProjectID)
		if err := optional(err); err != nil {
			return YTCanaryCandidate{}, err
		}
	}
	err = q.QueryRow(ctx, `SELECT coalesce(external_id,''), coalesce(tenant,'') FROM task_origins WHERE task_id=$1 AND provider='youtrack'`+lock,
		taskID).Scan(&c.MirrorIssueID, &c.MirrorTenant)
	if err := optional(err); err != nil {
		return YTCanaryCandidate{}, err
	}
	err = q.QueryRow(ctx, `SELECT yt_id FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$1`, strconv.FormatInt(taskID, 10)).Scan(&c.MappedIssueID)
	if err := optional(err); err != nil {
		return YTCanaryCandidate{}, err
	}
	return c, nil
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

// AddYTCommentCanary puts a verified binding in the canary set. verified is
// the candidate the binding was verified against (VerifyCanary): the
// transaction reads the task's mappings again and refuses with
// ErrYTCanaryConflict if any of them changed since. Adding the same binding
// again is a no-op; another binding for an enrolled task, an issue another
// task is enrolled with or mapped to, a task mapped to another issue, and a
// yt_task_bindings row that binds the task elsewhere or the issue to another
// task (mode on, hk 3810), is ErrYTCanaryConflict. Nothing else is written: an
// enrollment creates no op row.
//
// Locks, as AddYTTaskBinding takes them (lockorder.go): the issue ownership
// lock, FOR UPDATE on the task row (a project change or a first mirror link
// waits), the task's lane lock (the one Claim and the comment enqueue take,
// so the yt_entity_map row of the task and the claim's admission cannot
// change under it), then FOR SHARE on the task's youtrack_projects and
// task_origins rows, then the new canary row.
func (s *Store) AddYTCommentCanary(ctx context.Context, c YTCommentCanary, verified YTCanaryCandidate) (YTCommentCanary, error) {
	if c.TaskID < 1 || c.Tenant == "" || len(c.Tenant) > 255 || c.IssueID == "" || len(c.IssueID) > 64 ||
		len(c.IssueReadable) > 64 || len(c.ProjectID) > 64 || c.AddedBy == "" || !validText(c.AddedBy, 200) || verified.TaskID != c.TaskID {
		return YTCommentCanary{}, errors.New("invalid canary binding")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return YTCommentCanary{}, err
	}
	defer tx.Rollback(ctx)
	taskKey := strconv.FormatInt(c.TaskID, 10)
	if err := lockYTIssueOwnershipTx(ctx, tx, c.TaskID); err != nil {
		return YTCommentCanary{}, err
	}
	if err := lockYTEntity(ctx, tx, "task", taskKey); err != nil {
		return YTCommentCanary{}, err
	}
	current, err := readYTCanaryCandidate(ctx, tx, c.TaskID, " FOR SHARE")
	if err != nil {
		return YTCommentCanary{}, err
	}
	if current != verified {
		return YTCommentCanary{}, fmt.Errorf("%w: task %d's YouTrack mappings changed after verification (verified %+v, now %+v); verify again",
			ErrYTCanaryConflict, c.TaskID, verified, current)
	}
	var mapped bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_entity_map WHERE entity_kind='task'
		AND ((hk_key=$1 AND yt_id<>$2) OR (hk_key<>$1 AND yt_id=$2)))`, taskKey, c.IssueID).Scan(&mapped); err != nil {
		return YTCommentCanary{}, err
	}
	if mapped {
		return YTCommentCanary{}, fmt.Errorf("%w: yt_entity_map maps task %d or issue %s elsewhere", ErrYTCanaryConflict, c.TaskID, c.IssueID)
	}
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_task_bindings
		WHERE (task_id=$1 AND (tenant<>$2 OR issue_id<>$3)) OR (task_id<>$1 AND tenant=$2 AND issue_id=$3))`, c.TaskID, c.Tenant, c.IssueID).Scan(&bound); err != nil {
		return YTCommentCanary{}, err
	}
	if bound {
		return YTCommentCanary{}, fmt.Errorf("%w: yt_task_bindings binds task %d or issue %s elsewhere", ErrYTCanaryConflict, c.TaskID, c.IssueID)
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
// when it was not in it. Ops already prepared are not touched: the worker's
// claim holds them (YTCommentLedger) until the task is enrolled again on the
// op's tenant. It takes the task's lane lock first, the lock a claim holds
// while it reads the admission: a removal that commits first holds every
// later claim, and one that waits behind a claim follows that claim.
func (s *Store) RemoveYTCommentCanary(ctx context.Context, taskID int64) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(taskID, 10)); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM yt_comment_canary WHERE task_id=$1`, taskID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, tx.Commit(ctx)
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

// YTCommentConfig wires cfg for the comment worker of one YouTrack tenant:
// its ledger is YTCommentLedger(tenant), whose claim admits a dispatch in the
// claim's own transaction. Mode off, a removal from the canary (mode canary),
// a task with no matching binding (mode on), or an op bound to another tenant
// freezes sends without rejecting anything.
func (s *Store) YTCommentConfig(tenant string, cfg ytwrite.Config) ytwrite.Config {
	cfg.Ledger = s.YTCommentLedger(tenant)
	return cfg
}

// YTCommentEchoOp reports whether op opID is a comment_append op hk wrote (or
// is writing) to the YouTrack issue issueID: the ownership evidence the
// activity pull needs before it drops a comment as hk's own echo.
func (s *Store) YTCommentEchoOp(ctx context.Context, opID int64, issueID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_write_ops o JOIN yt_comment_write_links w ON w.op_id = o.id
		WHERE o.id=$1 AND o.kind=$2 AND o.yt_target_id=$3)`, opID, ytwrite.KindCommentAppend, issueID).Scan(&ok)
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
