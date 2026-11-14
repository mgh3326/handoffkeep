package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Relation kinds. blocks and duplicate_of point from Src to Dst (Src blocks
// Dst; Src is a duplicate of Dst). related is symmetric and stored with
// Src < Dst.
const (
	RelationBlocks      = "blocks"
	RelationDuplicateOf = "duplicate_of"
	RelationRelated     = "related"
)

// TaskRelation is one live edge between two tasks. blocks is informational:
// it appears in task detail and the ledger, gates no claim, and edits no
// state. duplicate_of is the exception, tied to the duplicate state.
type TaskRelation struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	SrcID     int64     `json:"src_id"`
	DstID     int64     `json:"dst_id"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskChild is a sub-task as shown on its parent.
type TaskChild struct {
	ID       int64  `json:"id"`
	Title    string `json:"title"`
	State    string `json:"state"`
	Category string `json:"category"`
}

// RelationKinds lists the relation vocabulary.
func RelationKinds() []string {
	return []string{RelationBlocks, RelationDuplicateOf, RelationRelated}
}

func validRelationKind(kind string) bool {
	return kind == RelationBlocks || kind == RelationDuplicateOf || kind == RelationRelated
}

// SetAutoCloseParent turns the auto_close_parent setting on or off. It is on
// by default.
func (s *Store) SetAutoCloseParent(on bool) { s.autoCloseOff.Store(!on) }

// AutoCloseParent reports the auto_close_parent setting.
func (s *Store) AutoCloseParent() bool { return !s.autoCloseOff.Load() }

// normalizeRelation puts a related edge in stored order.
func normalizeRelation(kind string, src, dst int64) (int64, int64) {
	if kind == RelationRelated && src > dst {
		return dst, src
	}
	return src, dst
}

// relationReachesTx reports whether a live edge path of kind leads from
// `from` to `to`.
func relationReachesTx(ctx context.Context, q rowQuerier, kind string, from, to int64) (bool, error) {
	var reaches bool
	err := q.QueryRow(ctx, `WITH RECURSIVE reach(id) AS (
		SELECT dst_id FROM task_relations WHERE kind=$1 AND src_id=$2 AND archived_at IS NULL
		UNION
		SELECT r.dst_id FROM task_relations r JOIN reach ON r.src_id=reach.id WHERE r.kind=$1 AND r.archived_at IS NULL
	) SELECT EXISTS(SELECT 1 FROM reach WHERE id=$3)`, kind, from, to).Scan(&reaches)
	return reaches, err
}

func hasLiveRelationTx(ctx context.Context, q rowQuerier, src int64, kind string) (bool, error) {
	var live bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_relations WHERE kind=$1 AND src_id=$2 AND archived_at IS NULL)`, kind, src).Scan(&live)
	return live, err
}

