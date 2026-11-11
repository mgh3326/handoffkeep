package store

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// Comment mode on (hk 3810, docs/youtrack-write-protocol.md). A task's
// yt_task_bindings row is the one authority for where mode on copies its
// comments. `youtrack comments-backfill` writes it after the canary's
// verification (youtrack.VerifyBinding); every other mapping hk keeps for the
// task or its issue must agree with it, both when it is published and when a
// comment is enqueued, or the comment gets no op and stays with the legacy
// push and the import.

// YTTaskBinding is a task's verified YouTrack issue: the tenant and the
// stable internal issue id (not the readable id, which changes when an issue
// moves project), and the YouTrack project it was verified in.
type YTTaskBinding struct {
	TaskID        int64     `json:"task_id"`
	Tenant        string    `json:"tenant"`
	IssueID       string    `json:"issue_id"`
	IssueReadable string    `json:"issue_readable"`
	ProjectID     string    `json:"project_id"`
	BoundBy       string    `json:"bound_by"`
	BoundAt       time.Time `json:"bound_at"`
	// Origin is how the binding was published: YTBindingImportVerified
	// (`youtrack comments-backfill`) or YTBindingOpApplied (an applied
	// issue_create op, OpID). Set by the store, never by a caller.
	Origin string `json:"origin"`
	OpID   int64  `json:"op_id,omitempty"`
}

// The origins of a binding (yt_task_bindings.origin).
const (
	YTBindingImportVerified = "import_verified"
	YTBindingOpApplied      = "op_applied"
)

// ErrYTBindingConflict is a binding that disagrees with a mapping hk already
// keeps, or one verified against mappings that changed since.
var ErrYTBindingConflict = errors.New("yt_binding_conflict")

// ytIssueOwnershipLock serialises the writers that give a YouTrack issue to a
// task: AddYTTaskBinding and AddYTCommentCanary. Each checks the other's
// table, and neither table's unique index spans both.
const ytIssueOwnershipLock = 8241873810

// lockYTIssueOwnershipTx takes the issue ownership lock and then FOR UPDATE on
// the task row, so neither the task's project nor its first task_origins row
// can change before commit. FOR SHARE would not do: a first origin insert
// (UpsertYouTrackMirror, AddTaskOrigin) reaches the task row only through its
// foreign key's KEY SHARE, which FOR SHARE allows and FOR UPDATE blocks, and
// with no origin row there is nothing else to lock. A missing task is
// ErrTaskNotFound.
func lockYTIssueOwnershipTx(ctx context.Context, tx pgx.Tx, taskID int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, ytIssueOwnershipLock); err != nil {
		return err
	}
	var id int64
	err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, taskID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrTaskNotFound
	}
	return err
}

// The sources a binding is checked against, in the order they are named.
const (
	ytSourceOrigins      = "task_origins"            // the task's mirror link names another issue, or another non-empty tenant
	ytSourceMapTask      = "yt_entity_map_task"      // the task's map row names another issue
	ytSourceMapIssue     = "yt_entity_map_issue"     // a map row gives the issue to another task
	ytSourceCanaryTask   = "yt_comment_canary_task"  // the task's canary row names another issue or tenant
	ytSourceCanaryIssue  = "yt_comment_canary_issue" // a canary row gives the tenant's issue to another task
	ytSourceBindingIssue = "yt_task_bindings_issue"  // another task's binding holds the tenant's issue
	ytSourceProject      = "youtrack_projects"       // the task has no project, or it maps to another YouTrack project
)

// ytBindingSourcesSQL reads, in one statement, which sources disagree with
// the binding ($1 task id, $2 tenant, $3 issue id, $4 task id as text, $5
// YouTrack project id). Plain reads: the caller decides the locks.
const ytBindingSourcesSQL = `SELECT
	EXISTS(SELECT 1 FROM task_origins WHERE task_id=$1 AND provider='youtrack'
		AND (coalesce(external_id,'') <> $3 OR (coalesce(tenant,'') <> '' AND tenant <> $2))),
	EXISTS(SELECT 1 FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$4 AND yt_id <> $3),
	EXISTS(SELECT 1 FROM yt_entity_map WHERE entity_kind='task' AND yt_id=$3 AND hk_key <> $4),
	EXISTS(SELECT 1 FROM yt_comment_canary WHERE task_id=$1 AND (tenant <> $2 OR issue_id <> $3)),
	EXISTS(SELECT 1 FROM yt_comment_canary WHERE tenant=$2 AND issue_id=$3 AND task_id <> $1),
	EXISTS(SELECT 1 FROM yt_task_bindings WHERE tenant=$2 AND issue_id=$3 AND task_id <> $1),
	NOT EXISTS(SELECT 1 FROM tasks t JOIN youtrack_projects p ON p.hk_project = t.project WHERE t.id=$1 AND p.yt_project_id=$5)`

