package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
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

// ytCommentKeyPrefix is YTCommentKey without the comment id.
const ytCommentKeyPrefix = "task-comment:"

// YTCommentKey is the idempotency key of the op that copies one comment.
func YTCommentKey(commentID int64) string {
	return ytCommentKeyPrefix + strconv.FormatInt(commentID, 10)
}

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

func init() { registerYTKindGate(ytwrite.KindCommentAppend, ytCommentGateSQL) }

// ytCommentGateSQL is the comment_append gate (ytAdmitSQL): either the copy
// of an hk comment (ytCommentModeGateSQL, which never admits a document
// copy) or a document copy (ytDocCommentGateSQL).
func ytCommentGateSQL(s *Store) string {
	return `(` + ytCommentModeGateSQL(s) + `) OR (` + ytDocCommentGateSQL(s) + `)`
}

// ytCommentModeGateSQL admits o as an hk comment's copy: o's task is enrolled
// in the canary (mode canary) or bound (mode on, yt_task_bindings) with o's
// issue on o's tenant. Mode off admits nothing.
func ytCommentModeGateSQL(s *Store) string {
	switch s.ytCommentsModeNow() {
	case YTCommentsCanary:
		return `o.principal <> '` + ytDocCommentPrincipal + `' AND EXISTS (SELECT 1 FROM yt_comment_canary c
		WHERE o.entity_kind = 'task' AND c.task_id::text = o.entity_id AND c.issue_id = o.yt_target_id AND c.tenant = o.tenant)`
	case YTCommentsOn:
		// An op with no target is a comment queued behind its task's
		// issue_create (hk 4263): lane order holds it until the create
		// resolves, and its claim fills the target from the create's binding
		// (claimYTCommentTargetTx) or leaves it empty, which Send refuses.
		return `o.principal <> '` + ytDocCommentPrincipal + `' AND o.entity_kind = 'task' AND (EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)
		OR (o.principal = '` + YTCommentPrincipal + `' AND o.yt_target_id = ''))`
	default:
		return `FALSE`
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
//
// While comments are canonical in YouTrack (hk 4122), a comment that would
// end up only in PG is refused with ErrYTCommentUnsendable and the caller's
// transaction rolls back: a comment the protocol can never send, and a
// comment that gets no op or a born-rejected one (the mode is not on or
// canary, the task is not bound or enrolled, a source disagrees). One
// exception (hk 4263): in mode on, a task with no binding whose issue_create
// op is still pending gets a queued op behind that create
// (queueYTCommentTx). FOR SHARE on the comments row orders the comment
// against a switch (SetYTAuthority).
func (s *Store) enqueueYTCommentTx(ctx context.Context, tx pgx.Tx, c TaskComment) (ytwrite.Op, bool, error) {
	side, err := ytAuthorityShareTx(ctx, tx, YTAuthorityComments)
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	if side != YTAuthorityYouTrack {
		return s.enqueueYTCommentModeTx(ctx, tx, c)
	}
	if cerr := ytwrite.CheckCommentContent(c.Author, c.Body); cerr != nil {
		return ytwrite.Op{}, false, fmt.Errorf("%w: %v", ErrYTCommentUnsendable, cerr)
	}
	op, enqueued, err := s.enqueueYTCommentModeTx(ctx, tx, c)
	if err == nil && !enqueued && s.ytCommentsModeNow() == YTCommentsOn {
		op, enqueued, err = queueYTCommentTx(ctx, tx, c)
	}
	switch {
	case err != nil:
		return op, enqueued, err
	case !enqueued:
		return ytwrite.Op{}, false, fmt.Errorf("%w: comments mode %s has no YouTrack copy for task %d (not bound or enrolled, or a mapping disagrees)",
			ErrYTCommentUnsendable, s.ytCommentsModeNow(), c.TaskID)
	case op.State == ytwrite.StateRejected:
		return ytwrite.Op{}, false, fmt.Errorf("%w: its YouTrack copy would be rejected (%s)", ErrYTCommentUnsendable, op.Reason)
	}
	return op, enqueued, nil
}

// queueYTCommentTx is the comment copy of a task that has no binding yet
// because its issue is still being created (hk 4263): a task created a moment
// ago gets its binding only when its issue_create op applies. When the task
// has no binding and its issue_create op (YTIssuePrincipal, IssueKey) is
// prepared, dispatched or remote_unknown, it prepares the comment's
// comment_append op with the usual key and payload, on the create's tenant,
// with no target, and links it to the comment. The op is on the task's lane
// behind the create, so it is never claimed before the create resolves; its
// claim takes the target from the binding the create published
// (claimYTCommentTargetTx). A create that ends rejected, or applied without a
// binding (binding_conflict), leaves the target empty and the op is rejected
// at its send (ytwrite.ReasonIssueNotBound), which the rejected-op alert
// reports. No map anchor is written: the create's apply writes the task's map
// row. A task with a binding (a source disagreed) or with no pending create
// gets nothing (enqueued false).
//
// Only enqueueYTCommentTx calls it, under comments authority youtrack in mode
// on; under hk such a comment commits with no op and the catch-up copies it
// once the binding exists.
//
// Locks: the task's lane lock (already held from ytCommentOnOpTx; advisory
// locks are re-entrant), then plain reads of the binding and the create op,
// then new rows of yt_write_ops and yt_comment_write_links (lockorder.go).
// Apply takes the same lane lock before it publishes the binding, so either
// the binding is seen here or the create is still pending.
func queueYTCommentTx(ctx context.Context, tx pgx.Tx, c TaskComment) (ytwrite.Op, bool, error) {
	taskKey := strconv.FormatInt(c.TaskID, 10)
	if err := lockYTEntity(ctx, tx, "task", taskKey); err != nil {
		return ytwrite.Op{}, false, err
	}
	if _, found, err := readYTBinding(ctx, tx, c.TaskID); err != nil || found {
		return ytwrite.Op{}, false, err
	}
	var createID int64
	var tenant string
	err := tx.QueryRow(ctx, `SELECT id, tenant FROM yt_write_ops WHERE principal=$1 AND kind=$2 AND idem_key=$3
		AND entity_kind='task' AND entity_id=$4 AND state IN ('prepared','dispatched','remote_unknown')`,
		YTIssuePrincipal, ytwrite.KindIssueCreate, ytwrite.IssueKey(c.TaskID), taskKey).Scan(&createID, &tenant)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && tenant == "") {
		return ytwrite.Op{}, false, nil
	}
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	n, err := ytwrite.CommentOp(ytwrite.CommentRequest{Principal: YTCommentPrincipal, Key: YTCommentKey(c.ID), Actor: c.Author, TaskID: c.TaskID, Tenant: tenant, Body: c.Body})
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
	log.Printf("ytwrite.comment_queued op_id=%d task_id=%d comment_id=%d issue_op_id=%d", op.ID, c.TaskID, c.ID, createID)
	return op, true, nil
}

// claimYTCommentTargetTx is the target of a queued comment op (no target,
// queueYTCommentTx) at its claim, in Claim's transaction under the task's
// lane lock: the issue of the task's binding published by the task's own
// issue_create op (origin op_applied, that op applied) on the op's tenant,
// and the payload hash that covers it. A task with no such binding gives ""
// and the op's own hash: the op is dispatched with no target and Send refuses
// it. Any other op is returned unchanged.
func claimYTCommentTargetTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op) (target, sha string, err error) {
	if op.Kind != ytwrite.KindCommentAppend || op.Principal != YTCommentPrincipal || op.EntityKind != "task" || op.YTTargetID != "" {
		return op.YTTargetID, op.PayloadSHA, nil
	}
	taskID, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil || taskID < 1 {
		return op.YTTargetID, op.PayloadSHA, nil
	}
	var issueID string
	err = tx.QueryRow(ctx, `SELECT b.issue_id FROM yt_task_bindings b JOIN yt_write_ops i ON i.id = b.op_id
		WHERE b.task_id=$1 AND b.tenant=$2 AND b.origin=$3
		AND i.principal=$4 AND i.kind=$5 AND i.idem_key=$6 AND i.state='applied'`,
		taskID, op.Tenant, YTBindingOpApplied, YTIssuePrincipal, ytwrite.KindIssueCreate, ytwrite.IssueKey(taskID)).Scan(&issueID)
	if errors.Is(err, pgx.ErrNoRows) {
		log.Printf("ytwrite.comment_queued_unbound op_id=%d task_id=%d", op.ID, taskID)
		return op.YTTargetID, op.PayloadSHA, nil
	}
	if err != nil {
		return "", "", err
	}
	var p ytwrite.CommentPayload
	if err := json.Unmarshal(op.Payload, &p); err != nil {
		return "", "", fmt.Errorf("payload of op %d: %w", op.ID, err)
	}
	n, err := ytwrite.CommentOp(ytwrite.CommentRequest{Principal: op.Principal, Key: op.Key, Actor: op.Actor, TaskID: taskID, YTIssueID: issueID, Tenant: op.Tenant, Body: p.Body})
	if err != nil {
		return "", "", err
	}
	log.Printf("ytwrite.comment_queued_targeted op_id=%d task_id=%d issue_id=%q", op.ID, taskID, issueID)
	return n.YTTargetID, n.PayloadSHA, nil
}