// addRelationTx adds or revives one edge under the caller's transaction and
// task lock. A live edge is left alone (changed=false); an archived edge is
// revived; otherwise a row is inserted. Self-links and blocks or duplicate_of
// cycles are refused.
func addRelationTx(ctx context.Context, tx pgx.Tx, src int64, kind string, dst int64, by string, now time.Time) (bool, error) {
	if !validRelationKind(kind) || src < 1 || dst < 1 || by == "" || !validText(by, 128) {
		return false, errors.New("invalid relation")
	}
	if src == dst {
		return false, errors.New("invalid relation: a task cannot relate to itself")
	}
	src, dst = normalizeRelation(kind, src, dst)
	var locked int64
	if err := tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, src).Scan(&locked); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrTaskNotFound
		}
		return false, err
	}
	var found int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id IN ($1,$2)`, src, dst).Scan(&found); err != nil {
		return false, err
	}
	if found != 2 {
		return false, ErrTaskNotFound
	}
	var relID int64
	var archived *time.Time
	err := tx.QueryRow(ctx, `SELECT id, archived_at FROM task_relations WHERE kind=$1 AND src_id=$2 AND dst_id=$3 FOR UPDATE`, kind, src, dst).Scan(&relID, &archived)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	if exists && archived == nil {
		return false, nil
	}
	if kind != RelationRelated {
		cycle, err := relationReachesTx(ctx, tx, kind, dst, src)
		if err != nil {
			return false, err
		}
		if cycle {
			return false, fmt.Errorf("%w: %s from %d to %d would close a cycle", ErrRelationCycle, kind, src, dst)
		}
	}
	if exists {
		if _, err = tx.Exec(ctx, `UPDATE task_relations SET archived_at=NULL, archived_by='' WHERE id=$1`, relID); err != nil {
			return false, err
		}
	} else if _, err = tx.Exec(ctx, `INSERT INTO task_relations(kind,src_id,dst_id,created_by,created_at) VALUES($1,$2,$3,$4,$5)`, kind, src, dst, by, now); err != nil {
		return false, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: src, Kind: TaskEventRelation, From: "", To: "", By: by, At: now, Field: "relation", ToValue: fmt.Sprintf("%s:%d", kind, dst), Refs: TaskRefs{}}); err != nil {
		return false, err
	}
	return true, bumpRelationEndpointsTx(ctx, tx, now, src, dst)
}

// bumpRelationEndpointsTx sets updated_at on both endpoints of a changed edge,
// in the edge's own transaction, so the change reaches the task feed for
// each (design/2026-11-13/yt-issue-links I3). Only one endpoint writes the
// YouTrack link (ytLinkWriter), but bumping both keeps the rule "an edge
// change reaches both endpoints" true whichever is the writer; the other's
// snapshot is unchanged and makes no op. Every caller holds the task-mutation
// lock (lockorder.go level 2) and already holds src's row, so the second row
// lock cannot close a cycle with another task writer. updated_at never moves
// backwards: callers read now under that lock, and GREATEST keeps a newer
// revision if a clock step put one ahead of now.
func bumpRelationEndpointsTx(ctx context.Context, tx pgx.Tx, now time.Time, src, dst int64) error {
	ids := []int64{src, dst}
	slices.Sort(ids)
	_, err := tx.Exec(ctx, `UPDATE tasks SET updated_at=GREATEST(updated_at, $2) WHERE id = ANY($1)`, ids, now)
	return err
}

// archiveLiveRelationsTx archives every live edge of kind that starts at src.
func archiveLiveRelationsTx(ctx context.Context, tx pgx.Tx, src int64, kind, by string, now time.Time) error {
	rows, err := tx.Query(ctx, `UPDATE task_relations SET archived_at=$4, archived_by=$3 WHERE kind=$1 AND src_id=$2 AND archived_at IS NULL RETURNING dst_id`, kind, src, by, now)
	if err != nil {
		return err
	}
	var dsts []int64
	for rows.Next() {
		var dst int64
		if err = rows.Scan(&dst); err != nil {
			rows.Close()
			return err
		}
		dsts = append(dsts, dst)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, dst := range dsts {
		if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: src, Kind: TaskEventRelation, By: by, At: now, Field: "relation", FromValue: fmt.Sprintf("%s:%d", kind, dst), Refs: TaskRefs{}}); err != nil {
			return err
		}
		if err = bumpRelationEndpointsTx(ctx, tx, now, src, dst); err != nil {
			return err
		}
	}
	return nil
}

// AddTaskRelation adds an edge. Adding it again changes nothing, and adding
// an archived edge revives it, so repeated adds and removes converge. A
// duplicate_of edge and the duplicate state are created in one transaction:
// adding the edge moves the task to duplicate, and neither exists without the
// other.
func (s *Store) AddTaskRelation(ctx context.Context, src int64, kind string, dst int64, by string) (changed bool, err error) {
	if !validRelationKind(kind) || src < 1 || dst < 1 || by == "" || !validText(by, 128) {
		return false, errors.New("invalid relation")
	}
	if src == dst {
		return false, errors.New("invalid relation: a task cannot relate to itself")
	}
	if kind == RelationDuplicateOf {
		return s.addDuplicateOf(ctx, src, dst, by)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return false, err
	}
	changed, err = addRelationTx(ctx, tx, src, kind, dst, by, time.Now().UTC())
	if err != nil {
		return false, err
	}
	return changed, tx.Commit(ctx)
}

func (s *Store) addDuplicateOf(ctx context.Context, src, dst int64, by string) (bool, error) {
	var live bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_relations WHERE kind='duplicate_of' AND src_id=$1 AND dst_id=$2 AND archived_at IS NULL)`, src, dst).Scan(&live); err != nil {
		return false, err
	}
	if live {
		return false, nil
	}
	_, err := s.TransitionTaskWith(ctx, src, TransitionInput{To: StateDuplicate, By: by, Note: fmt.Sprintf("duplicate of #%d", dst), DuplicateOf: dst})
	return err == nil, err
}

