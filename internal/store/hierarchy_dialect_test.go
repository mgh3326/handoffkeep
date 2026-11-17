package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// Dialect-neutral behaviour cases for relations, parent links and the
// auto-close rule (hk 5074, B2). See task_flow_dialect_test.go for the
// helpers and for what a skipped MySQL step means.

func init() {
	registerTaskAreaCase("hierarchy/relations", hierarchyCaseRelations)
	registerTaskAreaCase("hierarchy/relation-cycles", hierarchyCaseRelationCycles)
	registerTaskAreaCase("hierarchy/relation-add-concurrent", hierarchyCaseRelationAddConcurrent)
	registerTaskAreaCase("hierarchy/duplicate-of", hierarchyCaseDuplicateOf)
	registerTaskAreaCase("hierarchy/parent-links", hierarchyCaseParentLinks)
	registerTaskAreaCase("hierarchy/parent-cycle-concurrent", hierarchyCaseParentCycleConcurrent)
	registerTaskAreaCase("hierarchy/auto-close", hierarchyCaseAutoClose)
}

func hierarchyCaseRelations(t *testing.T, s *Store) {
	ctx := t.Context()
	a, b := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")

	changed, err := s.AddTaskRelation(ctx, a.ID, RelationBlocks, b.ID, "tester")
	flowMust(t, err)
	if !changed {
		t.Fatal("a new edge reported no change")
	}
	if changed, err = s.AddTaskRelation(ctx, a.ID, RelationBlocks, b.ID, "tester"); err != nil || changed {
		t.Fatalf("a live edge added again: changed=%t err=%v", changed, err)
	}
	// related is symmetric: either order is the same stored edge, low id first.
	if changed, err = s.AddTaskRelation(ctx, b.ID, RelationRelated, a.ID, "tester"); err != nil || !changed {
		t.Fatalf("related edge: changed=%t err=%v", changed, err)
	}
	if changed, err = s.AddTaskRelation(ctx, a.ID, RelationRelated, b.ID, "tester"); err != nil || changed {
		t.Fatalf("related edge in the other order: changed=%t err=%v", changed, err)
	}

	for _, id := range []int64{a.ID, b.ID} {
		rels, err := s.ListTaskRelations(ctx, id)
		flowMust(t, err)
		if len(rels) != 2 || rels[0].Kind != RelationBlocks || rels[1].Kind != RelationRelated || rels[0].ID >= rels[1].ID {
			t.Fatalf("relations of %d=%+v", id, rels)
		}
		if rels[0].SrcID != a.ID || rels[0].DstID != b.ID || rels[1].SrcID != min(a.ID, b.ID) || rels[1].DstID != max(a.ID, b.ID) {
			t.Fatalf("stored ends of %d=%+v", id, rels)
		}
	}

	// An edge change reaches both endpoints' updated_at (the task feed).
	before := flowCaseGet(t, s, b.ID).UpdatedAt
	time.Sleep(2 * time.Millisecond)
	if changed, err = s.RemoveTaskRelation(ctx, a.ID, RelationBlocks, b.ID, "tester"); err != nil || !changed {
		t.Fatalf("remove: changed=%t err=%v", changed, err)
	}
	if after := flowCaseGet(t, s, b.ID).UpdatedAt; !after.After(before) {
		t.Fatalf("destination updated_at did not move: %v -> %v", before, after)
	}
	if changed, err = s.RemoveTaskRelation(ctx, a.ID, RelationBlocks, b.ID, "tester"); err != nil || changed {
		t.Fatalf("removing an archived edge: changed=%t err=%v", changed, err)
	}
	if changed, err = s.RemoveTaskRelation(ctx, 1<<40, RelationBlocks, b.ID, "tester"); err != nil || changed {
		t.Fatalf("removing an edge of a missing task: changed=%t err=%v", changed, err)
	}
	rels, err := s.ListTaskRelations(ctx, a.ID)
	flowMust(t, err)
	if len(rels) != 1 || rels[0].Kind != RelationRelated {
		t.Fatalf("relations after remove=%+v", rels)
	}
	// Adding an archived edge revives it.
	if changed, err = s.AddTaskRelation(ctx, a.ID, RelationBlocks, b.ID, "tester"); err != nil || !changed {
		t.Fatalf("revive: changed=%t err=%v", changed, err)
	}
	if rels, err = s.ListTaskRelations(ctx, a.ID); err != nil || len(rels) != 2 {
		t.Fatalf("relations after revive=%+v err=%v", rels, err)
	}

	// Refusals.
	if _, err = s.AddTaskRelation(ctx, a.ID, RelationBlocks, a.ID, "tester"); err == nil {
		t.Fatal("a self-link was accepted")
	}
	if _, err = s.AddTaskRelation(ctx, a.ID, "no-such-kind", b.ID, "tester"); err == nil {
		t.Fatal("an unknown relation kind was accepted")
	}
	if _, err = s.AddTaskRelation(ctx, a.ID, RelationBlocks, 1<<40, "tester"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing destination: err=%v, want ErrTaskNotFound", err)
	}
	if _, err = s.AddTaskRelation(ctx, 1<<40, RelationBlocks, b.ID, "tester"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing source: err=%v, want ErrTaskNotFound", err)
	}
	if _, err = s.AddTaskRelation(ctx, a.ID, RelationBlocks, b.ID, ""); err == nil {
		t.Fatal("a relation without an actor was accepted")
	}
}