// enqueueYTCommentIfCanonicalTx is the YouTrack copy of a comment whose writer
// prepares no op while hk is canonical (ReplyDots): nothing under hk, today's
// behaviour; under youtrack, enqueueYTCommentTx, which refuses a comment that
// would stay only in PG.
func (s *Store) enqueueYTCommentIfCanonicalTx(ctx context.Context, tx pgx.Tx, c TaskComment) (ytwrite.Op, bool, error) {
	side, err := ytAuthorityShareTx(ctx, tx, YTAuthorityComments)
	if err != nil || side != YTAuthorityYouTrack {
		return ytwrite.Op{}, false, err
	}
	return s.enqueueYTCommentTx(ctx, tx, c)
}

// enqueueYTCommentModeTx is enqueueYTCommentTx by the comments mode alone.
func (s *Store) enqueueYTCommentModeTx(ctx context.Context, tx pgx.Tx, c TaskComment) (ytwrite.Op, bool, error) {
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
	pending, err := ytIssueOpPendingTx(ctx, tx, c.TaskID)
	if err != nil {
		return YTCommentCanary{}, err
	}
	if pending {
		return YTCommentCanary{}, fmt.Errorf("%w: task %d has an unresolved issue_create op (hk 3898)", ErrYTCanaryConflict, c.TaskID)
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
// claim holds them (YTTenantLedger) until the task is enrolled again on the
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
// its ledger is YTTenantLedger(tenant), whose claim admits a dispatch in the
// claim's own transaction. Mode off, a removal from the canary (mode canary),
// a task with no matching binding (mode on), or an op bound to another tenant
// freezes sends without rejecting anything.
func (s *Store) YTCommentConfig(tenant string, cfg ytwrite.Config) ytwrite.Config {
	cfg.Ledger = s.YTTenantLedger(tenant)
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
// "rejected" also lists applied ops whose binding was not published
// (reason binding_conflict).
type YTOpFilter struct {
	State   string
	Kind    string
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
	if f.Kind != "" && !slices.Contains(ytwrite.RegisteredKinds(), f.Kind) {
		return nil, fmt.Errorf("%w: unknown yt op kind %q", ytwrite.ErrInvalid, f.Kind)
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
	// An applied issue_create op whose binding was not published is listed
	// with the rejected ops: the task has an issue hk does not use.
	conflicts := len(states) == 1 && states[0] == string(ytwrite.StateRejected)
	return s.YTWriteLedger().list(ctx, `SELECT `+ytOpColumns+` FROM yt_write_ops
		WHERE (cardinality($1::text[]) = 0 OR state = ANY($1::text[]) OR ($5 AND state = 'applied' AND reason = '`+ytwrite.ReasonBindingConflict+`'))
		AND ($2 = '' OR (entity_kind = 'task' AND entity_id = $2))
		AND ($6 = '' OR kind = $6)
		AND id > $3 ORDER BY id LIMIT $4`, states, task, f.AfterID, f.Limit, conflicts, f.Kind)
}