// ytBindingDisagreement names the first local source that disagrees with
// binding b, or "" when every source present agrees: the task's mirror link,
// its map row and canary row name b's issue (and tenant), no map row, canary
// row or other binding gives that issue to another task, and the task's
// project maps to b's YouTrack project. It makes no network call. Enqueue,
// the binding's publication and the preflight share it.
func ytBindingDisagreement(ctx context.Context, q ytRowQuerier, b YTTaskBinding) (string, error) {
	var flags [7]bool
	if err := q.QueryRow(ctx, ytBindingSourcesSQL, b.TaskID, b.Tenant, b.IssueID, strconv.FormatInt(b.TaskID, 10), b.ProjectID).Scan(
		&flags[0], &flags[1], &flags[2], &flags[3], &flags[4], &flags[5], &flags[6]); err != nil {
		return "", err
	}
	for i, name := range []string{ytSourceOrigins, ytSourceMapTask, ytSourceMapIssue, ytSourceCanaryTask, ytSourceCanaryIssue, ytSourceBindingIssue, ytSourceProject} {
		if flags[i] {
			return name, nil
		}
	}
	return "", nil
}

// YTBindingDisagreement is ytBindingDisagreement on the pool: read-only, for
// the preflight, against a stored binding or one about to be published.
func (s *Store) YTBindingDisagreement(ctx context.Context, b YTTaskBinding) (string, error) {
	return ytBindingDisagreement(ctx, s.pool, b)
}

const ytBindingColumns = `task_id, tenant, issue_id, issue_readable, project_id, bound_by, bound_at, origin, coalesce(op_id, 0)`

func scanYTBinding(row interface{ Scan(...any) error }, b *YTTaskBinding) error {
	err := row.Scan(&b.TaskID, &b.Tenant, &b.IssueID, &b.IssueReadable, &b.ProjectID, &b.BoundBy, &b.BoundAt, &b.Origin, &b.OpID)
	b.BoundAt = b.BoundAt.UTC()
	return err
}

func readYTBinding(ctx context.Context, q ytRowQuerier, taskID int64) (YTTaskBinding, bool, error) {
	var b YTTaskBinding
	err := scanYTBinding(q.QueryRow(ctx, `SELECT `+ytBindingColumns+` FROM yt_task_bindings WHERE task_id=$1`, taskID), &b)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTTaskBinding{}, false, nil
	}
	if err != nil {
		return YTTaskBinding{}, false, err
	}
	return b, true, nil
}

// YTTaskBinding reads a task's binding; found is false when it has none.
func (s *Store) YTTaskBinding(ctx context.Context, taskID int64) (YTTaskBinding, bool, error) {
	return readYTBinding(ctx, s.pool, taskID)
}