// hierarchyCaseRelationCycles checks the recursive reachability query: an edge
// that would close a cycle of blocks is refused, however long the path, and a
// related edge is never a cycle.
func hierarchyCaseRelationCycles(t *testing.T, s *Store) {
	ctx := t.Context()
	const n = 12
	chain := make([]Task, n)
	for i := range chain {
		chain[i] = flowCaseTask(t, s, "implement")
		if i > 0 {
			if _, err := s.AddTaskRelation(ctx, chain[i-1].ID, RelationBlocks, chain[i].ID, "tester"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.AddTaskRelation(ctx, chain[n-1].ID, RelationBlocks, chain[0].ID, "tester"); !errors.Is(err, ErrRelationCycle) {
		t.Fatalf("closing the chain: err=%v, want ErrRelationCycle", err)
	}
	if _, err := s.AddTaskRelation(ctx, chain[n-1].ID, RelationBlocks, chain[n/2].ID, "tester"); !errors.Is(err, ErrRelationCycle) {
		t.Fatalf("closing a loop in the middle: err=%v, want ErrRelationCycle", err)
	}
	// A skip edge forward is not a cycle, and neither is a related edge back.
	if changed, err := s.AddTaskRelation(ctx, chain[0].ID, RelationBlocks, chain[n-1].ID, "tester"); err != nil || !changed {
		t.Fatalf("skip edge: changed=%t err=%v", changed, err)
	}
	if changed, err := s.AddTaskRelation(ctx, chain[n-1].ID, RelationRelated, chain[0].ID, "tester"); err != nil || !changed {
		t.Fatalf("related edge back: changed=%t err=%v", changed, err)
	}
	// Archiving a link of the chain breaks the path: the edge that was a cycle
	// through it is allowed (the skip edge 0 -> n-1 leads nowhere back to 4).
	if _, err := s.AddTaskRelation(ctx, chain[4].ID, RelationBlocks, chain[0].ID, "tester"); !errors.Is(err, ErrRelationCycle) {
		t.Fatalf("4 -> 0 over the intact chain: err=%v, want ErrRelationCycle", err)
	}
	if changed, err := s.RemoveTaskRelation(ctx, chain[3].ID, RelationBlocks, chain[4].ID, "tester"); err != nil || !changed {
		t.Fatalf("break the chain: changed=%t err=%v", changed, err)
	}
	if changed, err := s.AddTaskRelation(ctx, chain[4].ID, RelationBlocks, chain[0].ID, "tester"); err != nil || !changed {
		t.Fatalf("4 -> 0 over the broken chain: changed=%t err=%v", changed, err)
	}
}

// hierarchyCaseRelationAddConcurrent adds one edge from several callers at
// once: the task-mutation lock lets exactly one create it.
func hierarchyCaseRelationAddConcurrent(t *testing.T, s *Store) {
	ctx := t.Context()
	a, b := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")
	const callers = 6
	changed := make([]bool, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			changed[i], errs[i] = s.AddTaskRelation(ctx, a.ID, RelationBlocks, b.ID, "tester")
		}()
	}
	wg.Wait()
	created := 0
	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if changed[i] {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d callers created the edge, want 1", created)
	}
	if rels, err := s.ListTaskRelations(ctx, a.ID); err != nil || len(rels) != 1 {
		t.Fatalf("relations=%+v err=%v", rels, err)
	}
}

// hierarchyCaseDuplicateOf: adding a duplicate_of edge moves the task to the
// duplicate state in the same transaction (entering a settled state mirrors
// decision records, so MySQL skips this until that area is ported).
func hierarchyCaseDuplicateOf(t *testing.T, s *Store) {
	ctx := t.Context()
	a, b := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")

	changed, err := s.AddTaskRelation(ctx, a.ID, RelationDuplicateOf, b.ID, "tester")
	flowSkipUnported(t, s, err)
	if !changed {
		t.Fatal("duplicate_of reported no change")
	}
	if got := flowCaseGet(t, s, a.ID); got.State != StateDuplicate {
		t.Fatalf("state=%q, want duplicate", got.State)
	}
	if changed, err = s.AddTaskRelation(ctx, a.ID, RelationDuplicateOf, b.ID, "tester"); err != nil || changed {
		t.Fatalf("duplicate_of again: changed=%t err=%v", changed, err)
	}
	// The edge and the state exist together: removing the edge is refused.
	if _, err = s.RemoveTaskRelation(ctx, a.ID, RelationDuplicateOf, b.ID, "tester"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("remove of a duplicate's edge: err=%v, want ErrTaskConflict", err)
	}
	// A duplicate_of edge that would close a cycle is refused.
	if _, err = s.AddTaskRelation(ctx, b.ID, RelationDuplicateOf, a.ID, "tester"); !errors.Is(err, ErrRelationCycle) {
		t.Fatalf("duplicate cycle: err=%v, want ErrRelationCycle", err)
	}
	if _, err = s.RemoveTaskRelation(ctx, 1<<40, RelationDuplicateOf, b.ID, "tester"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("remove of a missing task's duplicate_of: err=%v, want ErrTaskNotFound", err)
	}
}

func hierarchyCaseParentLinks(t *testing.T, s *Store) {
	ctx := t.Context()
	a, b, c := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")

	got, changed, err := s.SetTaskParent(ctx, b.ID, &a.ID, "tester")
	flowMust(t, err)
	if !changed || got.ParentID == nil || *got.ParentID != a.ID {
		t.Fatalf("set parent: changed=%t parent=%v", changed, got.ParentID)
	}
	if got, changed, err = s.SetTaskParent(ctx, b.ID, &a.ID, "tester"); err != nil || changed || got.ParentID == nil {
		t.Fatalf("same parent again: changed=%t parent=%v err=%v", changed, got.ParentID, err)
	}
	if _, _, err = s.SetTaskParent(ctx, c.ID, &b.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	kids, err := s.ListTaskChildren(ctx, a.ID)
	flowMust(t, err)
	if len(kids) != 1 || kids[0].ID != b.ID || kids[0].Title != b.Title || kids[0].State != StateBacklog {
		t.Fatalf("children of a=%+v", kids)
	}

	// Refusals: itself, a missing task, and an ancestor chain that loops.
	if _, _, err = s.SetTaskParent(ctx, a.ID, &a.ID, "tester"); !errors.Is(err, ErrTaskParentInvalid) {
		t.Fatalf("own parent: err=%v, want ErrTaskParentInvalid", err)
	}
	missing := int64(1 << 40)
	if _, _, err = s.SetTaskParent(ctx, a.ID, &missing, "tester"); !errors.Is(err, ErrTaskParentInvalid) {
		t.Fatalf("missing parent: err=%v, want ErrTaskParentInvalid", err)
	}
	if _, _, err = s.SetTaskParent(ctx, missing, &a.ID, "tester"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: err=%v, want ErrTaskNotFound", err)
	}
	if _, _, err = s.SetTaskParent(ctx, a.ID, &c.ID, "tester"); !errors.Is(err, ErrTaskParentCycle) {
		t.Fatalf("a under its grandchild: err=%v, want ErrTaskParentCycle", err)
	}
	if _, _, err = s.SetTaskParent(ctx, 0, &a.ID, "tester"); err == nil {
		t.Fatal("a parent change of task 0 was accepted")
	}

	// Clearing the link records an event and frees the task.
	got, changed, err = s.SetTaskParent(ctx, c.ID, nil, "tester")
	flowMust(t, err)
	if !changed || got.ParentID != nil {
		t.Fatalf("clear: changed=%t parent=%v", changed, got.ParentID)
	}
	if got, changed, err = s.SetTaskParent(ctx, c.ID, nil, "tester"); err != nil || changed {
		t.Fatalf("clear again: changed=%t err=%v", changed, err)
	}
	if _, _, err = s.SetTaskParent(ctx, a.ID, &c.ID, "tester"); err != nil {
		t.Fatalf("a under c after the link was cleared: %v", err)
	}
	var parentEvents int
	for _, e := range flowCaseGet(t, s, c.ID).Events {
		if e.Kind == TaskEventParent {
			parentEvents++
		}
	}
	if parentEvents != 2 {
		t.Fatalf("%d parent events on c, want 2 (set and clear)", parentEvents)
	}
}

// hierarchyCaseParentCycleConcurrent makes two tasks each other's parent from
// two callers at once: the lock serialises them, so one wins and the other
// sees the cycle.
func hierarchyCaseParentCycleConcurrent(t *testing.T, s *Store) {
	ctx := t.Context()
	a, b := flowCaseTask(t, s, "implement"), flowCaseTask(t, s, "implement")
	pairs := [][2]Task{{a, b}, {b, a}}
	errs := make([]error, len(pairs))
	var wg sync.WaitGroup
	for i := range pairs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = s.SetTaskParent(ctx, pairs[i][0].ID, &pairs[i][1].ID, "tester")
		}()
	}
	wg.Wait()
	var won, cycles int
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrTaskParentCycle):
			cycles++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 || cycles != 1 {
		t.Fatalf("won=%d cycles=%d (errors %v), want 1 and 1", won, cycles, errs)
	}
}

