package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The read-only requeue (hk 5506). Before hk 5494 (#521) YouTrack's read-only
// maintenance answer, 418 invalid_state, rejected the op it refused (reason
// http_418), and a rejected op is terminal. Such an op provably changed
// nothing in YouTrack: the refused request was not processed. This pass
// returns each one to prepared with the same id, key and payload, so the
// normal dispatcher sends it once more. Nothing is prepared in its place, so
// the idempotency of its key and plan holds.
//
// Eligibility, all of which must hold (ytRequeueRefusal and the transaction
// below; anything else is listed not_eligible and never touched):
//   - rejected, with reason http_418 and a recorded error that is the
//     read-only answer (ytwrite.RecordedReadOnly, the match #521 uses);
//   - of a kind whose refusal is its only write (ytRequeueKinds), so no
//     earlier write of the op can have taken effect and be sent twice;
//   - a payload still held (a redacted payload would send an empty plan);
//   - never reconciled (no evidence search has ruled on an earlier send);
//   - for a comment op, the comment's link still names this op (a copy made
//     by RequeueYTMovedComments supersedes it, and sending it would duplicate
//     the comment);
//   - the worker's own admission (ytAdmitSQL) admits it now, so a binding
//     that moved, a mode that is off or a document bound elsewhere refuses
//     the op here instead of leaving it prepared and blocking its lane.
//
//   - for a root-capable article_create, its family has no other open
//     root-capable create and no uncertain root on its project
//     (ytArticleFamilyHeldTx: family_root_pending, family_root_uncertain;
//     hk 5761).
//
// Locks (lockorder.go): the entity's lane lock, the authority share of the
// op's principal, for a root-capable article_create its family create lock
// (ytArticleFamilyLock), then the op row, the order prepareYTOpTx's callers
// take. A dry run takes none and writes nothing.

// ytRequeueKinds are the kinds the requeue may return to prepared. Their
// refusal comes before any write the op makes: a comment append is one POST,
// and an article create's family root and marker search are reconciled by
// the dispatch itself (searchOrCreateRoot, markedArticles). The planned kinds
// (issue_links, issue_move, article_split) send several steps, so a refusal
// can follow an applied step, and they stay rejected.
var ytRequeueKinds = []string{ytwrite.KindCommentAppend, ytwrite.KindArticleCreate}

// YTRequeueListMax bounds the ops one requeue pass lists or changes.
const YTRequeueListMax = 200

// Outcomes of one op in a requeue pass.
const (
	YTRequeueRequeued     = "requeued"
	YTRequeueWouldRequeue = "would_requeue"
	YTRequeueNotEligible  = "not_eligible"
)

// Reasons an op is not eligible. They are stable codes.
const (
	ytRequeueNoOp          = "no_such_op"
	ytRequeueNotRejected   = "not_rejected"
	ytRequeueNotReadOnly   = "not_read_only_418"
	ytRequeueKindRefused   = "kind_not_requeueable"
	ytRequeuePayloadGone   = "payload_redacted"
	ytRequeueReconciled    = "reconciled"
	ytRequeueCommentLinked = "comment_not_linked"
	ytRequeueNotAdmitted   = "not_admitted"
	ytRequeueChanged       = "changed_concurrently"
	ytRequeueOtherTenant   = "other_tenant"
)

// YTRequeueByMax is the length of the operator name a requeue records: the
// history row's by_operator (CHECK <= 200 on both dialects). A longer name is
// refused before any op is touched.
const YTRequeueByMax = 200

// YTRequeueOptions selects the ops of one pass and says whether it applies.
type YTRequeueOptions struct {
	// OpIDs names the ops to consider, in order. When empty the pass lists
	// the rejected ops after AfterID, oldest first, up to Limit.
	OpIDs   []int64
	AfterID int64
	Limit   int
	// By names the operator who requeues the ops; it is recorded with each
	// requeue. Required when Apply is set.
	By string
	// Tenant is the serving tenant. Only ops of this tenant are considered
	// (the dispatcher admits a tenant's ops only), and the admission check is
	// bound to it, not to the op's own tenant.
	Tenant string
	Apply  bool
}

// YTRequeueItem is the answer for one op: its outcome and, when it is not
// eligible, the reason.
type YTRequeueItem struct {
	OpID       int64  `json:"op_id"`
	Kind       string `json:"kind,omitempty"`
	EntityKind string `json:"entity_kind,omitempty"`
	EntityID   string `json:"entity_id,omitempty"`
	Outcome    string `json:"outcome"`
	Reason     string `json:"reason,omitempty"`
}

// YTRequeueReport is one pass: every op it considered, in order, and the id to
// pass as AfterID for the next page when the listing was full.
type YTRequeueReport struct {
	Apply       bool            `json:"apply"`
	Ops         []YTRequeueItem `json:"ops"`
	NextAfterID *int64          `json:"next_after_id"`
}

// ytRequeueRefusal is the eligibility of an op that was read under the lane
// lock (or without one, for a dry run), as far as its own row says: "" when
// the row qualifies, else the reason. It reads no other table; the link and
// the admission are checked by the transaction.
func ytRequeueRefusal(op ytwrite.Op) string {
	switch {
	case op.State != ytwrite.StateRejected:
		return ytRequeueNotRejected
	case !ytwrite.RecordedReadOnly(op.Reason, op.LastError):
		return ytRequeueNotReadOnly
	case !slices.Contains(ytRequeueKinds, op.Kind):
		return ytRequeueKindRefused
	case len(op.Payload) == 0:
		return ytRequeuePayloadGone
	case op.Reconciles != 0:
		return ytRequeueReconciled
	}
	return ""
}

// RequeueYTReadOnlyOps is the operator's pass (hk 5506, `youtrack ops
// requeue`): for each selected op it either reports what it would do (Apply
// false: nothing is written) or requeues it (Apply true). Every other op is
// reported not_eligible with its reason and is not changed. A second pass over
// the same ops requeues nothing: a requeued op is prepared, not rejected.
func (s *Store) RequeueYTReadOnlyOps(ctx context.Context, now time.Time, opts YTRequeueOptions) (YTRequeueReport, error) {
	out := YTRequeueReport{Apply: opts.Apply, Ops: []YTRequeueItem{}}
	by := strings.TrimSpace(opts.By)
	if opts.Apply && by == "" {
		return out, errors.New("requeue: --by names the operator who requeues the ops")
	}
	if len(by) > YTRequeueByMax {
		return out, fmt.Errorf("requeue: --by is longer than %d characters", YTRequeueByMax)
	}
	if strings.TrimSpace(opts.Tenant) == "" {
		return out, errors.New("requeue: the serving tenant is required")
	}
	ids, next, err := s.selectYTRequeueOps(ctx, opts)
	if err != nil {
		return out, err
	}
	out.NextAfterID = next
	for _, id := range ids {
		item, err := s.requeueYTReadOnlyOp(ctx, now, id, by, opts.Tenant, opts.Apply)
		if err != nil {
			return out, err
		}
		out.Ops = append(out.Ops, item)
	}
	return out, nil
}

// selectYTRequeueOps lists the op ids of a pass: the named ids in order, or
// the next page of rejected ops after AfterID. next is the id to continue
// from when the page is full, else nil.
func (s *Store) selectYTRequeueOps(ctx context.Context, opts YTRequeueOptions) ([]int64, *int64, error) {
	if len(opts.OpIDs) > 0 {
		seen := map[int64]bool{}
		ids := []int64{}
		for _, id := range opts.OpIDs {
			if id < 1 {
				return nil, nil, errors.New("requeue: op ids are positive")
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if len(ids) > YTRequeueListMax {
			return nil, nil, errors.New("requeue: too many --op ids for one pass")
		}
		return ids, nil, nil
	}
	limit := opts.Limit
	if limit < 1 || limit > YTRequeueListMax {
		limit = YTRequeueListMax
	}
	rows, err := s.db().Query(ctx, s.q(
		`SELECT id FROM yt_write_ops WHERE state='rejected' AND id > $1 ORDER BY id LIMIT $2`,
		`SELECT id FROM yt_write_ops WHERE state='rejected' AND id > ? ORDER BY id LIMIT ?`), opts.AfterID, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(ids) == limit {
		last := ids[len(ids)-1]
		return ids, &last, nil
	}
	return ids, nil, nil
}

// requeueYTReadOnlyOp decides one op in its own transaction, retried on a
// transaction conflict like the other ledger writes.
func (s *Store) requeueYTReadOnlyOp(ctx context.Context, now time.Time, id int64, by, tenant string, apply bool) (YTRequeueItem, error) {
	var item YTRequeueItem
	err := s.retryTx(ctx, func() error {
		var err error
		item, err = s.requeueYTReadOnlyOpOnce(ctx, now, id, by, tenant, apply)
		return err
	})
	return item, err
}

// requeueYTReadOnlyOpOnce is one attempt of requeueYTReadOnlyOp.
func (s *Store) requeueYTReadOnlyOpOnce(ctx context.Context, now time.Time, id int64, by, tenant string, apply bool) (YTRequeueItem, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return YTRequeueItem{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	op, found, err := readYTOpRowTx(ctx, tx, id, false)
	if err != nil {
		return YTRequeueItem{}, err
	}
	if !found {
		return ytRequeueNotEligibleItem(ytwrite.Op{ID: id}, ytRequeueNoOp), nil
	}
	if apply {
		if err := lockYTEntity(ctx, tx, op.EntityKind, op.EntityID); err != nil {
			return YTRequeueItem{}, err
		}
		if err := lockYTAuthorityForOpTx(ctx, tx, op.Principal); err != nil {
			return YTRequeueItem{}, err
		}
		// A root-capable create takes its family's create lock, as its
		// preparation did (hk 5761): the family check below and the return to
		// prepared are atomic against another document's create. The family
		// is the payload's, which no transition changes.
		if family := ytRequeueFamily(op); family != "" {
			if err := lockYTEntity(ctx, tx, ytArticleFamilyLock, op.Tenant+"\x1f"+op.YTTargetID+"\x1f"+family); err != nil {
				return YTRequeueItem{}, err
			}
		}
		// Read again under the locks: the op may have moved since the first read.
		op, found, err = readYTOpRowTx(ctx, tx, id, true)
		if err != nil {
			return YTRequeueItem{}, err
		}
		if !found {
			return ytRequeueNotEligibleItem(ytwrite.Op{ID: id}, ytRequeueNoOp), nil
		}
	}
	if op.Tenant != tenant {
		return ytRequeueNotEligibleItem(op, ytRequeueOtherTenant), nil
	}
	if reason := ytRequeueRefusal(op); reason != "" {
		return ytRequeueNotEligibleItem(op, reason), nil
	}
	if reason, err := s.ytRequeueAdmissionTx(ctx, tx, op, tenant); err != nil {
		return YTRequeueItem{}, err
	} else if reason != "" {
		return ytRequeueNotEligibleItem(op, reason), nil
	}
	if family := ytRequeueFamily(op); family != "" {
		// One root-capable create per tenant, project and family until its
		// root is recorded (S1): an op revived beside another document's open
		// create, or behind an uncertain one, could make a second root.
		held, err := ytArticleFamilyHeldTx(ctx, tx, op.Tenant, op.YTTargetID, family, true)
		if err != nil {
			return YTRequeueItem{}, err
		}
		if held != "" {
			return ytRequeueNotEligibleItem(op, string(held)), nil
		}
	}
	if !apply {
		return ytRequeueItem(op, YTRequeueWouldRequeue, ""), nil
	}
	// The history row first, with what the op held, then the op: a
	// compare-and-set on the row that was read, so a second caller moves it
	// once and an op that changed since is left alone.
	if _, err := tx.Exec(ctx, sqlFor(tx,
		`INSERT INTO yt_write_op_requeues(op_id, requeued_at, by_operator, prior_attempts, prior_reason, prior_last_error, prior_resolved_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		`INSERT INTO yt_write_op_requeues(op_id, requeued_at, by_operator, prior_attempts, prior_reason, prior_last_error, prior_resolved_at)
		VALUES (?,?,?,?,?,?,?)`),
		id, now.UTC(), by, op.Attempts, op.Reason, op.LastError, op.ResolvedAt.UTC()); err != nil {
		return YTRequeueItem{}, err
	}
	n, err := tx.Exec(ctx, sqlFor(tx,
		`UPDATE yt_write_ops SET state='prepared', reason='', last_error='', attempts=0, dispatched_at=NULL, resolved_at=NULL, next_attempt_at=$2
		WHERE id=$1 AND state='rejected' AND reason='http_418' AND attempts=$3`,
		`UPDATE yt_write_ops SET state='prepared', reason='', last_error='', attempts=0, dispatched_at=NULL, resolved_at=NULL, next_attempt_at=?
		WHERE id=? AND state='rejected' AND reason='http_418' AND attempts=?`),
		ytRequeueUpdateArgs(tx, id, now, op.Attempts)...)
	if err != nil {
		return YTRequeueItem{}, err
	}
	if n != 1 {
		return ytRequeueNotEligibleItem(op, ytRequeueChanged), nil
	}
	if err := tx.Commit(ctx); err != nil {
		return YTRequeueItem{}, err
	}
	log.Printf("ytwrite.read_only_requeued op_id=%d kind=%s entity_kind=%s entity_id=%q by=%q", id, op.Kind, op.EntityKind, op.EntityID, by)
	return ytRequeueItem(op, YTRequeueRequeued, ""), nil
}

// ytRequeueFamily is the family of op when it is a documents article_create
// that can make a family root (its payload names a parent_family), else "".
// An unreadable payload names none: ytRequeueRefusal and the admission decide
// such an op.
func ytRequeueFamily(op ytwrite.Op) string {
	if op.Kind != ytwrite.KindArticleCreate || op.Principal != YTDocsPrincipal || len(op.Payload) == 0 {
		return ""
	}
	var p ytwrite.ArticleCreatePayload
	if json.Unmarshal(op.Payload, &p) != nil {
		return ""
	}
	return p.ParentFamily
}

// ytRequeueLinkRequired reports whether op must still be the one its task
// comment's yt_comment_write_links row names. Only the task-comment principal
// writes those rows; a document-copy comment (hk:doc-comments) has none, and a
// copy made by RequeueYTMovedComments supersedes only task-comment ops.
func ytRequeueLinkRequired(op ytwrite.Op) bool {
	return op.Kind == ytwrite.KindCommentAppend && op.Principal == YTCommentPrincipal
}

// ytRequeueAdmissionTx is the non-empty reason when the worker's own admission
// (ytAdmitSQL, the same SQL Due and Claim use, bound to the serving tenant) does
// not admit op now, or the task comment's link no longer names it; "" when the
// op can be sent.
func (s *Store) ytRequeueAdmissionTx(ctx context.Context, tx transaction, op ytwrite.Op, tenant string) (string, error) {
	if ytRequeueLinkRequired(op) {
		var comment int64
		err := tx.QueryRow(ctx, sqlFor(tx, `SELECT comment_id FROM yt_comment_write_links WHERE op_id=$1`,
			`SELECT comment_id FROM yt_comment_write_links WHERE op_id=?`), op.ID).Scan(&comment)
		if errors.Is(err, ErrNoRows) {
			return ytRequeueCommentLinked, nil
		}
		if err != nil {
			return "", err
		}
	}
	var one int
	err := tx.QueryRow(ctx, sqlFor(tx,
		`SELECT 1 FROM yt_write_ops o WHERE o.id=$1 AND `+s.ytAdmitSQL("$2"),
		`SELECT 1 FROM yt_write_ops o WHERE o.id=? AND `+s.ytAdmitSQL("?")), op.ID, tenant).Scan(&one)
	if errors.Is(err, ErrNoRows) {
		return ytRequeueNotAdmitted, nil
	}
	if err != nil {
		return "", err
	}
	return "", nil
}

// readYTOpRowTx reads one op inside the caller's transaction; forUpdate takes
// its row lock, as the requeue's apply path needs.
func readYTOpRowTx(ctx context.Context, tx transaction, id int64, forUpdate bool) (ytwrite.Op, bool, error) {
	q := `SELECT ` + ytOpColumns + ` FROM yt_write_ops WHERE id=$1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	my := `SELECT ` + ytOpColumns + ` FROM yt_write_ops WHERE id=?`
	if forUpdate {
		my += ` FOR UPDATE`
	}
	op, err := scanYTOp(tx.QueryRow(ctx, sqlFor(tx, q, my), id))
	if errors.Is(err, ErrNoRows) {
		return ytwrite.Op{}, false, nil
	}
	if err != nil {
		return ytwrite.Op{}, false, err
	}
	return op, true, nil
}

// ytRequeueUpdateArgs orders the requeue's update arguments: PostgreSQL
// numbers them as they appear ($1 id, $2 next_attempt_at, $3 attempts), while
// MySQL takes the SET value first, as ytRequeueLinkArgs does.
func ytRequeueUpdateArgs(tx transaction, id int64, now time.Time, attempts int) []any {
	if dialectFor(tx) == mysqlDialect {
		return []any{now.UTC(), id, attempts}
	}
	return []any{id, now.UTC(), attempts}
}

func ytRequeueItem(op ytwrite.Op, outcome, reason string) YTRequeueItem {
	return YTRequeueItem{OpID: op.ID, Kind: op.Kind, EntityKind: op.EntityKind, EntityID: op.EntityID, Outcome: outcome, Reason: reason}
}

func ytRequeueNotEligibleItem(op ytwrite.Op, reason string) YTRequeueItem {
	return ytRequeueItem(op, YTRequeueNotEligible, reason)
}
