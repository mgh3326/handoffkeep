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

// Project moves (hk 4055, docs/youtrack-write-protocol.md "Kind issue_move"):
// a task whose hk project changed while its issue stayed in the old YouTrack
// project. The task_fields consumer (yt_fields.go) prepares one issue_move op
// for it and then the task's snapshot, on the task's lane, so the snapshot
// reaches the issue once the move applied. The move applies in the same
// transaction as the binding's new project (bindYTMoveTx). Moves have their
// own gate, HANDOFFKEEP_YT_MOVES_MODE (default off): while it is off a moved
// task is skipped as it was before moves existed, and no move op is admitted.

// YTMovesMode gates the project moves (HANDOFFKEEP_YT_MOVES_MODE).
type YTMovesMode string

const (
	// YTMovesOff prepares no move, and admits no issue_move op (the default).
	YTMovesOff YTMovesMode = "off"
	// YTMovesOn prepares the move of a moved task and admits the issue_move
	// ops whose target is the task's bound issue.
	YTMovesOn YTMovesMode = "on"
)

// SetYTMovesMode sets the project-move gate for this process. serve calls it
// once at start. Any value that is not on is off.
func (s *Store) SetYTMovesMode(m YTMovesMode) { s.ytMoves.Store(m) }

// ytMovesNow is the gate: on, or off (unset, or any other value).
func (s *Store) ytMovesNow() YTMovesMode {
	switch m, _ := s.ytMoves.Load().(YTMovesMode); m {
	case YTMovesOn:
		return YTMovesOn
	default:
		return YTMovesOff
	}
}

func init() { registerYTKindGate(ytwrite.KindIssueMove, ytMoveGateSQL) }

// ytMoveGateSQL is the issue_move gate (ytAdmitSQL): moves mode on, and o's
// target is the issue o's task is bound to on o's tenant now. Moves mode off
// admits nothing.
func ytMoveGateSQL(s *Store) string {
	if s.ytMovesNow() != YTMovesOn {
		return `FALSE`
	}
	return `o.entity_kind = 'task' AND EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)`
}

// ytFieldsMoveSkipped is a task whose issue must move but cannot: its hk
// project is cleared or maps to no YouTrack project (nothing to move to), or
// the move the task needs was already prepared once and refused (a rejected
// move with the same target is never prepared again, hk 4111). The issue
// stays in the project it is in (or on its way to), and the task's snapshot
// is still prepared for it there (hk 4118). A task rebound under the feed is
// counted here too, with nothing prepared.
const ytFieldsMoveSkipped ytFieldsOutcome = "move_skipped"

// ytMoveDecision is what ytMoveForTx decided for one task: project, the
// YouTrack project its snapshot names ("" for nothing to prepare); moving, a
// move is pending or was prepared; skipped, the issue should move but cannot;
// prepared, a move op was prepared.
type ytMoveDecision struct {
	project                   string
	moving, skipped, prepared bool
}

// ytMoveForTx decides, for a task bound to its issue in tx, the YouTrack
// project its issue belongs in and prepares the move to it when the issue is
// not there or on its way there. The project is the task's mapped project; a
// task with no hk project stays where its issue is (the HK bucket) while the
// binding agrees with that. The issue is on its way where its latest
// unresolved move goes (ytPendingMoveTx), so a task moved away and back before
// the first move applied gets a move back.
//
// It returns the project the task's snapshot names, moving (a move is pending
// for the task or was prepared here), skipped (the issue should move but
// cannot) and prepared (a move op was prepared here). When the task's project is cleared or maps to no YouTrack project,
// or the move to its project was prepared before and refused, no move is
// prepared and the project is the one the issue is in or on its way to, so
// its fields still reach it there (hk 4118, hk 4111). A move is prepared
// once per target (prepareYTOnceTx): a refused move is prepared again only
// when the target changes, never on every pass.
//
// The binding is read again under the task's lane lock: a move's apply changes
// the binding's project without a change the feed would see, so the read the
// feed made may be older than the lock. A binding that is no longer the task's
// issue on the tenant is skipped for this pass: project is "", nothing is
// prepared.
//
// Errors wrapping ytwrite.ErrInvalid are a row the protocol cannot store; the
// caller skips the task and logs it. The caller holds nothing but the feed
// lock: this takes the task's lane lock, the lock every op of the task takes.
func ytMoveForTx(ctx context.Context, tx pgx.Tx, now time.Time, id int64, t ytFieldsRow) (d ytMoveDecision, err error) {
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(id, 10)); err != nil {
		return ytMoveDecision{}, err
	}
	bound, found, err := ytBindingNowTx(ctx, tx, id)
	if err != nil {
		return ytMoveDecision{}, err
	}
	if !found || bound.issueID != t.issueID || bound.tenant != t.tenant {
		log.Printf("ytwrite.move_skipped_rebound task_id=%d issue_id=%q", id, t.issueID)
		return ytMoveDecision{}, nil
	}
	pending, err := ytPendingMoveTx(ctx, tx, id, t.issueID)
	if err != nil {
		return ytMoveDecision{}, err
	}
	effective := bound.projectID
	if pending != "" {
		effective = pending
	}
	desired := t.mappedProjectID
	if desired == "" && t.projectAgrees {
		desired = t.bindingProjectID
	}
	if desired == "" {
		// Cleared or unmapped: nothing to move to. The issue stays where it
		// is, and its fields are written there (hk 4118).
		log.Printf("ytwrite.move_skipped_unmapped task_id=%d project=%q issue_id=%q binding_project_id=%q",
			id, t.fields.Project, t.issueID, bound.projectID)
		return ytMoveDecision{project: effective, moving: pending != "", skipped: true}, nil
	}
	if effective == desired {
		return ytMoveDecision{project: desired, moving: pending != ""}, nil
	}
	mv, err := ytwrite.MoveOp(ytwrite.MoveRequest{Principal: YTFieldsPrincipal, Actor: YTFieldsPrincipal, Tenant: t.tenant,
		IssueID: t.issueID, ProjectID: desired, TaskID: id})
	if err != nil {
		return ytMoveDecision{}, err
	}
	op, made, err := prepareYTOnceTx(ctx, tx, now, mv, mv.PayloadSHA)
	if err != nil {
		return ytMoveDecision{}, err
	}
	if !made {
		// The move to desired was the last one prepared and is no longer
		// pending, so the protocol settled it (rejected) without moving the
		// issue. It is not prepared again for the same target; the issue
		// stays where it is and its fields are written there.
		log.Printf("ytwrite.move_stuck task_id=%d issue_id=%q project_id=%q staying_in=%q", id, t.issueID, desired, effective)
		return ytMoveDecision{project: effective, moving: pending != "", skipped: true}, nil
	}
	log.Printf("ytwrite.move_enqueued op_id=%d task_id=%d issue_id=%q project_id=%q from=%q",
		op.ID, id, t.issueID, desired, effective)
	return ytMoveDecision{project: desired, moving: true, prepared: true}, nil
}

