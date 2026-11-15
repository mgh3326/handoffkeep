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
	// CommentsFrom is the highest task_comments id the binding does not
	// cover: the task's highest comment id when the binding was published.
	// Only a comment above it is ever copied (CatchUpYTComments).
	CommentsFrom int64 `json:"comments_from"`
	// Origin is how the binding was published: YTBindingImportVerified
	// (`youtrack comments-backfill`) or YTBindingOpApplied (an applied
	// issue_create op, OpID). Set by the store, never by a caller.
	Origin string `json:"origin"`
	OpID   int64  `json:"op_id,omitempty"`
}

// The origins of a binding (yt_task_bindings.origin).
const (
	// YTBindingImportVerified is a binding `youtrack comments-backfill`
	// published after its verification.
	YTBindingImportVerified = "import_verified"
	// YTBindingOpApplied is a binding an applied issue_create op published.
	YTBindingOpApplied = "op_applied"
	// YTBindingAutoVerified is a binding the automatic binder published (hk
	// 4492, HANDOFFKEEP_YT_BIND): the same verification as the backfill, with
	// the comment watermark computed from the issue's own listing instead of
	// the task's highest comment id.
	YTBindingAutoVerified = "auto_verified"
)

// ErrYTBindingConflict is a binding that disagrees with a mapping hk already
// keeps, or one verified against mappings that changed since.
var ErrYTBindingConflict = errors.New("yt_binding_conflict")

// ErrYTBindingExcluded is an automatic publication refused because the
// operator unbound the task and nothing bound it since (I7): the binder read
// that before it verified the issue, but the unbind may commit while it does
// its YouTrack reads, so the publication rechecks it under the ownership lock.
// Nothing was written.
var ErrYTBindingExcluded = errors.New("yt_binding_excluded")

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
	ytSourceProject      = "youtrack_projects"       // the task's project maps to another YouTrack project or none, or a task with no project is bound to a mapped project
)

// ytBindingSourcesSQL reads, in one statement, which sources disagree with
// the binding ($1 task id, $2 tenant, $3 issue id, $4 task id as text, $5
// YouTrack project id). Plain reads: the caller decides the locks. The map
// rows that name an issue are read on the binding's tenant: the same YouTrack
// id on another tenant is another issue (hk 3912). A row a release that
// predates the tenant column wrote (tenant ”) is read as this tenant's, the
// safe direction for a check that refuses.
const ytBindingSourcesSQL = `SELECT
	EXISTS(SELECT 1 FROM task_origins WHERE task_id=$1 AND provider='youtrack'
		AND (coalesce(external_id,'') <> $3 OR (coalesce(tenant,'') <> '' AND tenant <> $2))),
	EXISTS(SELECT 1 FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$4 AND yt_id <> $3),
	EXISTS(SELECT 1 FROM yt_entity_map WHERE entity_kind='task' AND (tenant=$2 OR tenant='') AND yt_id=$3 AND hk_key <> $4),
	EXISTS(SELECT 1 FROM yt_comment_canary WHERE task_id=$1 AND (tenant <> $2 OR issue_id <> $3)),
	EXISTS(SELECT 1 FROM yt_comment_canary WHERE tenant=$2 AND issue_id=$3 AND task_id <> $1),
	EXISTS(SELECT 1 FROM yt_task_bindings WHERE tenant=$2 AND issue_id=$3 AND task_id <> $1),
	NOT (EXISTS(SELECT 1 FROM tasks t JOIN youtrack_projects p ON p.hk_project = t.project WHERE t.id=$1 AND p.yt_project_id=$5)
		OR (EXISTS(SELECT 1 FROM tasks WHERE id=$1 AND coalesce(project,'') = '')
			AND NOT EXISTS(SELECT 1 FROM youtrack_projects WHERE yt_project_id=$5)))`

// ytBindingDisagreement names the first local source that disagrees with
// binding b, or "" when every source present agrees: the task's mirror link,
// its map row and canary row name b's issue (and tenant), no map row, canary
// row or other binding gives that issue to another task, and the task's
// project maps to b's YouTrack project. A task with no project agrees with a
// YouTrack project that no youtrack_projects row maps: the import's bucket
// for such tasks, short name HK (hk 3897), which has no row of its own and
// which youtrack.VerifyBinding resolves by name before it binds. It makes no
// network call. Enqueue, the catch-up, the binding's publication and the
// preflight share it.
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

