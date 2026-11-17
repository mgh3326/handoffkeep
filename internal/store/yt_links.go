package store

import (
	"cmp"
	"context"
	"errors"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// YouTrack issue-link projection (hk 3841, design/2026-11-13/yt-issue-links).
// This file holds the store side of slices S2 and S4: which task writes each
// link (I2), the set of links a writer wants (§5.1), the mode and gate, the
// change-feed consumer task_links (§5.2) and the bind hook (§5.3). The sweep
// is yt_links_sweep.go. The mode stays off until the operator surface and the
// canary (S5) exist.

// YTLinksMode gates the issue-link projection (HANDOFFKEEP_YT_LINKS_MODE).
type YTLinksMode string

const (
	// YTLinksOff prepares no issue_links op and admits none (the default).
	// The consumer's cursor still advances, and every pass that reads a
	// change moves the sweep generation (I13), so a change made while off
	// reaches YouTrack through the sweep once the mode is on.
	YTLinksOff YTLinksMode = "off"
	// YTLinksOn prepares an issue_links op for each changed task of the
	// feed, for the sweep's walk and for the dependents of a new binding, and
	// admits the ops whose target is the writer's bound issue.
	YTLinksOn YTLinksMode = "on"
)

// YTLinksPrincipal owns (and is the actor of) the issue_links ops; their key
// is SnapshotKey(kind, task id, seq, sha).
const YTLinksPrincipal = "hk:task-links"

// ytLinksConsumer is the change-feed consumer's name (its cursor's key).
const ytLinksConsumer = "task_links"

type ytLinksGate struct {
	mode   YTLinksMode
	tenant string
}

// SetYTLinksMode sets the gate for this process and the YouTrack tenant whose
// bindings it writes to. serve calls it once at start. On with an empty
// tenant is off.
func (s *Store) SetYTLinksMode(m YTLinksMode, tenant string) {
	s.ytLinks.Store(&ytLinksGate{mode: m, tenant: tenant})
}

// ytLinksNow is the gate: on and its tenant, or off.
func (s *Store) ytLinksNow() (YTLinksMode, string) {
	g := s.ytLinks.Load()
	if g == nil || g.mode != YTLinksOn || g.tenant == "" {
		return YTLinksOff, ""
	}
	return YTLinksOn, g.tenant
}

func init() {
	registerYTKindGate(ytwrite.KindIssueLinks, ytLinksGateSQL)
	registerYTFeedStoreConsumer(ytLinksConsumer, []string{"task"}, ytLinksHandler)
	// The first pass of the consumer creates its cursor at the feed's head;
	// history before it is the sweep's (I13).
	c := ytFeedConsumers[ytLinksConsumer]
	c.created = bumpYTLinksSweepTx
	ytFeedConsumers[ytLinksConsumer] = c
}

// ytLinksGateSQL is the issue_links gate (ytAdmitSQL): mode on, and o's
// target is the issue o's writer task is bound to on o's tenant now. An op
// whose task was rebound stays prepared until the held-op recovery closes it.
// Mode off admits nothing. The claim adds no link-specific check: ownership
// is the plan's job (yt_link_plans.go).
func ytLinksGateSQL(s *Store) string {
	if mode, _ := s.ytLinksNow(); mode != YTLinksOn {
		return `FALSE`
	}
	return `o.entity_kind = 'task' AND EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)`
}

// ytLinksTargetsSQL lists the link targets an issue_links op o names, one
// jsonb object {task_id, issue} per row in column t: the payload's parent,
// related and duplicate entries (rows whose t is not an object are noise and
// filtered by the callers).
const ytLinksTargetsSQL = `(SELECT o.payload->'desired'->'parent' AS t
		UNION ALL SELECT jsonb_array_elements(CASE WHEN jsonb_typeof(o.payload->'desired'->'related') = 'array'
			THEN o.payload->'desired'->'related' ELSE '[]'::jsonb END)
		UNION ALL SELECT jsonb_array_elements(CASE WHEN jsonb_typeof(o.payload->'desired'->'duplicates') = 'array'
			THEN o.payload->'desired'->'duplicates' ELSE '[]'::jsonb END))`

// ytLinksStaleSQL is the one staleness rule of a prepared issue_links op o
// (hk 4711): o is stale once any link target it names (ytLinksTargetsSQL) is
// no longer that task's binding on o's tenant, because the target was unbound
// (and maybe bound again elsewhere) after o was prepared. Written as it
// stands, o would add a link to an issue the target task no longer owns, so a
// stale op is never sent: Claim rejects it under the writer's lane lock
// (claimYTLinksTargetsTx), and the writer's next snapshot rejects the ones
// still queued before it is prepared (rejectStaleYTLinksOpsTx). The only
// writer that takes an issue away from a task is UnbindYTTaskBinding's
// DELETE; Claim holds the target binding rows FOR KEY SHARE across its check
// and its dispatch, and the unbind refuses while a writer's op that names the
// issue is in flight (ytLinksInFlightToTx), so a claim and an unbind are
// ordered either way. o's own target is not part of this rule: the gate
// checks it, and the held-op recovery closes an op whose own binding moved.
const ytLinksStaleSQL = `EXISTS (SELECT 1 FROM ` + ytLinksTargetsSQL + ` n
	WHERE jsonb_typeof(n.t) = 'object' AND NOT EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = n.t->>'task_id' AND b.issue_id = n.t->>'issue' AND b.tenant = o.tenant))`

// claimYTLinksTargetsTx is Claim's check of an issue_links op (ytLinksStaleSQL).
// It first takes FOR KEY SHARE on the binding rows of the op's targets, in
// task id order, and holds them until the claim commits: an unbind's DELETE of
// one of them either committed before (the check sees it) or waits for this
// claim and then finds the op dispatched and refuses (ytLinksInFlightToTx).
// A stale op is rejected with ReasonLinkTargetChanged, never dispatched, and
// the writer gets a task 'U' feed row in the same transaction so its next
// snapshot is prepared from the current bindings: without it, a stale op
// whose writer nothing else changes would leave its other link changes
// unsent. The claim holds the writer's lane lock and the op row.
func claimYTLinksTargetsTx(ctx context.Context, tx pgx.Tx, op ytwrite.Op) (reject, detail string, err error) {
	if op.Kind != ytwrite.KindIssueLinks {
		return "", "", nil
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM yt_task_bindings b WHERE b.task_id IN (
			SELECT (n.t->>'task_id')::bigint FROM yt_write_ops o, LATERAL `+ytLinksTargetsSQL+` n
			WHERE o.id = $1 AND jsonb_typeof(n.t) = 'object' AND n.t->>'task_id' ~ '^[0-9]{1,18}$')
		ORDER BY b.task_id FOR KEY SHARE OF b`, op.ID); err != nil {
		return "", "", err
	}
	var stale bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM yt_write_ops o WHERE o.id = $1 AND `+ytLinksStaleSQL+`)`, op.ID).Scan(&stale); err != nil {
		return "", "", err
	}
	if !stale {
		return "", "", nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sync_log(model,model_id,action,at) VALUES ('task', $1, 'U', clock_timestamp())`, op.EntityID); err != nil {
		return "", "", err
	}
	log.Printf("ytwrite.links_target_changed op_id=%d task_id=%s issue_id=%q", op.ID, op.EntityID, op.YTTargetID)
	return ytwrite.ReasonLinkTargetChanged, "a link target of this snapshot is no longer bound to the issue it names", nil
}

// ytLinksInFlightToTx lists the issue_links ops of other tasks on tenant that
// name task id bound to issue as a link target and may be writing it now
// (dispatched, remote_unknown). UnbindYTTaskBinding refuses while there are
// any, as it does for the task's own uncertain ops. It runs after the
// unbind's DELETE of the binding row, so a claim that held the row FOR KEY
// SHARE has committed and its op is seen here, and a later claim waits for
// the unbind and then sees no binding.
func ytLinksInFlightToTx(ctx context.Context, tx pgx.Tx, tenant string, id int64, issue string) ([]ytLaneOp, error) {
	rows, err := tx.Query(ctx, `SELECT o.id, o.kind FROM yt_write_ops o
		WHERE o.kind = $1 AND o.tenant = $2 AND o.entity_kind = 'task' AND o.entity_id <> $3::text
			AND o.state IN ('dispatched','remote_unknown')
			AND EXISTS (SELECT 1 FROM `+ytLinksTargetsSQL+` n
				WHERE jsonb_typeof(n.t) = 'object' AND n.t->>'task_id' = $3::text AND n.t->>'issue' = $4)
		ORDER BY o.id`, ytwrite.KindIssueLinks, tenant, strconv.FormatInt(id, 10), issue)
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

// rejectStaleYTLinksOpsTx rejects the prepared issue_links ops of task key on
// tenant that ytLinksStaleSQL calls stale, so the snapshot prepared next is
// not queued behind an op the claim would only reject. The caller holds the
// task's lane lock; the rows are the task's own yt_write_ops rows, which a
// lane holder updates after the lock (lockorder.go).
func rejectStaleYTLinksOpsTx(ctx context.Context, tx pgx.Tx, now time.Time, tenant, taskKey string) error {
	rows, err := tx.Query(ctx, `UPDATE yt_write_ops o SET state='rejected', reason=$4, last_error=$5, resolved_at=$6
		WHERE o.entity_kind='task' AND o.entity_id=$1 AND o.state='prepared' AND o.tenant=$2 AND o.principal=$3 AND o.kind=$7
			AND `+ytLinksStaleSQL+`
		RETURNING o.id`, taskKey, tenant, YTLinksPrincipal, ytwrite.ReasonLinkTargetChanged,
		"a link target of this snapshot is no longer bound to the issue it names", now.UTC(), ytwrite.KindIssueLinks)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, id := range ids {
		log.Printf("ytwrite.links_target_changed op_id=%d task_id=%s", id, taskKey)
	}
	return nil
}

// ytLinkKindParent names a parent edge in a desired or held set; the other
// two kinds are the relation kinds RelationRelated and RelationDuplicateOf.
// blocks is not projected.
const ytLinkKindParent = "parent"

// ytLinkTarget is one link a writer wants: the hk task at the other end and
// that task's bound issue on the tenant.
type ytLinkTarget struct {
	TaskID  int64
	IssueID string
}

// ytLinkHeld is an edge whose target task has no binding on the tenant. It is
// neither written nor removed until the target is bound (I14).
type ytLinkHeld struct {
	Kind   string
	TaskID int64
}

// ytLinksDesired is the set of links one writer task wants on its own issue,
// built from current rows. Parent is nil when the task has no parent or the
// parent is held. Slices are sorted by task id and never nil.
type ytLinksDesired struct {
	Parent     *ytLinkTarget
	Related    []ytLinkTarget
	Duplicates []ytLinkTarget
	Held       []ytLinkHeld
}

// ytLinkWriter returns the one task that writes the YouTrack link of an hk
// edge (I2): the child for a parent edge, the duplicate (src) for
// duplicate_of, and the lower id for related, which is the stored src_id
// (normalizeRelation). Two lanes therefore never mutate one link. ok is false
// for a kind that is not projected (blocks).
func ytLinkWriter(kind string, src, dst int64) (writer int64, ok bool) {
	switch kind {
	case ytLinkKindParent, RelationDuplicateOf:
		// For a parent edge src is the child.
		return src, true
	case RelationRelated:
		return min(src, dst), true
	}
	return 0, false
}

// ytLinksDesiredTx builds the desired set of writer task id on tenant: its
// parent, the live related edges where it is src_id (the lower id), and its
// live duplicate_of edges. An edge whose target has no binding on tenant goes
// to Held. found is false when the task does not exist. The caller holds the
// task's lane lock, so the set and the op prepared from it agree.
func ytLinksDesiredTx(ctx context.Context, q pgx.Tx, tenant string, id int64) (ytLinksDesired, bool, error) {
	out := ytLinksDesired{Related: []ytLinkTarget{}, Duplicates: []ytLinkTarget{}, Held: []ytLinkHeld{}}
	var parentID *int64
	var parentIssue *string
	err := q.QueryRow(ctx, `SELECT t.parent_id, b.issue_id FROM tasks t
		LEFT JOIN yt_task_bindings b ON b.task_id = t.parent_id AND b.tenant = $2
		WHERE t.id = $1`, id, tenant).Scan(&parentID, &parentIssue)
	if errors.Is(err, ErrNoRows) {
		return ytLinksDesired{}, false, nil
	}
	if err != nil {
		return ytLinksDesired{}, false, err
	}
	if parentID != nil {
		if parentIssue == nil {
			out.Held = append(out.Held, ytLinkHeld{Kind: ytLinkKindParent, TaskID: *parentID})
		} else {
			out.Parent = &ytLinkTarget{TaskID: *parentID, IssueID: *parentIssue}
		}
	}
	// Only edges this task writes: related is stored src<dst, so src_id is the
	// writer, and duplicate_of is written by its src.
	rows, err := q.Query(ctx, `SELECT r.kind, r.dst_id, b.issue_id FROM task_relations r
		LEFT JOIN yt_task_bindings b ON b.task_id = r.dst_id AND b.tenant = $2
		WHERE r.src_id = $1 AND r.archived_at IS NULL AND r.kind IN ('related','duplicate_of')
		ORDER BY r.kind, r.dst_id, r.id`, id, tenant)
	if err != nil {
		return ytLinksDesired{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var dst int64
		var issue *string
		if err := rows.Scan(&kind, &dst, &issue); err != nil {
			return ytLinksDesired{}, false, err
		}
		if issue == nil {
			out.Held = append(out.Held, ytLinkHeld{Kind: kind, TaskID: dst})
			continue
		}
		target := ytLinkTarget{TaskID: dst, IssueID: *issue}
		switch kind {
		case RelationRelated:
			out.Related = append(out.Related, target)
		case RelationDuplicateOf:
			out.Duplicates = append(out.Duplicates, target)
		}
	}
	if err := rows.Err(); err != nil {
		return ytLinksDesired{}, false, err
	}
	slices.SortFunc(out.Held, func(a, b ytLinkHeld) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), cmp.Compare(a.TaskID, b.TaskID))
	})
	return out, true, nil
}

// ytLinkWritersOfTx returns, ascending and without repeats, the tasks whose
// desired set points at task id through a live edge: its children, the
// sources of duplicate_of edges into it, and the src of each related edge
// where it is dst_id (the lower id writes). Binding id makes their held edge
// writable (I14), so the bind hook prepares each of them; id itself is not in
// the result.
func ytLinkWritersOfTx(ctx context.Context, q pgx.Tx, id int64) ([]int64, error) {
	rows, err := q.Query(ctx, `SELECT id FROM tasks WHERE parent_id = $1
		UNION
		SELECT src_id FROM task_relations
		WHERE dst_id = $1 AND archived_at IS NULL AND kind IN ('related','duplicate_of')
		ORDER BY 1`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// ytLinksOutcome is what prepareYTLinksTx did with one task.
type ytLinksOutcome string

const (
	ytLinksPrepared    ytLinksOutcome = "prepared"
	ytLinksUnchanged   ytLinksOutcome = "unchanged"
	ytLinksDeleted     ytLinksOutcome = "deleted"
	ytLinksUnbound     ytLinksOutcome = "unbound"
	ytLinksOtherTenant ytLinksOutcome = "other_tenant"
	ytLinksDisagrees   ytLinksOutcome = "disagrees"
	ytLinksInvalid     ytLinksOutcome = "invalid"
)

// ytLinksRequest is the op request of writer id bound to issue on tenant for
// the desired set d.
func ytLinksRequest(tenant, issue string, id int64, d ytLinksDesired) ytwrite.IssueLinksRequest {
	target := func(in []ytLinkTarget) []ytwrite.IssueLinkTarget {
		out := make([]ytwrite.IssueLinkTarget, 0, len(in))
		for _, t := range in {
			out = append(out, ytwrite.IssueLinkTarget{TaskID: t.TaskID, IssueID: t.IssueID})
		}
		return out
	}
	req := ytwrite.IssueLinksRequest{Principal: YTLinksPrincipal, Actor: YTLinksPrincipal, Tenant: tenant, IssueID: issue, TaskID: id,
		Desired: ytwrite.IssueLinksDesired{Related: target(d.Related), Duplicates: target(d.Duplicates)},
		Held:    make([]ytwrite.IssueLinkHeld, 0, len(d.Held))}
	if d.Parent != nil {
		req.Desired.Parent = &ytwrite.IssueLinkTarget{TaskID: d.Parent.TaskID, IssueID: d.Parent.IssueID}
	}
	for _, h := range d.Held {
		req.Held = append(req.Held, ytwrite.IssueLinkHeld{Kind: h.Kind, TaskID: h.TaskID})
	}
	return req
}

// prepareYTLinksTx decides one task's links snapshot in tx at now, for the
// worker of tenant, as the three callers share it (the feed, the sweep and a
// new binding's dependents all end here, so one rule decides every op). The
// task's lane lock is taken first and the binding, its sources and the
// desired set are read under it, so the op prepared from them agrees with
// them. A task that is not bound on this tenant, or whose binding a local
// source disagrees with, is skipped (the rules of the fields snapshot); every
// other task prepares through prepareYTSnapshotTx, which makes no op when the
// desired set is the one last prepared and that op was not rejected.
func prepareYTLinksTx(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, id int64) (ytLinksOutcome, error) {
	if err := lockYTEntity(ctx, tx, "task", strconv.FormatInt(id, 10)); err != nil {
		return "", err
	}
	b, found, err := readYTBinding(ctx, tx, id)
	if err != nil {
		return "", err
	}
	switch {
	case !found:
		return ytLinksUnbound, nil
	case b.Tenant != tenant:
		return ytLinksOtherTenant, nil
	}
	if source, err := ytBindingDisagreement(ctx, tx, b); err != nil {
		return "", err
	} else if source != "" {
		log.Printf("ytwrite.links_skipped task_id=%d reason=binding_disagreement source=%q", id, source)
		return ytLinksDisagrees, nil
	}
	if err := rejectStaleYTLinksOpsTx(ctx, tx, now, tenant, strconv.FormatInt(id, 10)); err != nil {
		return "", err
	}
	d, exists, err := ytLinksDesiredTx(ctx, tx, tenant, id)
	if err != nil {
		return "", err
	}
	if !exists {
		return ytLinksDeleted, nil
	}
	n, err := ytwrite.IssueLinksOp(ytLinksRequest(tenant, b.IssueID, id, d))
	if errors.Is(err, ytwrite.ErrInvalid) {
		// Bad data must not stop the feed: the cursor would never pass this
		// row, and the sweep would stop at this task.
		log.Printf("ytwrite.links_skipped task_id=%d reason=invalid err=%q", id, err.Error())
		return ytLinksInvalid, nil
	}
	if err != nil {
		return "", err
	}
	op, made, err := prepareYTSnapshotTx(ctx, tx, now, n, n.PayloadSHA)
	if err != nil {
		return "", err
	}
	if !made {
		return ytLinksUnchanged, nil
	}
	log.Printf("ytwrite.links_enqueued op_id=%d task_id=%d issue_id=%q", op.ID, id, b.IssueID)
	return ytLinksPrepared, nil
}

// ytLinksHandler is the task_links consumer's handler for one pass at now.
// Mode off prepares nothing but moves the sweep generation (I13): the cursor
// advances past rows nothing handled, and the sweep catches them up when the
// mode is on. Otherwise each changed task, in ascending id order (so two
// passes that overlap in time take lane locks in one order), goes through
// prepareYTLinksTx.
func ytLinksHandler(s *Store, now time.Time) FeedHandler {
	return func(ctx context.Context, tx pgx.Tx, rows []FeedRow) error {
		mode, tenant := s.ytLinksNow()
		if mode != YTLinksOn {
			return bumpYTLinksSweepTx(ctx, tx, now)
		}
		ids := make([]int64, 0, len(rows))
		for _, r := range rows {
			if id, err := strconv.ParseInt(r.ModelID, 10, 64); err == nil && id > 0 && r.Model == "task" {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)
		counts := map[ytLinksOutcome]int{}
		for _, id := range ids {
			outcome, err := prepareYTLinksTx(ctx, tx, now, tenant, id)
			if err != nil {
				return err
			}
			counts[outcome]++
		}
		log.Printf("ytwrite.links_feed tasks=%d prepared=%d unchanged=%d deleted=%d unbound=%d other_tenant=%d disagrees=%d invalid=%d",
			len(ids), counts[ytLinksPrepared], counts[ytLinksUnchanged], counts[ytLinksDeleted], counts[ytLinksUnbound],
			counts[ytLinksOtherTenant], counts[ytLinksDisagrees], counts[ytLinksInvalid])
		return nil
	}
}

// ytLinksOnBindTx makes the links snapshots of the tasks a new binding of
// task id affects be prepared (I14): the writers that point at id (its
// children, the duplicates of it, the lower id of each related edge into it),
// whose held edge is now writable, and id itself when it writes a link. A
// binding is no task change, so the feed would not see it; one 'U' row of the
// task feed for each affected task makes the task_links consumer read it
// again, in the binding's own transaction, so the binding and the request to
// look at its dependents commit or fail together. It writes the rows whatever
// this process's mode: the CLI that publishes a binding has the mode off, and
// the serve worker that has it on reads the rows. A task with nothing to
// write is left out, so a binding of a task with no edges writes nothing.
//
// Locks: the caller holds the bound task's lane lock; the dependents' lane
// locks are not taken, because only a feed pass may hold more than one lane
// lock (lockorder.go) and the consumer takes each one under its own pass. The
// rows take the commit-time sync allocator lock, last, as every task write
// does.
func ytLinksOnBindTx(ctx context.Context, tx pgx.Tx, taskID int64) error {
	return ytLinksWakeTx(ctx, tx, taskID, true)
}

// ytLinksOnUnbindTx is ytLinksOnBindTx for the removal of task id's binding
// (hk 4711): the writers that point at id prepared snapshots naming id's
// issue, which are stale now (ytLinksStaleSQL), and their next snapshot holds
// the edge instead. One 'U' row per writer makes the consumer prepare it, and
// that preparation rejects the stale ops first; a claim that comes before the
// consumer rejects them itself. id's own ops are the unbind's. The unbind
// does not reject the writers' ops itself: it holds id's lane lock, and a
// feed pass that holds a writer's lane lock and its op rows may be waiting
// for id's lane lock, so updating those rows there could close a cycle.
func ytLinksOnUnbindTx(ctx context.Context, tx pgx.Tx, taskID int64) error {
	return ytLinksWakeTx(ctx, tx, taskID, false)
}

// ytLinksWakeTx writes the task 'U' feed rows of ytLinksOnBindTx: one for
// each writer that points at task id, and one for id itself when self is set
// and id writes a link.
func ytLinksWakeTx(ctx context.Context, tx pgx.Tx, taskID int64, self bool) error {
	ids, err := ytLinkWritersOfTx(ctx, tx, taskID)
	if err != nil {
		return err
	}
	var writes bool
	if self {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1 AND parent_id IS NOT NULL)
		OR EXISTS (SELECT 1 FROM task_relations WHERE src_id = $1 AND archived_at IS NULL AND kind IN ('related','duplicate_of'))`,
			taskID).Scan(&writes); err != nil {
			return err
		}
	}
	if writes {
		ids = append(ids, taskID)
		slices.Sort(ids)
		ids = slices.Compact(ids)
	}
	if len(ids) == 0 {
		return nil
	}
	texts := make([]string, len(ids))
	for i, id := range ids {
		texts[i] = strconv.FormatInt(id, 10)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sync_log(model,model_id,action,at)
		SELECT 'task', u.model_id, 'U', clock_timestamp() FROM unnest($1::text[]) AS u(model_id)`, texts); err != nil {
		return err
	}
	log.Printf("ytwrite.links_wake task_id=%d self=%t tasks=%d", taskID, self, len(ids))
	return nil
}
