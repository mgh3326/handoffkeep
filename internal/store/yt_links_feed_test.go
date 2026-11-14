package store

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The consumer, mode, bind hook and sweep of the issue-link projection (hk
// 4639, S4). Every test names the behaviour it pins and fails without the
// change that gives it.

// ytlfSetup is a throwaway store in links mode (on or off) with the test
// project mapped and the task_links cursor already created, so the history
// before it is not the feed's; it returns n bound tasks in id order.
func ytlfSetup(t *testing.T, prefix string, mode YTLinksMode, n int) (*Store, []int64) {
	t.Helper()
	s := schemaStore(t, prefix)
	s.SetYTLinksMode(mode, ytcTenant)
	ytbMapped(t, s)
	ids := make([]int64, 0, n)
	for range n {
		id := ytcTask(t, s, ytbProject)
		ytbBind(t, s, ytbBinding(id))
		ids = append(ids, id)
	}
	if pass := ytlfPass(t, s); !pass.Created {
		t.Fatalf("first pass %+v", pass)
	}
	return s, ids
}

func ytlfPass(t *testing.T, s *Store) YTFeedPass {
	t.Helper()
	return ytFeedPassOK(t, s, ytFeedConsumers[ytLinksConsumer])
}

// ytlfOps is the issue_links ops of a task, oldest first.
func ytlfOps(t *testing.T, s *Store, task int64) []ytwrite.Op {
	t.Helper()
	ops, err := s.YTWriteLedger().list(t.Context(), `SELECT `+ytOpColumns+` FROM yt_write_ops WHERE kind=$1 AND entity_kind='task' AND entity_id=$2 ORDER BY id`,
		ytwrite.KindIssueLinks, strconv.FormatInt(task, 10))
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

// ytlfLast is the payload of a task's newest issue_links op; it fails when
// the task has none.
func ytlfLast(t *testing.T, s *Store, task int64) ytwrite.IssueLinksSnapshot {
	t.Helper()
	ops := ytlfOps(t, s, task)
	if len(ops) == 0 {
		t.Fatalf("task %d has no issue_links op", task)
	}
	snap, err := ytwrite.IssueLinksPayload(ops[len(ops)-1])
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func ytlfTargets(ids ...int64) []ytwrite.IssueLinkTarget {
	out := []ytwrite.IssueLinkTarget{}
	for _, id := range ids {
		out = append(out, ytwrite.IssueLinkTarget{TaskID: id, IssueID: ytfIssue(id)})
	}
	return out
}

func ytlfSweepState(t *testing.T, s *Store) YTLinksSweepState {
	t.Helper()
	st, err := ytLinksSweepStateTx(t.Context(), s.pool)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Archiving a related edge with the higher id as the subject reaches the
// writer (the lower id): its next op no longer carries the link (I2, I3). The
// higher endpoint never writes the link, so its ops stay empty.
func TestYTLinksFeedRelatedEdgeHigherIdSubject(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_feed_rel", YTLinksOn, 2)
	ctx := t.Context()
	lo, hi := ids[0], ids[1]

	if changed, err := s.AddTaskRelation(ctx, hi, RelationRelated, lo, "tester"); err != nil || !changed {
		t.Fatalf("add: changed=%t err=%v", changed, err)
	}
	if pass := ytlfPass(t, s); pass.Handled != 2 {
		t.Fatalf("pass after add %+v: both endpoints reach the feed", pass)
	}
	if got := ytlfLast(t, s, lo).Desired.Related; !reflect.DeepEqual(got, ytlfTargets(hi)) {
		t.Fatalf("writer's related after add = %+v, want task %d", got, hi)
	}
	n := len(ytlfOps(t, s, lo))

	if changed, err := s.RemoveTaskRelation(ctx, hi, RelationRelated, lo, "tester"); err != nil || !changed {
		t.Fatalf("archive: changed=%t err=%v", changed, err)
	}
	ytlfPass(t, s)
	if ops := ytlfOps(t, s, lo); len(ops) != n+1 {
		t.Fatalf("archive made %d ops for the writer, want %d", len(ops), n+1)
	}
	if got := ytlfLast(t, s, lo).Desired.Related; len(got) != 0 {
		t.Fatalf("writer's related after archive = %+v, want none", got)
	}
	for _, op := range ytlfOps(t, s, hi) {
		if snap, err := ytwrite.IssueLinksPayload(op); err != nil || len(snap.Desired.Related) != 0 || len(snap.Held) != 0 {
			t.Fatalf("the higher endpoint wrote a link: %+v %v", snap, err)
		}
	}
}

// Clearing a parent reaches the child: its next op has no parent.
func TestYTLinksFeedClearParent(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_feed_parent", YTLinksOn, 2)
	ctx := t.Context()
	parent, child := ids[0], ids[1]
	if _, changed, err := s.SetTaskParent(ctx, child, &parent, "tester"); err != nil || !changed {
		t.Fatalf("set parent: changed=%t err=%v", changed, err)
	}
	ytlfPass(t, s)
	if got := ytlfLast(t, s, child).Desired.Parent; got == nil || *got != ytlfTargets(parent)[0] {
		t.Fatalf("parent after set = %+v", got)
	}
	n := len(ytlfOps(t, s, child))
	if _, changed, err := s.SetTaskParent(ctx, child, nil, "tester"); err != nil || !changed {
		t.Fatalf("clear parent: changed=%t err=%v", changed, err)
	}
	ytlfPass(t, s)
	if ops := ytlfOps(t, s, child); len(ops) != n+1 {
		t.Fatalf("clearing the parent made %d ops, want %d", len(ops), n+1)
	}
	if got := ytlfLast(t, s, child).Desired.Parent; got != nil {
		t.Fatalf("parent after clear = %+v, want none", got)
	}
}

// Binding a target after its writer changed prepares the writer's op that adds
// the link (I14, §5.3): the edge was held while the target had no binding, and
// the binding is no task change, so only the bind hook's feed row reaches the
// writer. One case per edge kind, each with the writer on the side the writer
// rule names.
func TestYTLinksBindTargetPreparesWriters(t *testing.T) {
	for _, c := range []struct {
		name string
		kind string // ytLinkKindParent or a relation kind
		// the writer is created first (lower id), the target second, except
		// for the parent case where the order does not matter.
		held     string
		snapshot func(ytwrite.IssueLinksSnapshot) []ytwrite.IssueLinkTarget
	}{
		{"parent", ytLinkKindParent, ytwrite.LinkEdgeParent, func(s ytwrite.IssueLinksSnapshot) []ytwrite.IssueLinkTarget {
			if s.Desired.Parent == nil {
				return []ytwrite.IssueLinkTarget{}
			}
			return []ytwrite.IssueLinkTarget{*s.Desired.Parent}
		}},
		{"related", RelationRelated, ytwrite.LinkEdgeRelated, func(s ytwrite.IssueLinksSnapshot) []ytwrite.IssueLinkTarget { return s.Desired.Related }},
		{"duplicate_of", RelationDuplicateOf, ytwrite.LinkEdgeDuplicateOf, func(s ytwrite.IssueLinksSnapshot) []ytwrite.IssueLinkTarget { return s.Desired.Duplicates }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, ids := ytlfSetup(t, "yt_links_bind_"+strings.ReplaceAll(c.name, "_", ""), YTLinksOn, 1)
			ctx := t.Context()
			writer := ids[0]
			target := ytcTask(t, s, ytbProject) // not bound yet
			switch c.kind {
			case ytLinkKindParent:
				if _, changed, err := s.SetTaskParent(ctx, writer, &target, "tester"); err != nil || !changed {
					t.Fatalf("set parent: changed=%t err=%v", changed, err)
				}
			default:
				// The target has the higher id, so the writer is the src of a
				// related edge and the duplicate of a duplicate_of edge.
				if changed, err := s.AddTaskRelation(ctx, writer, c.kind, target, "tester"); err != nil || !changed {
					t.Fatalf("add %s: changed=%t err=%v", c.kind, changed, err)
				}
			}
			ytlfPass(t, s)
			held := ytlfLast(t, s, writer)
			if got := c.snapshot(held); len(got) != 0 {
				t.Fatalf("unbound target was written: %+v", got)
			}
			if !reflect.DeepEqual(held.Held, []ytwrite.IssueLinkHeld{{Kind: c.held, TaskID: target}}) {
				t.Fatalf("held = %+v, want %s edge to %d", held.Held, c.held, target)
			}
			n := len(ytlfOps(t, s, writer))

			before := ytFeedMaxSyncID(t, s)
			ytbBind(t, s, ytbBinding(target))
			if got := ytlFeedIDs(t, s, before); !reflect.DeepEqual(got, ytlIDs(writer)) {
				t.Fatalf("the binding wrote feed rows for tasks %v, want only the writer %d", got, writer)
			}
			ytlfPass(t, s)
			if ops := ytlfOps(t, s, writer); len(ops) != n+1 {
				t.Fatalf("binding the target made %d ops for the writer, want %d", len(ops), n+1)
			}
			bound := ytlfLast(t, s, writer)
			if got := c.snapshot(bound); !reflect.DeepEqual(got, ytlfTargets(target)) || len(bound.Held) != 0 {
				t.Fatalf("after the bind: link %+v held %+v, want the link to task %d and nothing held", got, bound.Held, target)
			}
		})
	}
}

// A binding of a task with no edge, and no task that points at it, writes
// nothing to the feed: the hook costs a task with nothing to project nothing.
func TestYTLinksBindWritesNothingWithoutEdges(t *testing.T) {
	s := schemaStore(t, "yt_links_bind_none")
	ytbMapped(t, s)
	task := ytcTask(t, s, ytbProject)
	before := ytFeedMaxSyncID(t, s)
	ytbBind(t, s, ytbBinding(task))
	if got := ytlFeedIDs(t, s, before); len(got) != 0 {
		t.Fatalf("binding a task with no edges wrote feed rows for %v", got)
	}
}

// A task that writes a link before it is bound gets its own snapshot when it
// is bound: the feed skipped it while unbound, and a binding is no change.
func TestYTLinksBindPreparesTheBoundTaskItself(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_bind_self", YTLinksOn, 1)
	ctx := t.Context()
	parent := ids[0]
	child := ytcTask(t, s, ytbProject)
	if _, changed, err := s.SetTaskParent(ctx, child, &parent, "tester"); err != nil || !changed {
		t.Fatalf("set parent: changed=%t err=%v", changed, err)
	}
	ytlfPass(t, s) // the child is unbound: skipped
	if ops := ytlfOps(t, s, child); len(ops) != 0 {
		t.Fatalf("an unbound task got %d ops", len(ops))
	}
	ytbBind(t, s, ytbBinding(child))
	ytlfPass(t, s)
	if got := ytlfLast(t, s, child).Desired.Parent; got == nil || *got != ytlfTargets(parent)[0] {
		t.Fatalf("the bound child's parent = %+v, want task %d", got, parent)
	}
}

// Mode off prepares and admits nothing, whatever changes (the default leaves
// behaviour as it was), but every pass that reads a change moves the sweep
// generation, and so does the creation of the cursor (I13). Turning the mode
// on makes the sweep prepare the snapshot of the change made while off.
func TestYTLinksGateOffChangeThenEnableSweeps(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_off", YTLinksOff, 2)
	ctx := t.Context()
	lo, hi := ids[0], ids[1]
	// The cursor's creation was the first change nothing handled; ytbBind's
	// own feed rows (none: it only writes bindings) did not exist.
	if st := ytlfSweepState(t, s); st.Generation != 1 || st.DoneGeneration != 0 || st.Complete() {
		t.Fatalf("after the cursor was created: %+v", st)
	}
	if changed, err := s.AddTaskRelation(ctx, hi, RelationRelated, lo, "tester"); err != nil || !changed {
		t.Fatalf("add: changed=%t err=%v", changed, err)
	}
	if pass := ytlfPass(t, s); pass.Handled != 2 {
		t.Fatalf("pass %+v", pass)
	}
	if st := ytlfSweepState(t, s); st.Generation != 2 {
		t.Fatalf("a pass that read changes with the mode off left generation %d, want 2", st.Generation)
	}
	// An idle pass reads nothing and moves nothing.
	if pass := ytlfPass(t, s); pass.Read != 0 {
		t.Fatalf("idle pass %+v", pass)
	}
	if st := ytlfSweepState(t, s); st.Generation != 2 {
		t.Fatalf("an idle pass moved the generation to %d", st.Generation)
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE kind='issue_links'`); n != 0 {
		t.Fatalf("mode off prepared %d ops", n)
	}
	// With the mode off the sweep does nothing either.
	if pass, err := s.RunYTLinksSweep(ctx, time.Now().UTC(), 10); err != nil || !pass.Idle {
		t.Fatalf("sweep with the mode off = %+v, %v", pass, err)
	}

	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	// One task per slice: the walk takes three passes, the last finding the end.
	var complete bool
	for i := 0; i < 5 && !complete; i++ {
		pass, err := s.RunYTLinksSweep(ctx, time.Now().UTC(), 1)
		if err != nil || pass.Restarted {
			t.Fatalf("sweep slice %d = %+v, %v", i, pass, err)
		}
		complete = pass.Complete
	}
	if st := ytlfSweepState(t, s); !complete || !st.Complete() || st.DoneGeneration != 2 || st.AfterTaskID != 0 {
		t.Fatalf("sweep did not complete at generation 2: complete=%t %+v", complete, st)
	}
	if got := ytlfLast(t, s, lo).Desired.Related; !reflect.DeepEqual(got, ytlfTargets(hi)) {
		t.Fatalf("the sweep prepared related %+v for the writer, want task %d", got, hi)
	}
	// A complete sweep owes nothing: the next slice is idle and prepares nothing.
	if pass, err := s.RunYTLinksSweep(ctx, time.Now().UTC(), 10); err != nil || !pass.Idle {
		t.Fatalf("sweep after completion = %+v, %v", pass, err)
	}
}

// A sweep restarts when the generation moves mid-walk (I13): the slice that
// raced the bump discards its position, the row is back at the start of a
// walk of the new generation, and only a walk that ran entirely at that
// generation completes it.
func TestYTLinksSweepRestartsWhenGenerationMovesMidWalk(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_restart", YTLinksOn, 3)
	ctx := t.Context()
	if st := ytlfSweepState(t, s); st.Generation != 1 || st.Complete() {
		t.Fatalf("setup state %+v", st)
	}
	bump := func() {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := bumpYTLinksSweepTx(ctx, tx, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// Slice 1 visits two of three tasks; a change goes unhandled (generation
	// 2) after its first task.
	bumped := false
	s.ytLinksSweepVisited = func(id int64) {
		if id == ids[0] && !bumped {
			bumped = true
			bump()
		}
	}
	pass, err := s.RunYTLinksSweep(ctx, time.Now().UTC(), 2)
	if err != nil || !pass.Restarted || pass.Complete || pass.Generation != 1 {
		t.Fatalf("racing slice = %+v, %v", pass, err)
	}
	if st := ytlfSweepState(t, s); st.Generation != 2 || st.DoneGeneration != 0 || st.AfterTaskID != 0 {
		t.Fatalf("after the racing slice: %+v, want generation 2 at the start of a walk", st)
	}
	s.ytLinksSweepVisited = nil
	// A walk that completes at generation 1 must not count: finish the
	// remaining walk and see generation 2 done.
	for i := 0; i < 5; i++ {
		pass, err := s.RunYTLinksSweep(ctx, time.Now().UTC(), 2)
		if err != nil || pass.Restarted || pass.Generation != 2 {
			t.Fatalf("slice %d = %+v, %v", i, pass, err)
		}
		if pass.Complete {
			break
		}
	}
	if st := ytlfSweepState(t, s); st.DoneGeneration != 2 || !st.Complete() {
		t.Fatalf("after the walk at generation 2: %+v", st)
	}
}

// A bump on the last slice also restarts: the slice that reached the end of
// the bound tasks does not mark the generation it read as done when the row
// moved meanwhile.
func TestYTLinksSweepLastSliceFencedByGeneration(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_restart_last", YTLinksOn, 1)
	ctx := t.Context()
	bumped := false
	s.ytLinksSweepVisited = func(id int64) {
		if id == ids[0] && !bumped {
			bumped = true
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			if err := bumpYTLinksSweepTx(ctx, tx, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	pass, err := s.RunYTLinksSweep(ctx, time.Now().UTC(), 5)
	if err != nil || !pass.Restarted || pass.Complete {
		t.Fatalf("slice = %+v, %v", pass, err)
	}
	if st := ytlfSweepState(t, s); st.Generation != 2 || st.DoneGeneration != 0 {
		t.Fatalf("a slice that raced a bump marked a generation done: %+v", st)
	}
}

// The sweep walks only the tenant's bound tasks, in id order, and an unchanged
// snapshot makes no second op.
func TestYTLinksSweepIsIdempotent(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_idem", YTLinksOn, 2)
	ctx := t.Context()
	other := ytcTask(t, s, ytbProject)
	ytbBind(t, s, YTTaskBinding{TaskID: other, Tenant: "another.youtrack.cloud", IssueID: ytfIssue(other), ProjectID: ytbProjectID, BoundBy: "operator"})
	if changed, err := s.AddTaskRelation(ctx, ids[0], RelationRelated, ids[1], "tester"); err != nil || !changed {
		t.Fatalf("add: changed=%t err=%v", changed, err)
	}
	ytlfPass(t, s)
	n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE kind='issue_links'`)
	for gen := 1; gen <= 2; gen++ {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := bumpYTLinksSweepTx(ctx, tx, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			pass, err := s.RunYTLinksSweep(ctx, time.Now().UTC(), 100)
			if err != nil || pass.Restarted {
				t.Fatalf("slice = %+v, %v", pass, err)
			}
			if pass.Complete {
				break
			}
		}
		if got := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE kind='issue_links'`); got != n {
			t.Fatalf("walk %d made %d new ops for unchanged snapshots", gen, got-n)
		}
	}
	if n := ytcCount(t, s, `SELECT count(*) FROM yt_write_ops WHERE kind='issue_links' AND tenant='another.youtrack.cloud'`); n != 0 {
		t.Fatalf("the sweep prepared %d ops for another tenant's binding", n)
	}
}

// Unbinding a task rejects its prepared links op, like its field snapshots:
// the op targets the issue the binding named, and left prepared it would block
// the task's lane for good.
func TestYTLinksUnbindRejectsPreparedOps(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_unbind", YTLinksOn, 2)
	ctx := t.Context()
	if changed, err := s.AddTaskRelation(ctx, ids[0], RelationRelated, ids[1], "tester"); err != nil || !changed {
		t.Fatalf("add: changed=%t err=%v", changed, err)
	}
	ytlfPass(t, s)
	ops := ytlfOps(t, s, ids[0])
	if len(ops) == 0 || ops[len(ops)-1].State != ytwrite.StatePrepared {
		t.Fatalf("ops %+v", ops)
	}
	res, err := s.UnbindYTTaskBinding(ctx, ids[0], YTUnbindOptions{By: "operator", Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range res.Rejected {
		found = found || (r.Kind == ytwrite.KindIssueLinks && r.OpID == ops[len(ops)-1].ID)
	}
	if !found {
		t.Fatalf("unbind rejected %+v, want the links op %d", res.Rejected, ops[len(ops)-1].ID)
	}
}

// Held-op recovery covers the links kind: an op whose task lost its binding
// is closed once it has been held for the grace, so the lane is not blocked.
func TestYTLinksHeldRecoveryClosesStrandedOps(t *testing.T) {
	s, ids := ytlfSetup(t, "yt_links_held", YTLinksOn, 2)
	ctx := t.Context()
	if changed, err := s.AddTaskRelation(ctx, ids[0], RelationRelated, ids[1], "tester"); err != nil || !changed {
		t.Fatalf("add: changed=%t err=%v", changed, err)
	}
	ytlfPass(t, s)
	ops := ytlfOps(t, s, ids[0])
	op := ops[len(ops)-1]
	if _, err := s.pool.Exec(ctx, `DELETE FROM yt_task_bindings WHERE task_id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(2 * YTHeldRecoveryGrace)
	got, err := s.RecoverHeldYTOps(ctx, ytcTenant, at, YTHeldRecoveryGrace)
	if err != nil {
		t.Fatal(err)
	}
	var closed bool
	for _, id := range got.OpIDs {
		closed = closed || id == op.ID
	}
	if !closed {
		t.Fatalf("recovery %+v did not close the stranded links op %d", got, op.ID)
	}
}

// The gate admits an issue_links op only with the mode on and the op's
// target the writer's bound issue on the op's tenant.
func TestYTLinksGateSQL(t *testing.T) {
	s := &Store{}
	if got := ytLinksGateSQL(s); got != `FALSE` {
		t.Errorf("mode off: %s", got)
	}
	s.SetYTLinksMode(YTLinksOn, "")
	if got := ytLinksGateSQL(s); got != `FALSE` {
		t.Errorf("on without a tenant: %s", got)
	}
	s.SetYTLinksMode(YTLinksOn, ytcTenant)
	want := `o.entity_kind = 'task' AND EXISTS (SELECT 1 FROM yt_task_bindings b
		WHERE b.task_id::text = o.entity_id AND b.issue_id = o.yt_target_id AND b.tenant = o.tenant)`
	if got := ytLinksGateSQL(s); got != want {
		t.Errorf("mode on:\n got %s\nwant %s", got, want)
	}
	if !strings.Contains(s.ytAdmitSQL("$2"), `(o.kind = 'issue_links' AND (`+want+`))`) {
		t.Errorf("the admission SQL does not carry the links gate: %s", s.ytAdmitSQL("$2"))
	}
}

// The feed consumer's cursor creation and the registry: task_links is
// registered with its created hook.
func TestYTLinksConsumerRegistered(t *testing.T) {
	c, ok := ytFeedConsumers[ytLinksConsumer]
	if !ok || c.build == nil || c.created == nil || len(c.models) != 1 || c.models[0] != "task" {
		t.Fatalf("task_links consumer = %+v, %v", c, ok)
	}
}