// hierarchyCaseAutoClose settles the sub-tasks of an ops parent one by one:
// the parent closes with the last, and reopening a child afterwards leaves a
// record on the completed parent without reopening it. Settling a task mirrors
// decision records, so MySQL skips this until the decisions area is ported.
func hierarchyCaseAutoClose(t *testing.T, s *Store) {
	ctx := t.Context()
	parent := flowCaseTask(t, s, "ops")
	kids := []Task{flowCaseTask(t, s, "ops"), flowCaseTask(t, s, "ops")}
	for _, k := range kids {
		_, _, err := s.SetTaskParent(ctx, k.ID, &parent.ID, "tester")
		flowMust(t, err)
	}

	_, err := s.CloseTaskCompleted(ctx, kids[0].ID, "hk:doc research/auto-close", "tester", "first done", "ac-1")
	flowSkipUnported(t, s, err)
	if got := flowCaseGet(t, s, parent.ID); got.State != StateBacklog {
		t.Fatalf("parent closed with one sub-task open: state=%q", got.State)
	}
	_, err = s.CloseTaskCompleted(ctx, kids[1].ID, "hk:doc research/auto-close", "tester", "second done", "ac-2")
	flowMust(t, err)
	got := flowCaseGet(t, s, parent.ID)
	if got.State != StateCompleted || got.Refs.Closure == nil {
		t.Fatalf("parent state=%q closure=%v, want completed with a closure", got.State, got.Refs.Closure)
	}
	var closed bool
	for _, e := range got.Events {
		if e.Kind == TaskEventTransition && e.Auto && e.Source == "hk.auto_close" && e.To == StateCompleted {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("no automatic close event on the parent: %+v", got.Events)
	}

	if _, err = s.ReopenTask(ctx, kids[0].ID, "tester", "more to do"); err != nil {
		t.Fatal(err)
	}
	got = flowCaseGet(t, s, parent.ID)
	if got.State != StateCompleted {
		t.Fatalf("parent reopened with its child: state=%q", got.State)
	}
	var skipped bool
	for _, e := range got.Events {
		if e.Kind == TaskEventAutoClose && !e.Applied {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("no unapplied auto_close record on the completed parent: %+v", got.Events)
	}

	// An implement parent stays open when its sub-tasks only complete.
	impl := flowCaseTask(t, s, "implement")
	child := flowCaseTask(t, s, "ops")
	if _, _, err = s.SetTaskParent(ctx, child.ID, &impl.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CloseTaskCompleted(ctx, child.ID, "hk:doc research/auto-close", "tester", "done", "ac-3"); err != nil {
		t.Fatal(err)
	}
	if got = flowCaseGet(t, s, impl.ID); got.State != StateBacklog {
		t.Fatalf("implement parent state=%q, want backlog", got.State)
	}
}