// enqueueYTCommentOnTx is enqueueYTCommentTx in mode on. A task with no
// binding gets no op and no link, and so does a task whose binding any local
// source disagrees with (ytBindingDisagreement; one log event names the
// source): the legacy push and the import carry such a comment as before.
// Otherwise the op targets the binding's issue on its tenant, its link is
// written, and the first op anchors the task's yt_entity_map row as in the
// canary. A comment the protocol cannot send gets a rejected op and its link,
// as in the canary; an identity problem never does.
//
// Locks, after the comment insert lock and the comment's KEY SHARE on its
// task: the task's lane lock, then plain reads of the binding and the sources,
// then new rows of yt_write_ops, yt_comment_write_links and yt_entity_map
// (lockorder.go). The lane lock is the one AddYTTaskBinding takes before it
// reads and inserts, so a binding is either seen whole or not at all.
func enqueueYTCommentOnTx(ctx context.Context, tx pgx.Tx, c TaskComment) (ytwrite.Op, bool, error) {
	taskKey := strconv.FormatInt(c.TaskID, 10)
	if err := lockYTEntity(ctx, tx, "task", taskKey); err != nil {
		return ytwrite.Op{}, false, err
	}
	b, found, err := readYTBinding(ctx, tx, c.TaskID)
	if err != nil || !found {
		return ytwrite.Op{}, false, err
	}
	source, err := ytBindingDisagreement(ctx, tx, b)
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	if source != "" {
		log.Printf("ytwrite.comment_skipped_ambiguous task_id=%d comment_id=%d source=%s", c.TaskID, c.ID, source)
		return ytwrite.Op{}, false, nil
	}
	req := ytwrite.CommentRequest{Principal: YTCommentPrincipal, Key: YTCommentKey(c.ID), Actor: c.Author, TaskID: c.TaskID, YTIssueID: b.IssueID, Tenant: b.Tenant, Body: c.Body}
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
	if reason != "" {
		op, err = rejectYTOpTx(ctx, tx, now, op.ID, reason, detail)
		if err != nil {
			return ytwrite.Op{}, false, err
		}
		return op, true, nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind, hk_key, yt_id, yt_parent_id, op_id, created_at)
		VALUES ('task',$1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, taskKey, b.IssueID, b.ProjectID, op.ID, now)
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	if tag.RowsAffected() == 0 {
		// The sources were read under the lane lock and agreed, and every
		// writer of the task's map row takes that lock, so only an anchor
		// already naming this issue can be here. Anything else is a broken
		// invariant: fail the comment rather than link it to an op that
		// cannot be sent.
		var have string
		err := tx.QueryRow(ctx, `SELECT yt_id FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$1`, taskKey).Scan(&have)
		if err != nil || have != b.IssueID {
			return ytwrite.Op{}, false, fmt.Errorf("yt_entity_map conflicts with task %d's checked binding to %s (have %q): %v", c.TaskID, b.IssueID, have, err)
		}
	}
	return op, true, nil
}

