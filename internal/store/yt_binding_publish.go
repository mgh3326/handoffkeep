package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The automatic binder's publication (hk 4492,
// design/2026-11-13/yt-comment-binding-lifecycle §6.6, §6.7). One transaction
// publishes the binding and prepares the op of every comment the binder's
// issue listing proved missing, so the copies at or below comments_from are
// never debt held in memory or in a scheduling row: the op ledger is their
// durable record (I3). A failure anywhere rolls the binding back with them.

// YTAutoBindEvidence is what the binder's issue listing proved, computed under
// the comments authority side it read. The publication rechecks the authority
// and every comment under its locks; a change since the listing refuses the
// publication.
type YTAutoBindEvidence struct {
	Authority YTAuthoritySide
	// CommentsFrom is the highest comment id the issue holds (0: none). The
	// publication stores it as the binding's watermark and copies the
	// Missing set below it. Ignored under comments authority youtrack, where
	// the transaction applies the max-id rule itself.
	CommentsFrom int64
	// Missing is the comment ids the issue provably lacks, oldest first.
	// Empty under comments authority youtrack.
	Missing []int64
}

// YTAutoBinding is a committed automatic publication: the stored binding and
// the ops its transaction prepared for the evidence's missing comments.
// Linked counts the missing comments another writer had already linked.
type YTAutoBinding struct {
	Binding  YTTaskBinding
	Prepared int
	Linked   int
}

// ErrYTBindingExists is an automatic publication refused because the task
// already has a binding, of any origin. Its publisher owns its comments_from
// and everything at or below it; an automatic publication never adopts or
// completes another publisher's binding.
var ErrYTBindingExists = errors.New("yt_binding_exists")

// YTAutoBindCopyMax bounds the missing comments one automatic publication
// copies in its own transaction: the ownership lock it holds is global, so a
// larger history is refused before anything is written. The operator path for
// such a task is `comments-backfill --task N` then the audit.
const YTAutoBindCopyMax = 500

// ytBindingPublication is how one publication call runs. The backfill
// (import_verified) uses the task's highest comment id and treats the same
// binding again as a no-op; the automatic binder (auto_verified) inserts a new
// binding under the authority its evidence was read on and copies the
// evidence's missing comments.
type ytBindingPublication struct {
	origin   string
	actor    string
	auto     bool
	evidence YTAutoBindEvidence
}

// PublishYTAutoBinding publishes a verified binding from the automatic binder
// and prepares the op of every comment its evidence proved missing, in one
// transaction: the binding, its history row, the comment ops, their links and
// the task's map anchor commit or roll back together (I3, I8). It returns the
// stored binding and the ops this transaction prepared.
//
// An existing binding of any origin is ErrYTBindingExists; an operator unbind
// recorded since the YouTrack reads is ErrYTBindingExcluded (I7); a mapping or
// comments-authority change since the evidence is ErrYTBindingConflict.
//
// Locks, in this order (lockorder.go): the issue ownership lock, FOR UPDATE on
// the task row, the task's lane lock, FOR SHARE on the task's
// youtrack_projects and task_origins rows, the comments authority row FOR
// SHARE (as UnbindYTTaskBinding reads it), then the new binding, history and
// op/link/map rows.
func (s *Store) PublishYTAutoBinding(ctx context.Context, b YTTaskBinding, verified YTCanaryCandidate, ev YTAutoBindEvidence, actor string) (YTAutoBinding, error) {
	if actor == "" || ev.CommentsFrom < 0 || (ev.Authority != YTAuthorityHK && ev.Authority != YTAuthorityYouTrack) {
		return YTAutoBinding{}, errors.New("invalid automatic task binding publication")
	}
	if len(ev.Missing) > YTAutoBindCopyMax {
		return YTAutoBinding{}, errors.New("invalid automatic task binding publication: history too large")
	}
	if ev.Authority == YTAuthorityYouTrack && len(ev.Missing) != 0 {
		return YTAutoBinding{}, errors.New("invalid automatic task binding publication: copies under youtrack")
	}
	for _, id := range ev.Missing {
		if id < 1 {
			return YTAutoBinding{}, errors.New("invalid automatic task binding evidence")
		}
	}
	got, prepared, linked, err := s.publishYTBinding(ctx, b, verified, ytBindingPublication{
		origin: YTBindingAutoVerified, actor: actor, auto: true, evidence: ev})
	if err != nil {
		return YTAutoBinding{}, err
	}
	return YTAutoBinding{Binding: got, Prepared: prepared, Linked: linked}, nil
}

