package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The actions of yt_binding_history (migration_yt_binding_history.go).
const (
	ytBindingActionBound   = "bound"
	ytBindingActionUnbound = "unbound"
)

// ErrYTUnbindRefused is an unbind whose preconditions do not hold. Nothing was
// written. The text after "yt_unbind_refused: " names the task, the
// precondition and the ops or comments in the way.
var ErrYTUnbindRefused = errors.New("yt_unbind_refused")

// YTUnbindOptions is who unbinds a task and why. Force rejects the prepared
// comment ops that comments authority youtrack would otherwise keep the unbind
// from closing.
type YTUnbindOptions struct {
	By     string
	Reason string
	Force  bool
}

// YTUnbindOp is a prepared op the unbind rejected.
type YTUnbindOp struct {
	OpID   int64  `json:"op_id"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

// YTUnbindResult is what UnbindYTTaskBinding did. Removed is false, and
// nothing was written, when the task had no binding. Released lists the
// comments whose ops were rejected: hk keeps them and the operator can copy
// them again. Stranded lists the same comments when comments are canonical in
// YouTrack (Force only): no op will ever carry them. AnchorReleased is true
// when the task's yt_entity_map row, which named the removed binding's issue,
// was deleted with it (hk 4536).
type YTUnbindResult struct {
	TaskID         int64          `json:"task_id"`
	Removed        bool           `json:"removed"`
	Binding        *YTTaskBinding `json:"binding,omitempty"`
	Rejected       []YTUnbindOp   `json:"rejected"`
	Released       []int64        `json:"released"`
	Stranded       []int64        `json:"stranded"`
	AnchorReleased bool           `json:"anchor_released"`
}

// insertYTBindingHistoryTx appends one row to the task's binding history in
// the caller's transaction.
func insertYTBindingHistoryTx(ctx context.Context, tx pgx.Tx, b YTTaskBinding, action, actor, reason string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO yt_binding_history(task_id, tenant, issue_id, origin, action, actor, reason, at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, b.TaskID, b.Tenant, b.IssueID, b.Origin, action, actor, reason, at.UTC())
	return err
}

// UnbindYTTaskBinding removes a task's binding (hk 4490). Nothing is
// retargeted: the prepared ops the binding admitted are rejected, and the
// history keeps the task from being bound again by anything but the operator
// (the latest row's action is 'unbound').
//
// The task must have a binding, or the result says Removed false and nothing
// is written. Then, in this order, any refusal writes nothing:
//
//  1. no op of the task's lane is dispatched or remote_unknown: an uncertain
//     write may have landed on the old issue, and `yt ops resolve` closes it
//     first;
//  2. the task has no unresolved issue_create op (AddYTTaskBinding makes the
//     same check);
//  3. the task is not enrolled in the comment canary, which routes comments
//     in mode canary with no regard for the binding (`youtrack canary remove`);
//  4. while comments are canonical in YouTrack, no prepared hk comment op
//     exists, unless o.Force: closing it would delete the only delivery of a
//     comment YouTrack owns.
//
// Then every prepared op of the lane that this binding admits is rejected with
// ReasonBindingRemoved (ReasonUnbindForced under o.Force): hk comment_append
// ops (and the ones queued with no target, hk 4263), issue_fields and
// issue_move ops, on the binding's tenant and issue. An op of another tenant
// or aimed at another issue was never this binding's to admit and stays
// prepared, as does a document comment's copy (it reads its article binding)
// and every other kind. The binding row is deleted and the history row
// appended in the same transaction, and the task's pending issue intent, if
// any, is dropped with them so the issue worker cannot bind it again.
//
// The task's yt_entity_map row goes with the binding when it names the
// binding's issue on the binding's tenant (hk 4536). The row is the routing
// anchor the task's first op wrote, and every later binding is checked against
// it (ytBindingDisagreement, and the candidate the CLI verifies): left in
// place it would refuse every verified issue but the old one, so
// `comments-backfill --task` could not repair a wrong binding. The old issue
// stays on record without it: the unbound history row and the ops name it,
// and the comments already applied there stay there. A row that names another
// issue or tenant is not this binding's anchor and stays, and so do the mirror
// link (task_origins) and the canary row, which have their own owners.
//
// Locks, as AddYTTaskBinding takes them (lockorder.go): the issue ownership
// lock, FOR UPDATE on the task row, the task's lane lock, then the comments
// authority row FOR SHARE (the order prepareYTOpTx has), then the task's
// yt_write_ops rows, its binding row and its yt_entity_map row.
func (s *Store) UnbindYTTaskBinding(ctx context.Context, taskID int64, o YTUnbindOptions) (YTUnbindResult, error) {
	res := YTUnbindResult{TaskID: taskID, Rejected: []YTUnbindOp{}, Released: []int64{}, Stranded: []int64{}}
	if taskID < 1 || o.By == "" || !validText(o.By, 200) || !validText(o.Reason, 2000) {
		return res, errors.New("invalid task unbind")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)
	taskKey := strconv.FormatInt(taskID, 10)
	if err := lockYTIssueOwnershipTx(ctx, tx, taskID); err != nil {
		return res, err
	}
	if err := lockYTEntity(ctx, tx, "task", taskKey); err != nil {
		return res, err
	}
	b, found, err := readYTBinding(ctx, tx, taskID)
	if err != nil || !found {
		return res, err
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("%w: task %d: %s", ErrYTUnbindRefused, taskID, fmt.Sprintf(format, args...))
	}
	uncertain, err := ytLaneOpsTx(ctx, tx, taskKey, `state IN ('dispatched','remote_unknown')`)
	if err != nil {
		return res, err
	}
	if len(uncertain) > 0 {
		return res, refuse("ops %s may have reached issue %s; resolve them first (yt ops resolve)", ytUnbindOpList(uncertain), b.IssueID)
	}
	pending, err := ytIssueOpPendingTx(ctx, tx, taskID)
	if err != nil {
		return res, err
	}
	if pending {
		return res, refuse("an issue_create op is unresolved")
	}
	// In mode canary the task's enrollment routes its comments by itself,
	// without the binding, so an unbind that left it would not stop delivery
	// to the issue. The canary has its own remover.
	var enrolled bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM yt_comment_canary WHERE task_id=$1)`, taskID).Scan(&enrolled); err != nil {
		return res, err
	}
	if enrolled {
		return res, refuse("the task is enrolled in the comment canary, which routes its comments without the binding; run youtrack canary remove %d first", taskID)
	}
	side, err := ytAuthorityShareTx(ctx, tx, YTAuthorityComments)
	if err != nil {
		return res, err
	}
	if side == YTAuthorityYouTrack && !o.Force {
		held, err := ytLaneOpsTx(ctx, tx, taskKey, `state = 'prepared' AND principal = '`+YTCommentPrincipal+`' AND kind = '`+ytwrite.KindCommentAppend+`'`)
		if err != nil {
			return res, err
		}
		if len(held) > 0 {
			comments, err := ytOpCommentIDsTx(ctx, tx, ytLaneOpIDs(held))
			if err != nil {
				return res, err
			}
			return res, refuse("comments are canonical in YouTrack and ops %s (comments %s) are prepared; --force rejects them and strands the comments",
				ytUnbindOpList(held), ytUnbindIDList(comments))
		}
	}
	reason := ytwrite.ReasonBindingRemoved
	if o.Force {
		reason = ytwrite.ReasonUnbindForced
	}
	now := time.Now().UTC()
	rows, err := tx.Query(ctx, `UPDATE yt_write_ops SET state='rejected', reason=$2, last_error=$3, resolved_at=$4
		WHERE entity_kind='task' AND entity_id=$1 AND state='prepared' AND tenant=$10
			AND ((principal=$5 AND kind=$6 AND (yt_target_id=$11 OR yt_target_id=''))
				OR (principal=$7 AND kind IN ($8,$9) AND yt_target_id=$11))
		RETURNING id, principal, kind`,
		taskKey, reason, clipText(fmt.Sprintf("the binding to %s on %s was removed by %s", b.IssueID, b.Tenant, o.By), 2000), now,
		YTCommentPrincipal, ytwrite.KindCommentAppend, YTFieldsPrincipal, ytwrite.KindIssueFields, ytwrite.KindIssueMove, b.Tenant, b.IssueID)
	if err != nil {
		return res, err
	}
	var commentOps []int64
	for rows.Next() {
		var id int64
		var principal, kind string
		if err := rows.Scan(&id, &principal, &kind); err != nil {
			rows.Close()
			return res, err
		}
		res.Rejected = append(res.Rejected, YTUnbindOp{OpID: id, Kind: kind, Reason: reason})
		if kind == ytwrite.KindCommentAppend {
			commentOps = append(commentOps, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, err
	}
	slices.SortFunc(res.Rejected, func(a, b YTUnbindOp) int { return cmp.Compare(a.OpID, b.OpID) })
	comments, err := ytOpCommentIDsTx(ctx, tx, commentOps)
	if err != nil {
		return res, err
	}
	if side == YTAuthorityYouTrack {
		res.Stranded = comments
	} else {
		res.Released = comments
	}
	if _, err := tx.Exec(ctx, `DELETE FROM yt_task_bindings WHERE task_id=$1`, taskID); err != nil {
		return res, err
	}
	// The anchor's writers (the comment enqueue, the canary, the apply of an
	// issue_create) all hold the task's lane lock, which this transaction
	// holds, so the row cannot appear or change between the read of the
	// binding and this delete.
	anchor, err := tx.Exec(ctx, `DELETE FROM yt_entity_map WHERE entity_kind='task' AND hk_key=$1 AND yt_id=$2 AND (tenant=$3 OR tenant='')`,
		taskKey, b.IssueID, b.Tenant)
	if err != nil {
		return res, err
	}
	res.AnchorReleased = anchor.RowsAffected() > 0
	// A pending issue intent (a task created while its project was unmapped)
	// would make the issue worker create or adopt an issue and publish a
	// binding again, undoing the unbind. The operator took over the task's
	// issue; dropYTIssueIntentTx is what ensureYTIssueTx does for a task that
	// has one.
	if err := dropYTIssueIntentTx(ctx, tx, taskID); err != nil {
		return res, err
	}
	if err := insertYTBindingHistoryTx(ctx, tx, b, ytBindingActionUnbound, o.By, o.Reason, now); err != nil {
		return res, err
	}
	res.Removed, res.Binding = true, &b
	return res, tx.Commit(ctx)
}

// ytLaneOp is an op of a task's lane named in a refusal.
type ytLaneOp struct {
	id   int64
	kind string
}

// ytLaneOpsTx lists, in id order, the ops of the task's lane (entity kind
// task) that match cond, a fixed SQL condition over yt_write_ops.
func ytLaneOpsTx(ctx context.Context, tx pgx.Tx, taskKey, cond string) ([]ytLaneOp, error) {
	rows, err := tx.Query(ctx, `SELECT id, kind FROM yt_write_ops WHERE entity_kind='task' AND entity_id=$1 AND `+cond+` ORDER BY id`, taskKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ytLaneOp
	for rows.Next() {
		var op ytLaneOp
		if err := rows.Scan(&op.id, &op.kind); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func ytUnbindOpList(ops []ytLaneOp) string {
	parts := make([]string, len(ops))
	for i, op := range ops {
		parts[i] = strconv.FormatInt(op.id, 10) + " (" + op.kind + ")"
	}
	return strings.Join(parts, ", ")
}

func ytUnbindIDList(ids []int64) string {
	if len(ids) == 0 {
		return "none"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ", ")
}

func ytLaneOpIDs(ops []ytLaneOp) []int64 {
	ids := make([]int64, len(ops))
	for i, op := range ops {
		ids[i] = op.id
	}
	return ids
}

// ytOpCommentIDsTx lists, ascending, the hk comments that the ops carry
// through yt_comment_write_links.
func ytOpCommentIDsTx(ctx context.Context, tx pgx.Tx, opIDs []int64) ([]int64, error) {
	out := []int64{}
	if len(opIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT comment_id FROM yt_comment_write_links WHERE op_id = ANY($1) ORDER BY comment_id`, opIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