// claimYTFieldsProjectTx is the claim-time check of an issue_fields op whose
// lane is clear (hk 4111): the op names the YouTrack project its issue was to
// be in, and Claim treats a rejected issue_move before it as resolved. When the
// task's binding (on the op's tenant and issue) names another project now, the
// move did not happen, and the op is rejected (ytwrite.ReasonIssueNotMoved)
// instead of writing a snapshot meant for one project to the issue in
// another. The feed then prepares the snapshot for the project the issue is
// in (ytMoveForTx). Any other op, or an op whose binding moved to another
// issue or tenant (the gate holds those), passes. The caller holds the task's
// lane lock (Claim).
func claimYTFieldsProjectTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op) (reject, detail string, err error) {
	if op.Kind != ytwrite.KindIssueFields || op.EntityKind != "task" {
		return "", "", nil
	}
	want, err := ytwrite.IssueFieldsProject(op)
	if err != nil {
		return ytwrite.ReasonInvalidIssue, err.Error(), nil
	}
	var bound string
	err = tx.QueryRow(ctx, `SELECT project_id FROM yt_task_bindings WHERE task_id::text=$1 AND tenant=$2 AND issue_id=$3`,
		op.EntityID, op.Tenant, op.YTTargetID).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if bound == want {
		return "", "", nil
	}
	log.Printf("ytwrite.fields_not_moved op_id=%d task_id=%s issue_id=%q project_id=%q binding_project_id=%q",
		op.ID, op.EntityID, op.YTTargetID, want, bound)
	return ytwrite.ReasonIssueNotMoved, "the snapshot is for project " + want + " but the issue is bound in " + bound + " (its move did not apply)", nil
}

// ytBinding is a task's binding as the store reads it under the task's lane lock.
type ytBinding struct {
	tenant, issueID, projectID string
}

// ytBindingNowTx reads task's binding in tx: found is false for an unbound task.
func ytBindingNowTx(ctx context.Context, tx pgx.Tx, taskID int64) (b ytBinding, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT tenant, issue_id, project_id FROM yt_task_bindings WHERE task_id=$1`, taskID).
		Scan(&b.tenant, &b.issueID, &b.projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ytBinding{}, false, nil
	}
	if err != nil {
		return ytBinding{}, false, err
	}
	return b, true, nil
}

// ytPendingMoveTx is the project the task's issue is moving to: the target of
// the task's latest issue_move op on issueID that is not yet applied or
// rejected, or "" when none is pending. The caller holds the task's lane lock,
// so the answer is the one the lane's next op sees.
func ytPendingMoveTx(ctx context.Context, tx pgx.Tx, taskID int64, issueID string) (string, error) {
	var project string
	err := tx.QueryRow(ctx, `SELECT coalesce((SELECT payload->>'project_id' FROM yt_write_ops
		WHERE kind=$1 AND entity_kind='task' AND entity_id=$2 AND yt_target_id=$3 AND state NOT IN ('applied','rejected')
		ORDER BY id DESC LIMIT 1),'')`, ytwrite.KindIssueMove, strconv.FormatInt(taskID, 10), issueID).Scan(&project)
	return project, err
}

// bindYTMoveTx publishes an applied issue_move op's project on its task's
// binding, in the apply's transaction, which holds the task's lane lock. The
// binding keeps its issue: a move changes only the project. issue_readable is
// cleared: the protocol carries only the stable id, so the reply's readable id
// is not known here, and an issue_create binding leaves it empty too. A
// binding that no longer names the op's issue (rebound since the op was
// prepared) is ErrConflictingResult, and the apply rolls back.
func bindYTMoveTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op) error {
	taskID, err := strconv.ParseInt(op.EntityID, 10, 64)
	if err != nil || taskID < 1 || op.EntityKind != "task" {
		return fmt.Errorf("issue_move op %d is not on a task lane (%s:%s)", op.ID, op.EntityKind, op.EntityID)
	}
	project, err := ytwrite.IssueMoveProject(op)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE yt_task_bindings SET project_id=$3, issue_readable=''
		WHERE task_id=$1 AND tenant=$2 AND issue_id=$4`, taskID, op.Tenant, project, op.YTTargetID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: issue %s on %s is not bound to task %d any more (move op %d)", ytwrite.ErrConflictingResult, op.YTTargetID, op.Tenant, taskID, op.ID)
	}
	log.Printf("ytwrite.move_applied op_id=%d task_id=%d issue_id=%q project_id=%q", op.ID, taskID, op.YTTargetID, project)
	return nil
}