// RemoveTaskRelation archives an edge. Removing an absent or already archived
// edge changes nothing. A duplicate task keeps its duplicate_of edge: reopen
// the task instead.
func (s *Store) RemoveTaskRelation(ctx context.Context, src int64, kind string, dst int64, by string) (changed bool, err error) {
	if !validRelationKind(kind) || src < 1 || dst < 1 || by == "" || !validText(by, 128) {
		return false, errors.New("invalid relation")
	}
	src, dst = normalizeRelation(kind, src, dst)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return false, err
	}
	var state string
	if err = tx.QueryRow(ctx, `SELECT state FROM tasks WHERE id=$1 FOR UPDATE`, src).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if kind != RelationDuplicateOf {
				return false, tx.Commit(ctx)
			}
			return false, ErrTaskNotFound
		}
		return false, err
	}
	if kind == RelationDuplicateOf {
		if state == StateDuplicate {
			return false, fmt.Errorf("%w: a duplicate task keeps its duplicate_of relation; reopen it first", ErrTaskConflict)
		}
	}
	now := time.Now().UTC()
	tag, err := tx.Exec(ctx, `UPDATE task_relations SET archived_at=$5, archived_by=$4 WHERE kind=$1 AND src_id=$2 AND dst_id=$3 AND archived_at IS NULL`, kind, src, dst, by, now)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, tx.Commit(ctx)
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: src, Kind: TaskEventRelation, By: by, At: now, Field: "relation", FromValue: fmt.Sprintf("%s:%d", kind, dst), Refs: TaskRefs{}}); err != nil {
		return false, err
	}
	if err = bumpRelationEndpointsTx(ctx, tx, now, src, dst); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// ListTaskRelations returns the live edges that touch a task, oldest first.