// ytEnqueuedCopy is a comment op the publication prepared, logged after its
// commit (logYTCommentEnqueued reads only committed rows).
type ytEnqueuedCopy struct {
	comment TaskComment
	op      ytwrite.Op
}

// publishYTBinding is the publication both callers share. For the backfill
// (p.auto false) the watermark is the task's highest comment id and nothing is
// copied; for the binder it is the evidence, checked again under the locks,
// and the missing comments are copied here.
func (s *Store) publishYTBinding(ctx context.Context, b YTTaskBinding, verified YTCanaryCandidate, p ytBindingPublication) (YTTaskBinding, int, int, error) {
	if b.TaskID < 1 || b.Tenant == "" || len(b.Tenant) > 255 || b.IssueID == "" || len(b.IssueID) > 64 ||
		len(b.IssueReadable) > 64 || b.ProjectID == "" || len(b.ProjectID) > 64 || b.BoundBy == "" || !validText(b.BoundBy, 200) || verified.TaskID != b.TaskID ||
		(p.actor != "" && !validText(p.actor, 200)) {
		return YTTaskBinding{}, 0, 0, errors.New("invalid task binding")
	}
	commentsFrom := int64(-1)
	if p.auto && p.evidence.Authority == YTAuthorityHK {
		commentsFrom = p.evidence.CommentsFrom
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockYTIssueOwnershipTx(ctx, tx, b.TaskID); err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(b.TaskID, 10)); err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	current, err := readYTCanaryCandidate(ctx, tx, b.TaskID, " FOR SHARE")
	if err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	if current != verified {
		return YTTaskBinding{}, 0, 0, fmt.Errorf("%w: task %d's YouTrack mappings changed after verification (verified %+v, now %+v); verify again",
			ErrYTBindingConflict, b.TaskID, verified, current)
	}
	have, found, err := readYTBinding(ctx, tx, b.TaskID)
	if err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	latest, err := ytBindingLatestActionTx(ctx, tx, b.TaskID)
	if err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	// An operator unbind recorded while the binder was doing its YouTrack
	// reads is not undone by this publication (I7): UnbindYTTaskBinding takes
	// the same ownership lock first, so one of the two sees the other whole.
	// The operator's backfill (origin import_verified) is the supported way
	// past an unbind and is not checked.
	if p.auto && latest == ytBindingActionUnbound {
		return YTTaskBinding{}, 0, 0, fmt.Errorf("%w: task %d was unbound by the operator", ErrYTBindingExcluded, b.TaskID)
	}
	if found {
		if p.auto {
			return YTTaskBinding{}, 0, 0, fmt.Errorf("%w: task %d already has a binding to issue %s on %s", ErrYTBindingExists, b.TaskID, have.IssueID, have.Tenant)
		}
		if have.Tenant != b.Tenant || have.IssueID != b.IssueID {
			return YTTaskBinding{}, 0, 0, fmt.Errorf("%w: task %d is bound to issue %s on %s", ErrYTBindingConflict, b.TaskID, have.IssueID, have.Tenant)
		}
		return have, 0, 0, tx.Commit(ctx)
	}
	// hk is creating (or adopting) the task's issue itself (hk 3898): that op
	// publishes the binding when it is applied.
	if pending, err := ytIssueOpPendingTx(ctx, tx, b.TaskID); err != nil {
		return YTTaskBinding{}, 0, 0, err
	} else if pending {
		return YTTaskBinding{}, 0, 0, fmt.Errorf("%w: task %d has an unresolved issue_create op", ErrYTBindingConflict, b.TaskID)
	}
	// The publication copies under the authority the evidence was read on, so
	// a switch commits before this read (refusing) or after this transaction,
	// never between the listing and the copies.
	if p.auto {
		side, err := ytAuthorityShareTx(ctx, tx, YTAuthorityComments)
		if err != nil {
			return YTTaskBinding{}, 0, 0, err
		}
		if side != p.evidence.Authority {
			return YTTaskBinding{}, 0, 0, fmt.Errorf("%w: comments authority changed since the listing", ErrYTBindingConflict)
		}
	}
	source, err := ytBindingDisagreement(ctx, tx, b)
	if err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	if source != "" {
		return YTTaskBinding{}, 0, 0, fmt.Errorf("%w: %s disagrees with binding task %d to issue %s on %s", ErrYTBindingConflict, source, b.TaskID, b.IssueID, b.Tenant)
	}
	if s.ytBindingChecked != nil {
		s.ytBindingChecked()
	}
	now := time.Now().UTC()
	var got YTTaskBinding
	err = scanYTBinding(tx.QueryRow(ctx, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, issue_readable, project_id, bound_by, bound_at, origin, comments_from)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,
			CASE WHEN $9 < 0 THEN (SELECT coalesce(max(id),0) FROM task_comments WHERE task_id=$1) ELSE $9 END)
		ON CONFLICT DO NOTHING RETURNING `+ytBindingColumns,
		b.TaskID, b.Tenant, b.IssueID, b.IssueReadable, b.ProjectID, b.BoundBy, now, p.origin, commentsFrom), &got)
	if errors.Is(err, pgx.ErrNoRows) {
		return YTTaskBinding{}, 0, 0, fmt.Errorf("%w: issue %s on %s is bound to another task", ErrYTBindingConflict, b.IssueID, b.Tenant)
	}
	if err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	actor := p.actor
	if actor == "" {
		actor = got.BoundBy
	}
	if err := insertYTBindingHistoryTx(ctx, tx, got, ytBindingActionBound, actor, "", now); err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	if err := s.ytFieldsOnBindTx(ctx, tx, b.TaskID, now); err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	// A binding the operator published after an unbind is no task change, so
	// the task_fields feed would not see it, and the unbind rejected the
	// task's prepared snapshot and move. This process's fields mode may be off
	// (the CLI's is), so ytFieldsOnBindTx may have prepared nothing: one task
	// 'U' row, as translations.go writes for a title, makes the serve
	// worker's consumer read the task again and prepare its move and snapshot
	// for the new binding (hk 4539). An unchanged snapshot makes no op.
	if latest == ytBindingActionUnbound {
		if _, err := tx.Exec(ctx, `INSERT INTO sync_log(model,model_id,action,at) VALUES('task',$1,'U',clock_timestamp())`,
			strconv.FormatInt(b.TaskID, 10)); err != nil {
			return YTTaskBinding{}, 0, 0, err
		}
	}
	prepared, linked := 0, 0
	var enqueued []ytEnqueuedCopy
	if p.auto && p.evidence.Authority == YTAuthorityHK {
		for _, id := range p.evidence.Missing {
			var c TaskComment
			err := scanTaskComment(tx.QueryRow(ctx, `SELECT `+taskCommentColumns+` FROM task_comments WHERE id=$1`, id), &c)
			if errors.Is(err, pgx.ErrNoRows) {
				return YTTaskBinding{}, 0, 0, fmt.Errorf("evidence comment %d does not exist", id)
			}
			if err != nil {
				return YTTaskBinding{}, 0, 0, err
			}
			if c.TaskID != b.TaskID || c.Author == YouTrackOwnedActor {
				return YTTaskBinding{}, 0, 0, fmt.Errorf("evidence comment %d is not task %d's to copy", id, b.TaskID)
			}
			var linkedOp int64
			err = tx.QueryRow(ctx, `SELECT op_id FROM yt_comment_write_links WHERE comment_id=$1`, id).Scan(&linkedOp)
			switch {
			case err == nil:
				linked++
				continue
			case !errors.Is(err, pgx.ErrNoRows):
				return YTTaskBinding{}, 0, 0, err
			}
			// The binding inserted above and every source checked above are
			// held by this transaction's locks, so the enqueue cannot refuse
			// for a missing binding or a disagreement: a false here is a
			// broken invariant and rolls the publication back.
			op, ok, err := enqueueYTCommentOnTx(ctx, tx, c)
			if err != nil {
				return YTTaskBinding{}, 0, 0, err
			}
			if !ok {
				return YTTaskBinding{}, 0, 0, fmt.Errorf("comment %d got no op inside its publication for task %d", id, b.TaskID)
			}
			prepared++
			enqueued = append(enqueued, ytEnqueuedCopy{comment: c, op: op})
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return YTTaskBinding{}, 0, 0, err
	}
	for _, e := range enqueued {
		logYTCommentEnqueued(e.comment, e.op, true)
	}
	return got, prepared, linked, nil
}
