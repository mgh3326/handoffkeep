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

// Held-op recovery (hk 4491; docs/youtrack-write-protocol.md, Held-op
// recovery). A task's yt_task_bindings row is the admission gate of three op
// kinds (comment_append for hk comments, issue_fields, issue_move). When the
// row disappears or names another issue, the ops it admitted stay prepared:
// the claim answers ClaimHeld, Due leaves them out, and every later op on the
// task's lane is blocked behind them. Nothing else closes a prepared op, so
// RecoverHeldYTOps does, after a grace, with a recorded reason and never a
// retarget (an op's tenant and issue are fixed when it is prepared).

const (
	// YTHeldRecoveryGrace is how long an op must have been prepared before the
	// recovery pass may close it: a binding that is being repaired by hand has
	// an hour to come back.
	YTHeldRecoveryGrace = time.Hour
	// ytHeldRecoveryPage and ytHeldRecoveryPages bound one pass to
	// pages*page candidates, oldest first. The candidate scan already leaves
	// out the ops a pass would keep (another tenant's binding, content
	// canonical in YouTrack), so a kept op never hides a later one; a full
	// pass rejects what it examined, and the next pass reaches the rest.
	ytHeldRecoveryPage  = 200
	ytHeldRecoveryPages = 5
)

// ytHeldClass is why a prepared op is not sent, or ytHeldReady when it will
// be. The alert worker labels its notice with it; the recovery pass acts on
// the two that mean the binding is gone.
type ytHeldClass string

const (
	// ytHeldReady: the kind's gate admits the op now; it is waiting for its
	// turn on the lane, or for a retry time.
	ytHeldReady ytHeldClass = "ready"
	// ytHeldBinding: the op's task has no binding. Recovery rejects the op
	// (binding_removed).
	ytHeldBinding ytHeldClass = "held_binding"
	// ytHeldBindingChanged: the task is bound to another issue on the op's
	// tenant. Recovery rejects the op (binding_changed).
	ytHeldBindingChanged ytHeldClass = "held_binding_changed"
	// ytHeldOtherTenant: the task's binding is on another tenant, whose worker
	// may own the op. Never closed.
	ytHeldOtherTenant ytHeldClass = "held_other_tenant"
	// ytHeldAuthorityYouTrack: the op would be closed, but the content it
	// carries is canonical in YouTrack, so it may be the only delivery. Never
	// closed; the operator restores the binding or resolves the op.
	ytHeldAuthorityYouTrack ytHeldClass = "held_authority_youtrack"
	// ytHeldMode: the op's kind has its write mode off in this process (a
	// stop). Never touched; turning the mode on releases it.
	ytHeldMode ytHeldClass = "held_mode"
)

// ytHeldOp is the part of a prepared op the classification reads.
type ytHeldOp struct {
	id                                  int64
	principal, kind, tenant, entityKind string
	entityID, target                    string
	createdAt                           time.Time
}

const ytHeldOpColumns = `o.id, o.principal, o.kind, o.tenant, o.entity_kind, o.entity_id, o.yt_target_id, o.created_at`

func scanYTHeldOp(r ytOpScanner) (ytHeldOp, error) {
	var op ytHeldOp
	err := r.Scan(&op.id, &op.principal, &op.kind, &op.tenant, &op.entityKind, &op.entityID, &op.target, &op.createdAt)
	op.createdAt = op.createdAt.UTC()
	return op, err
}

// ytHeldScopeSQL selects the ops whose gate is a task's binding. A document
// comment (the same kind, another principal) reads yt_article_bindings, and
// the create kinds have their own gates; neither is this pass's.
const ytHeldScopeSQL = `o.entity_kind = 'task' AND ((o.kind = '` + ytwrite.KindCommentAppend + `' AND o.principal = '` + YTCommentPrincipal + `')
	OR o.kind IN ('` + ytwrite.KindIssueFields + `','` + ytwrite.KindIssueMove + `'))`