// AddYTTaskBinding publishes a verified binding. verified is the candidate
// the binding was verified against (youtrack.VerifyBinding): the transaction
// reads the task's mappings again and refuses with ErrYTBindingConflict if
// any of them changed since, then refuses if any local source disagrees with
// the binding (ytBindingDisagreement) or another binding holds the task.
// Adding the same binding again is a no-op that returns the stored row.
// Nothing else is written: a binding creates no op row and no map row.
//
// Locks, in this order (lockorder.go): the issue ownership lock, FOR UPDATE on
// the task row, the task's lane lock, FOR SHARE on the task's youtrack_projects
// and task_origins rows, then the new binding row.
func (s *Store) AddYTTaskBinding(ctx context.Context, b YTTaskBinding, verified YTCanaryCandidate) (YTTaskBinding, error) {
	if b.TaskID < 1 || b.Tenant == "" || len(b.Tenant) > 255 || b.IssueID == "" || len(b.IssueID) > 64 ||
		len(b.IssueReadable) > 64 || b.ProjectID == "" || len(b.ProjectID) > 64 || b.BoundBy == "" || !validText(b.BoundBy, 200) || verified.TaskID != b.TaskID {
		return YTTaskBinding{}, errors.New("invalid task binding")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return YTTaskBinding{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockYTIssueOwnershipTx(ctx, tx, b.TaskID); err != nil {
		return YTTaskBinding{}, err
	}
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(b.TaskID, 10)); err != nil {
		return YTTaskBinding{}, err
	}
	current, err := readYTCanaryCandidate(ctx, tx, b.TaskID, " FOR SHARE")
	if err != nil {
		return YTTaskBinding{}, err
	}
	if current != verified {
		return YTTaskBinding{}, fmt.Errorf("%w: task %d's YouTrack mappings changed after verification (verified %+v, now %+v); verify again",
			ErrYTBindingConflict, b.TaskID, verified, current)
	}
	have, found, err := readYTBinding(ctx, tx, b.TaskID)
	if err != nil {
		return YTTaskBinding{}, err
	}
	if found {
		if have.Tenant != b.Tenant || have.IssueID != b.IssueID {
			return YTTaskBinding{}, fmt.Errorf("%w: task %d is bound to issue %s on %s", ErrYTBindingConflict, b.TaskID, have.IssueID, have.Tenant)
		}
		return have, tx.Commit(ctx)
	}
	source, err := ytBindingDisagreement(ctx, tx, b)
	if err != nil {
		return YTTaskBinding{}, err
	}
	if source != "" {
		return YTTaskBinding{}, fmt.Errorf("%w: %s disagrees with binding task %d to issue %s on %s", ErrYTBindingConflict, source, b.TaskID, b.IssueID, b.Tenant)
	}
	if s.ytBindingChecked != nil {
		s.ytBindingChecked()
	}
	var got YTTaskBinding
	err = scanYTBinding(tx.QueryRow(ctx, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, issue_readable, project_id, bound_by, bound_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING RETURNING `+ytBindingColumns,
		b.TaskID, b.Tenant, b.IssueID, b.IssueReadable, b.ProjectID, b.BoundBy, time.Now().UTC()), &got)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTTaskBinding{}, fmt.Errorf("%w: issue %s on %s is bound to another task", ErrYTBindingConflict, b.IssueID, b.Tenant)
	}
	if err != nil {
		return YTTaskBinding{}, err
	}
	return got, tx.Commit(ctx)
}

// YTCommentCandidate is a task the comments preflight and backfill classify.
type YTCommentCandidate struct {
	TaskID  int64
	Project string
}

// YTCommentCandidates lists, in id order, the tasks with a comment created at
// or after since, every task in yt_comment_canary, and every task with a
// comment op not yet applied or rejected; project, when not "", keeps only
// that hk project's tasks.
func (s *Store) YTCommentCandidates(ctx context.Context, since time.Time, project string) ([]YTCommentCandidate, error) {
	rows, err := s.pool.Query(ctx, `WITH ids AS (
			SELECT task_id FROM task_comments WHERE created_at >= $1
			UNION SELECT task_id FROM yt_comment_canary
			UNION SELECT CASE WHEN entity_id ~ '^[0-9]{1,18}$' THEN entity_id::bigint END FROM yt_write_ops
				WHERE principal=$3 AND kind=$4 AND entity_kind='task' AND state NOT IN ('applied','rejected'))
		SELECT t.id, coalesce(t.project,'') FROM tasks t JOIN ids ON ids.task_id = t.id
		WHERE $2 = '' OR t.project = $2 ORDER BY t.id`, since.UTC(), project, YTCommentPrincipal, ytwrite.KindCommentAppend)
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

// YTCommentVolume counts the comments created at or after since that an hk
// entry point wrote (not the ones the YouTrack activity pull wrote, which are
// never copied), in all and in the busiest clock hour (UTC); project, when not
// "", keeps only that hk project's tasks.
func (s *Store) YTCommentVolume(ctx context.Context, since time.Time, project string) (total, maxPerHour int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT coalesce(sum(n),0), coalesce(max(n),0) FROM (
			SELECT count(*) AS n FROM task_comments c JOIN tasks t ON t.id = c.task_id
			WHERE c.created_at >= $1 AND c.author <> $3 AND ($2 = '' OR t.project = $2)
			GROUP BY date_trunc('hour', c.created_at AT TIME ZONE 'UTC')) h`, since.UTC(), project, YouTrackOwnedActor).Scan(&total, &maxPerHour)
	return total, maxPerHour, err
}

// YTUnresolvedCommentOp is a comment op not yet applied or rejected.
type YTUnresolvedCommentOp struct {
	OpID    int64
	TaskID  int64
	Tenant  string
	IssueID string
}

// YTUnresolvedCommentOps lists the comment ops not yet applied or rejected,
// in id order.
func (s *Store) YTUnresolvedCommentOps(ctx context.Context) ([]YTUnresolvedCommentOp, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, entity_id, tenant, yt_target_id FROM yt_write_ops
		WHERE principal=$1 AND kind=$2 AND entity_kind='task' AND state NOT IN ('applied','rejected') ORDER BY id`,
		YTCommentPrincipal, ytwrite.KindCommentAppend)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []YTUnresolvedCommentOp{}
	for rows.Next() {
		var op YTUnresolvedCommentOp
		var entity string
		if err := rows.Scan(&op.OpID, &entity, &op.Tenant, &op.IssueID); err != nil {
			return nil, err
		}
		if op.TaskID, err = strconv.ParseInt(entity, 10, 64); err != nil {
			return nil, fmt.Errorf("comment op %d names task %q: %w", op.OpID, entity, err)
		}
		out = append(out, op)
	}
	return out, rows.Err()
}