const ytBindingColumns = `task_id, tenant, issue_id, issue_readable, project_id, bound_by, bound_at, comments_from, origin, coalesce(op_id, 0)`

func scanYTBinding(row interface{ Scan(...any) error }, b *YTTaskBinding) error {
	err := row.Scan(&b.TaskID, &b.Tenant, &b.IssueID, &b.IssueReadable, &b.ProjectID, &b.BoundBy, &b.BoundAt, &b.CommentsFrom, &b.Origin, &b.OpID)
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
// Otherwise ytCommentOnOpTx writes the op, its link and the first op's map
// anchor, with the comment's own time.
//
// Locks, after the comment insert lock and the comment's KEY SHARE on its
// task: those of ytCommentOnOpTx (lockorder.go).
func enqueueYTCommentOnTx(ctx context.Context, tx pgx.Tx, c TaskComment) (ytwrite.Op, bool, error) {
	op, enqueued, source, err := ytCommentOnOpTx(ctx, tx, c, c.CreatedAt.UTC())
	if source != "" {
		log.Printf("ytwrite.comment_skipped_ambiguous task_id=%d comment_id=%d source=%s", c.TaskID, c.ID, source)
	}
	return op, enqueued, err
}

// ytCommentOnOpTx is the mode-on copy of comment c, shared by the enqueue
// (in the comment's transaction) and the worker's catch-up (CatchUpYTComments,
// in its own). A task with no binding gets nothing; a binding some local
// source disagrees with gets nothing either, and source names that source for
// the caller to log. Otherwise the op targets the binding's issue on its
// tenant, its link is written, and the first op anchors the task's
// yt_entity_map row as in the canary; now is the op's created_at. A comment
// the protocol cannot send gets a rejected op and its link, as in the canary;
// an identity problem never does.
//
// Locks: the task's lane lock, then plain reads of the binding and the
// sources, then new rows of yt_write_ops, yt_comment_write_links and
// yt_entity_map (lockorder.go). The lane lock is the one AddYTTaskBinding
// takes before it reads and inserts, so a binding is either seen whole or not
// at all.
func ytCommentOnOpTx(ctx context.Context, tx pgx.Tx, c TaskComment, now time.Time) (op ytwrite.Op, enqueued bool, source string, err error) {
	taskKey := strconv.FormatInt(c.TaskID, 10)
	if err := lockYTEntity(ctx, tx, "task", taskKey); err != nil {
		return ytwrite.Op{}, false, "", err
	}
	b, found, err := readYTBinding(ctx, tx, c.TaskID)
	if err != nil || !found {
		return ytwrite.Op{}, false, "", err
	}
	if source, err = ytBindingDisagreement(ctx, tx, b); err != nil || source != "" {
		return ytwrite.Op{}, false, source, err
	}
	req := ytwrite.CommentRequest{Principal: YTCommentPrincipal, Key: YTCommentKey(c.ID), Actor: c.Author, TaskID: c.TaskID, YTIssueID: b.IssueID, Tenant: b.Tenant, Body: c.Body}
	reason, detail := "", ""
	if cerr := ytwrite.CheckComment(req); cerr != nil {
		reason, detail = ytwrite.ReasonInvalidComment, cerr.Error()
	}
	n, err := ytwrite.CommentOp(req)
	if err != nil {
		return ytwrite.Op{}, false, "", err
	}
	now = now.UTC()
	op, _, err = prepareYTOpTx(ctx, tx, now, n)
	if err != nil {
		return ytwrite.Op{}, false, "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO yt_comment_write_links(comment_id, op_id, created_at) VALUES ($1,$2,$3)`, c.ID, op.ID, now); err != nil {
		return ytwrite.Op{}, false, "", err
	}
	if reason != "" {
		op, err = rejectYTOpTx(ctx, tx, now, op.ID, reason, detail)
		if err != nil {
			return ytwrite.Op{}, false, "", err
		}
		return op, true, "", nil
	}
	tag, err := tx.Exec(ctx, `INSERT INTO yt_entity_map(entity_kind, tenant, hk_key, yt_id, yt_parent_id, op_id, created_at)
		VALUES ('task',$1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, b.Tenant, taskKey, b.IssueID, b.ProjectID, op.ID, now)
	if err != nil {
		return ytwrite.Op{}, false, "", err
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
			return ytwrite.Op{}, false, "", fmt.Errorf("yt_entity_map conflicts with task %d's checked binding to %s (have %q): %v", c.TaskID, b.IssueID, have, err)
		}
	}
	return op, true, "", nil
}

// AddYTTaskBinding publishes a verified binding. verified is the candidate
// the binding was verified against (youtrack.VerifyBinding): the transaction
// reads the task's mappings again and refuses with ErrYTBindingConflict if
// any of them changed since, then refuses if any local source disagrees with
// the binding (ytBindingDisagreement) or another binding holds the task.
// Adding the same binding again is a no-op that returns the stored row.
// A new binding appends a 'bound' row to yt_binding_history in its
// transaction (the one UnbindYTTaskBinding's 'unbound' row follows).
// A new binding creates no map row; when task fields mode is on, it also
// prepares the task's first issue_fields op in this transaction. A binding
// published after an operator unbind also appends one task 'U' row to
// sync_log, whatever this process's fields mode, so the serve worker's
// task_fields consumer prepares the task's move and snapshot for it (hk 4539).
// The new row's comments_from is the task's highest comment id read in the
// same transaction, so the binding never copies a comment written before it.
// The automatic binder publishes through PublishYTAutoBinding, which shares
// this publication and copies its evidence's missing comments in it.
//
// Locks, in this order (lockorder.go): the issue ownership lock, FOR UPDATE on
// the task row, the task's lane lock, FOR SHARE on the task's youtrack_projects
// and task_origins rows, then the new binding row and its history row and,
// after an unbind, its sync_log row (taking the commit-time allocator lock
// every task write takes, sync_migration.go).
func (s *Store) AddYTTaskBinding(ctx context.Context, b YTTaskBinding, verified YTCanaryCandidate) (YTTaskBinding, error) {
	got, _, _, err := s.publishYTBinding(ctx, b, verified, ytBindingPublication{origin: YTBindingImportVerified})
	return got, err
}

// YTCommentCandidate is a task the comments preflight and backfill classify.
type YTCommentCandidate struct {
	TaskID  int64
	Project string
}

// YTCommentCandidates lists, in id order, the tasks with a comment created at
// or after since, every task in yt_comment_canary, and every task with a
// comment op not yet applied or rejected; all lists every task instead
// (bind-all, hk 3897). project, when not "", keeps only that hk project's
// tasks.
func (s *Store) YTCommentCandidates(ctx context.Context, since time.Time, project string, all bool) ([]YTCommentCandidate, error) {
	rows, err := s.pool.Query(ctx, `WITH ids AS (
			SELECT task_id FROM task_comments WHERE created_at >= $1
			UNION SELECT task_id FROM yt_comment_canary
			UNION SELECT CASE WHEN entity_id ~ '^[0-9]{1,18}$' THEN entity_id::bigint END FROM yt_write_ops
				WHERE principal=$3 AND kind=$4 AND entity_kind='task' AND state NOT IN ('applied','rejected')
			UNION SELECT id FROM tasks WHERE $5)
		SELECT t.id, coalesce(t.project,'') FROM tasks t JOIN ids ON ids.task_id = t.id
		WHERE $2 = '' OR t.project = $2 ORDER BY t.id`, since.UTC(), project, YTCommentPrincipal, ytwrite.KindCommentAppend, all)
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

// YTCommentCandidateOf reads one task as a candidate (`--task N` of the
// comments preflight and backfill); found is false for a task that does not
// exist.
func (s *Store) YTCommentCandidateOf(ctx context.Context, taskID int64) (YTCommentCandidate, bool, error) {
	var c YTCommentCandidate
	err := s.pool.QueryRow(ctx, `SELECT id, coalesce(project,'') FROM tasks WHERE id=$1`, taskID).Scan(&c.TaskID, &c.Project)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTCommentCandidate{}, false, nil
	}
	return c, err == nil, err
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