// ytHeldInScope is ytHeldScopeSQL for an op already read.
func ytHeldInScope(op ytHeldOp) bool {
	if op.entityKind != "task" {
		return false
	}
	switch op.kind {
	case ytwrite.KindCommentAppend:
		return op.principal == YTCommentPrincipal
	case ytwrite.KindIssueFields, ytwrite.KindIssueMove:
		return true
	}
	return false
}

// ytHeldKindModeOn reports whether kind's write mode is on in this process.
// Only a binding-gated kind has an answer; any other kind is off.
func (s *Store) ytHeldKindModeOn(kind string) bool {
	switch kind {
	case ytwrite.KindCommentAppend:
		return s.ytCommentsModeNow() == YTCommentsOn
	case ytwrite.KindIssueFields:
		m, _ := s.ytFieldsNow()
		return m == YTFieldsOn
	case ytwrite.KindIssueMove:
		return s.ytMovesNow() == YTMovesOn
	}
	return false
}

// ytHeldKindsOn lists the binding-gated kinds whose mode is on, for the
// candidate query: an op of a kind that is off is held_mode and is not read.
func (s *Store) ytHeldKindsOn() []string {
	var kinds []string
	for _, k := range []string{ytwrite.KindCommentAppend, ytwrite.KindIssueFields, ytwrite.KindIssueMove} {
		if s.ytHeldKindModeOn(k) {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

// ytHeldClassOf classifies a prepared op that its kind's gate does not admit
// although the kind's mode is on, from the task's binding (nil when it has
// none) and the canonical side of the op's content kind.
//
// A comment or snapshot whose content is canonical in YouTrack is kept: it is
// the only delivery of text YouTrack must end up holding, so closing it would
// lose data that no audit repairs (design G7). Under hk the op is only a copy
// and closing it with a reason is safe.
//
// A binding that names the op's tenant and issue cannot reach here: the gate
// would admit the op. The design's held_ambiguous class (the binding matches
// but a local source disagrees) therefore has no case; the claim of a
// dispatched op does not read ytBindingDisagreement for an op that has a
// target, and a targetless comment is admitted by its gate.
func ytHeldClassOf(op ytHeldOp, b *YTTaskBinding, side YTAuthoritySide) ytHeldClass {
	var class ytHeldClass
	switch {
	case b == nil:
		class = ytHeldBinding
	case b.Tenant != op.tenant:
		return ytHeldOtherTenant
	case b.IssueID != op.target:
		class = ytHeldBindingChanged
	default:
		return ytHeldReady
	}
	if side == YTAuthorityYouTrack {
		return ytHeldAuthorityYouTrack
	}
	return class
}

// classifyHeldYTOpTx reads what ytHeldClassOf needs for op and classifies it.
// The reads take no lock; the caller holds the ones it needs. side is the
// canonical side of the op's content kind.
func (s *Store) classifyHeldYTOpTx(ctx context.Context, q ytRowQuerier, op ytHeldOp, side YTAuthoritySide) (ytHeldClass, error) {
	if !s.ytHeldKindModeOn(op.kind) {
		return ytHeldMode, nil
	}
	var admitted bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.id=$1 AND `+s.ytAdmitSQL("$2")+`)`, op.id, op.tenant).Scan(&admitted); err != nil {
		return "", err
	}
	if admitted {
		return ytHeldReady, nil
	}
	var binding *YTTaskBinding
	if taskID, err := strconv.ParseInt(op.entityID, 10, 64); err == nil && taskID > 0 {
		b, found, err := readYTBinding(ctx, q, taskID)
		if err != nil {
			return "", err
		}
		if found {
			binding = &b
		}
	}
	return ytHeldClassOf(op, binding, side), nil
}

// YTRecovery is what one RecoverHeldYTOps pass did: Rejected ops closed (OpIDs
// names them) and Kept ops it decided to leave after reading them under the
// lock (a change since the scan); the ops it never reads, a standing hold, are
// named by the alert.
type YTRecovery struct {
	Rejected, Kept int
	OpIDs          []int64
}

// RecoverHeldYTOps closes the prepared ops of binding-gated kinds whose task
// no longer has the binding that admitted them. It acts for one tenant, the
// invoking worker's: an op on another tenant is that tenant's worker's to
// hold or close, whatever this process's modes are. An op is a candidate when
// it is the tenant's, has been prepared for longer than grace, its kind's mode
// is on in this process and its kind's gate does not admit it (read through
// yt_write_ops_alert_scan). The scan leaves out the ops the decision below
// would keep for a standing reason (a binding on another tenant, content
// canonical in YouTrack), so they cannot starve the ops behind them. Each candidate is decided in its own transaction,
// under its task's lane lock, from the rows as they are then:
//
//   - no binding: rejected, binding_removed;
//   - a binding to another issue on the op's tenant: rejected, binding_changed;
//   - a binding on another tenant: kept (that tenant's worker may own the op);
//   - the op's content is canonical in YouTrack (the authority row, FOR SHARE
//     under the lane lock, so a switch cannot count the op as pending and then
//     see it closed): kept;
//   - admitted by now: left alone.
//
// It never retargets an op, never closes a dispatched or remote_unknown op
// (the operator resolves those), and is idempotent: a rejected op is terminal.
//
// Locks: one lane lock per transaction (lockYTEntity), then the op row FOR
// UPDATE, then the authority row FOR SHARE, as op preparation does
// (lockorder.go). It reads no clock: time is passed in.
func (s *Store) RecoverHeldYTOps(ctx context.Context, tenant string, now time.Time, grace time.Duration) (YTRecovery, error) {
	return s.recoverHeldYTOps(ctx, tenant, 0, now, grace)
}

// RecoverHeldYTOpsForTask is RecoverHeldYTOps restricted to one task. It is
// used by comments-drain --task so a scoped command cannot reject another
// task's held ops.
func (s *Store) RecoverHeldYTOpsForTask(ctx context.Context, tenant string, taskID int64, now time.Time, grace time.Duration) (YTRecovery, error) {
	if taskID < 1 {
		return YTRecovery{}, errors.New("invalid task id")
	}
	return s.recoverHeldYTOps(ctx, tenant, taskID, now, grace)
}

func (s *Store) recoverHeldYTOps(ctx context.Context, tenant string, taskID int64, now time.Time, grace time.Duration) (YTRecovery, error) {
	var out YTRecovery
	kinds := s.ytHeldKindsOn()
	if len(kinds) == 0 {
		return out, nil
	}
	cutoff := now.UTC().Add(-grace)
	afterAt, afterID := time.Unix(0, 0).UTC(), int64(0)
	for range ytHeldRecoveryPages {
		cands, err := s.heldYTCandidatesForTask(ctx, tenant, taskID, kinds, cutoff, afterAt, afterID, ytHeldRecoveryPage)
		if err != nil {
			return out, err
		}
		for _, c := range cands {
			rejected, err := s.recoverHeldYTOp(ctx, now, c)
			if err != nil {
				return out, err
			}
			if rejected {
				out.Rejected++
				out.OpIDs = append(out.OpIDs, c.id)
			} else {
				out.Kept++
			}
		}
		if len(cands) < ytHeldRecoveryPage {
			break
		}
		last := cands[len(cands)-1]
		afterAt, afterID = last.createdAt, last.id
	}
	return out, nil
}

// ytHeldKeptSQL is the part of the candidate scan that leaves out the ops the
// decision keeps for a standing reason: the task is bound on another tenant,
// or the op's content kind is canonical in YouTrack. It is a prefilter only;
// recoverHeldYTOp decides from the rows under the lane lock.
var ytHeldKeptSQL = `NOT EXISTS (SELECT 1 FROM yt_task_bindings b WHERE b.task_id::text = o.entity_id AND b.tenant <> o.tenant)
	AND NOT EXISTS (SELECT 1 FROM yt_authority a WHERE a.authority = '` + string(YTAuthorityYouTrack) + `' AND (
		(o.principal = '` + YTCommentPrincipal + `' AND a.kind = '` + string(ytAuthorityKindOf(YTCommentPrincipal)) + `')
		OR (o.principal = '` + YTFieldsPrincipal + `' AND a.kind = '` + string(ytAuthorityKindOf(YTFieldsPrincipal)) + `')))`

// heldYTCandidates lists up to limit prepared ops of tenant and kinds created
// at or before cutoff, after the (created_at, id) position, that the gate of
// their kind does not admit and that the decision may reject.
func (s *Store) heldYTCandidates(ctx context.Context, tenant string, kinds []string, cutoff, afterAt time.Time, afterID int64, limit int) ([]ytHeldOp, error) {
	return s.heldYTCandidatesForTask(ctx, tenant, 0, kinds, cutoff, afterAt, afterID, limit)
}

func (s *Store) heldYTCandidatesForTask(ctx context.Context, tenant string, taskID int64, kinds []string, cutoff, afterAt time.Time, afterID int64, limit int) ([]ytHeldOp, error) {
	task := ""
	if taskID > 0 {
		task = strconv.FormatInt(taskID, 10)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+ytHeldOpColumns+` FROM yt_write_ops o
		WHERE o.state='prepared' AND o.tenant=$6 AND o.created_at <= $1 AND (o.created_at, o.id) > ($2, $3)
		AND o.kind = ANY($4) AND ($7 = '' OR o.entity_id = $7) AND `+ytHeldScopeSQL+` AND NOT `+s.ytAdmitSQL("$6")+` AND `+ytHeldKeptSQL+`
		ORDER BY o.created_at, o.id LIMIT $5`, cutoff.UTC(), afterAt.UTC(), afterID, kinds, limit, tenant, task)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ytHeldOp
	for rows.Next() {
		op, err := scanYTHeldOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// recoverHeldYTOp decides one candidate. Everything the candidate scan read
// is stale by now, so the op, its binding, the modes and the authority are
// read again under the lane lock, and an op that was dispatched, resolved or
// admitted in between is left alone.
func (s *Store) recoverHeldYTOp(ctx context.Context, now time.Time, c ytHeldOp) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, c.entityKind, c.entityID); err != nil {
		return false, err
	}
	op, err := scanYTHeldOp(tx.QueryRow(ctx, `SELECT `+ytHeldOpColumns+` FROM yt_write_ops o WHERE o.id=$1 AND o.state='prepared' FOR UPDATE`, c.id))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	kind := ytAuthorityKindOf(op.principal)
	if kind == "" || !ytHeldInScope(op) {
		return false, nil
	}
	side, err := ytAuthorityShareTx(ctx, tx, kind)
	if err != nil {
		return false, err
	}
	class, err := s.classifyHeldYTOpTx(ctx, tx, op, side)
	if err != nil {
		return false, err
	}
	var reason, detail string
	switch class {
	case ytHeldBinding:
		reason = ytwrite.ReasonBindingRemoved
		detail = fmt.Sprintf("task %s has no binding on %s; the op targets %s", op.entityID, op.tenant, op.target)
	case ytHeldBindingChanged:
		reason = ytwrite.ReasonBindingChanged
		detail = fmt.Sprintf("task %s is bound to another issue on %s; the op targets %s", op.entityID, op.tenant, op.target)
	default:
		return false, nil
	}
	if _, err := rejectYTOpTx(ctx, tx, now, op.id, reason, detail); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	log.Printf("ytwrite.held_op_rejected op_id=%d task_id=%s kind=%s reason=%s", op.id, op.entityID, op.kind, reason)
	return true, nil
}