func (s *Store) ListTaskRelations(ctx context.Context, id int64) ([]TaskRelation, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,kind,src_id,dst_id,created_by,created_at FROM task_relations WHERE (src_id=$1 OR dst_id=$1) AND archived_at IS NULL ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskRelation{}
	for rows.Next() {
		var r TaskRelation
		if err := rows.Scan(&r.ID, &r.Kind, &r.SrcID, &r.DstID, &r.CreatedBy, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListTaskChildren returns a task's sub-tasks by id.
func (s *Store) ListTaskChildren(ctx context.Context, id int64) ([]TaskChild, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,title,state FROM tasks WHERE parent_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskChild{}
	for rows.Next() {
		var c TaskChild
		if err := rows.Scan(&c.ID, &c.Title, &c.State); err != nil {
			return nil, err
		}
		c.Category = TaskCategory(c.State)
		out = append(out, c)
	}
	return out, rows.Err()
}

// parentReachesTx reports whether `ancestor` is `id` or lies above it.
func parentChainContainsTx(ctx context.Context, q rowQuerier, start, target int64) (bool, error) {
	var found bool
	err := q.QueryRow(ctx, `WITH RECURSIVE up(id) AS (
		SELECT $1::bigint
		UNION
		SELECT t.parent_id FROM tasks t JOIN up ON t.id=up.id WHERE t.parent_id IS NOT NULL
	) SELECT EXISTS(SELECT 1 FROM up WHERE id=$2)`, start, target).Scan(&found)
	return found, err
}

// checkParentTx validates a prospective parent for a task under the caller's
// lock: it must exist, differ from the task, and not lie below it.
func checkParentTx(ctx context.Context, q rowQuerier, id, parent int64) error {
	if parent < 1 {
		return fmt.Errorf("%w: parent id must be positive", ErrTaskParentInvalid)
	}
	if parent == id {
		return fmt.Errorf("%w: a task cannot be its own parent", ErrTaskParentInvalid)
	}
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=$1)`, parent).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: task %d does not exist", ErrTaskParentInvalid, parent)
	}
	if id != 0 {
		cycle, err := parentChainContainsTx(ctx, q, parent, id)
		if err != nil {
			return err
		}
		if cycle {
			return fmt.Errorf("%w: task %d is an ancestor of task %d", ErrTaskParentCycle, id, parent)
		}
	}
	return nil
}

// SetTaskParent sets or clears (parent=nil) a task's single parent. The link
// is acyclic; a no-op change writes no event. It follows SetTaskProject's
// shape: row lock, append-only event.
func (s *Store) SetTaskParent(ctx context.Context, id int64, parent *int64, by string) (x Task, changed bool, err error) {
	if id < 1 || by == "" || !validText(by, 128) {
		return Task{}, false, errors.New("invalid task parent change")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return Task{}, false, err
	}
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, ErrTaskNotFound
		}
		return Task{}, false, err
	}
	same := (x.ParentID == nil && parent == nil) || (x.ParentID != nil && parent != nil && *x.ParentID == *parent)
	if same {
		return x, false, tx.Commit(ctx)
	}
	if parent != nil {
		if err = checkParentTx(ctx, tx, id, *parent); err != nil {
			return Task{}, false, err
		}
	}
	from, to := "", ""
	if x.ParentID != nil {
		from = fmt.Sprint(*x.ParentID)
	}
	if parent != nil {
		to = fmt.Sprint(*parent)
	}
	now := time.Now().UTC()
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET parent_id=$2,updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, id, parent, now), &x); err != nil {
		return Task{}, false, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: TaskEventParent, From: x.State, To: x.State, By: by, At: now, Refs: x.Refs, Into: &x, Field: "parent_id", FromValue: from, ToValue: to}); err != nil {
		return Task{}, false, err
	}
	return x, true, tx.Commit(ctx)
}

// afterStateChangeTx runs the hierarchy rules after a task's state changed in
// the caller's transaction: auto-close of the parent when this task settled,
// and the record left on a completed parent when this task is reopened.
func (s *Store) afterStateChangeTx(ctx context.Context, tx pgx.Tx, x Task, from, by string, now time.Time) error {
	if x.ParentID == nil {
		return nil
	}
	if IsSettledState(x.State) {
		return s.autoCloseTx(ctx, tx, x, by, now)
	}
	if IsSettledState(from) {
		var parentState string
		if err := tx.QueryRow(ctx, `SELECT state FROM tasks WHERE id=$1 FOR UPDATE`, *x.ParentID).Scan(&parentState); err != nil {
			return err
		}
		if TaskCategory(parentState) == CategoryCompleted {
			// Reopening a child records an event on a completed parent and
			// does not reopen it.
			_, _, err := insertEvent(ctx, tx, taskEventInput{
				TaskID: *x.ParentID, Kind: TaskEventAutoClose, From: parentState, To: parentState, By: "hk",
				Note: fmt.Sprintf("sub-task #%d was reopened; the parent stays %s", x.ID, parentState), At: now,
				Source: "hk.auto_close", SourceRef: fmt.Sprintf("reopen/%d@%s", x.ID, now.Format(time.RFC3339Nano)), Auto: true, NotApplied: true,
			})
			return err
		}
	}
	return nil
}

// closeGateTx is the gate an automatic close of x passes (hk 3294; the
// hygiene close of hk 3292 is to reuse it): it returns why the close must
// wait for a person, or "". x waits while one of its own implements changes is
// open or reverted and not relanded (pieceHoldTx, the last-piece rule of a
// merge), while it is awaiting a decision, or while its decision request is
// open.
func (s *Store) closeGateTx(ctx context.Context, tx pgx.Tx, x Task) (string, error) {
	if reason, err := pieceHoldTx(ctx, tx, x.ID, ""); err != nil || reason != "" {
		return reason, err
	}
	if x.AwaitingDecision {
		return "parent is awaiting a decision", nil
	}
	if x.Refs.DecisionRequest != nil && x.Refs.DecisionRequest.Status == DecisionRequestOpen {
		return "parent has an open decision request", nil
	}
	return "", nil
}

// autoCloseStateList are the parent states an automatic close applies from.
var autoCloseStateList = []string{StateBacklog, StateUnstarted, StateResearching, StateDeveloping, StateReviewing}

// autoCloseStates is autoCloseStateList as a set; autoCloseStatesSQL as a SQL list.
var (
	autoCloseStates    = states(autoCloseStateList...)
	autoCloseStatesSQL = "'" + strings.Join(autoCloseStateList, "','") + "'"
)

// autoCloseAppliesFrom reports whether an automatic close may move a parent in
// state.
func autoCloseAppliesFrom(state string) bool { return autoCloseStates[state] }

// subTaskRollup is what a parent's sub-tasks say about its close.
type subTaskRollup struct {
	settled  bool // every sub-task is settled
	worked   int  // sub-tasks that did work: completed, merged or deployed
	merged   int  // sub-tasks merged and not yet deployed
	deployed int  // sub-tasks deployed
}

// rollupStates is the rollup of the sub-task states.
func rollupStates(sub []string) subTaskRollup {
	r := subTaskRollup{settled: true}
	for _, state := range sub {
		switch state {
		case StateCompleted:
			r.worked++
		case StateMerged:
			r.worked++
			r.merged++
		case StateDeployed:
			r.worked++
			r.deployed++
		case StateDropped, StateDuplicate:
		default:
			r.settled = false
		}
	}
	return r
}

// closeTarget is the state an automatic close moves a parent to, from its
// sub-tasks: merged while one waits for its deploy, deployed when one is
// deployed and none waits (completed sub-tasks never deploy and do not wait),
// completed when only completed sub-tasks did work, and "" when the close does
// not apply (an unsettled sub-task, or none did work).
func (r subTaskRollup) closeTarget() string {
	switch {
	case !r.settled || r.worked == 0:
		return ""
	case r.merged > 0:
		return StateMerged
	case r.deployed > 0:
		return StateDeployed
	default:
		return StateCompleted
	}
}

// subTaskRollupTx reads the rollup of the sub-tasks of parentID.
func subTaskRollupTx(ctx context.Context, tx pgx.Tx, parentID int64) (subTaskRollup, error) {
	rows, err := tx.Query(ctx, `SELECT state FROM tasks WHERE parent_id=$1`, parentID)
	if err != nil {
		return subTaskRollup{}, err
	}
	sub, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return subTaskRollup{}, err
	}
	return rollupStates(sub), nil
}

// autoCloseTx runs auto_close_parent in the transaction of a child's
// transition. A child is settled when it is merged, deployed, completed, dropped or
// duplicate; a child in triage, backlog, unstarted, a started state or icebox
// blocks the close (icebox is parked work, not finished work).
//
//	any child not settled             -> no change
//	some child merged, not deployed   -> parent merged
//	some deployed, none merged        -> parent deployed (completed children do not wait)
//	only completed children           -> completed for decide, ops, verify, research;
//	                                     otherwise no change; skip recorded
//	all dropped or duplicate          -> no change; the skip is recorded
//
// The close applies to a parent in backlog, unstarted or a started state; it
// does not bypass triage and does not touch canceled or completed parents. A
// parent that closes may in turn close its own parent. A merged parent is not
// settled by its sub-tasks again until a sub-task deploys (hk 4172): then the
// same rule is applied to it, and it deploys once no sub-task waits for its
// deploy.
func (s *Store) autoCloseTx(ctx context.Context, tx pgx.Tx, child Task, by string, now time.Time) error {
	if !s.AutoCloseParent() {
		return nil
	}
	for child.ParentID != nil {
		var p Task
		if err := scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, *child.ParentID), &p); err != nil {
			return err
		}
		closed, nextP, err := s.autoCloseOneParentTx(ctx, tx, p, child.ID, child.State, now)
		if err != nil || !closed {
			return err
		}
		child = nextP
	}
	return nil
}

// autoCloseOneParentTx evaluates auto-close for parent p under its row lock.
// It returns applied=true if the parent moved, along with the updated parent.
func (s *Store) autoCloseOneParentTx(ctx context.Context, tx pgx.Tx, p Task, childID int64, childState string, now time.Time) (bool, Task, error) {
	switch {
	case p.State == StateMerged:
		// A merged parent waits for its sub-tasks to deploy: only a
		// sub-task's deploy moves it on (hk 4172).
		if childState != StateDeployed {
			return false, p, nil
		}
	case !autoCloseAppliesFrom(p.State):
		return false, p, nil
	}
	roll, err := subTaskRollupTx(ctx, tx, p.ID)
	if err != nil {
		return false, p, err
	}
	if !roll.settled {
		return false, p, nil
	}
	ref := fmt.Sprintf("hk:doc child/%d@%s", childID, now.Format(time.RFC3339Nano))
	to := roll.closeTarget()
	if to == "" {
		_, _, err = insertEvent(ctx, tx, taskEventInput{
			TaskID: p.ID, Kind: TaskEventAutoClose, From: p.State, To: p.State, By: "hk", At: now, Refs: p.Refs,
			Note:   "every sub-task is dropped or duplicate; the parent stays where it is",
			Source: "hk.auto_close", SourceRef: ref, Auto: true, NotApplied: true,
		})
		return false, p, err
	}
	if p.State == StateMerged && to != StateDeployed {
		// A sub-task is still merged and waiting for its deploy.
		return false, p, nil
	}
	if gate, err := s.closeGateTx(ctx, tx, p); err != nil || gate != "" {
		if err != nil {
			return false, p, err
		}
		_, _, err = insertEvent(ctx, tx, taskEventInput{
			TaskID: p.ID, Kind: TaskEventAutoClose, From: p.State, To: p.State, By: "hk", At: now, Refs: p.Refs,
			Note:   gate + "; the parent stays where it is",
			Source: "hk.auto_close", SourceRef: ref, Auto: true, NotApplied: true,
		})
		return false, p, err
	}
	if to == StateCompleted && !canCompleteKind(p.Kind) {
		_, _, err = insertEvent(ctx, tx, taskEventInput{
			TaskID: p.ID, Kind: TaskEventAutoClose, From: p.State, To: p.State, By: "hk", At: now, Refs: p.Refs,
			Note:   fmt.Sprintf("sub-task #%d completed; this parent kind remains open", childID),
			Source: "hk.auto_close", SourceRef: ref, Auto: true, NotApplied: true,
		})
		return false, p, err
	}
	from := p.State
	decisionBefore := decisionBeforeOf(p)
	if to == StateMerged && p.Refs.PR == "" {
		// The close is the parent's merge: it adopts a held merge's PR (hk 4172).
		if p.Refs.PR, err = heldMergePRTx(ctx, tx, p.ID); err != nil {
			return false, p, err
		}
	}
	p.State, p.UpdatedAt = to, now
	if err := applyLifecycle(&p, from, to, now, &TaskClosure{Type: "resolved", Evidence: ref}); err != nil {
		return false, p, err
	}
	if p, err = writeTaskTx(ctx, tx, p); err != nil {
		return false, p, err
	}
	if err = mirrorTaskDecisionsTx(ctx, tx, decisionBefore, p, "hk", "", 0, now); err != nil {
		return false, p, err
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{
		TaskID: p.ID, From: from, To: to, By: "hk", At: now, Refs: p.Refs,
		Note:   fmt.Sprintf("every sub-task is settled; closed by #%d", childID),
		Source: "hk.auto_close", SourceRef: ref, Auto: true,
	}); err != nil {
		return false, p, err
	}
	// Rule 8 (a deploy record lists the PR) is not applied here. A close to
	// merged always has a merged sub-task waiting for its deploy, and a
	// close to deployed was made by that deploy: neither may deploy the
	// parent past a sub-task (hk 4172).
	return true, p, nil
}
