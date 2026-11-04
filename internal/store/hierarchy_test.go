package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newTask(t *testing.T, s *Store, pool *pgxpool.Pool, state string) Task {
	t.Helper()
	return flowTask(t, s, pool, "implement", state, true)
}

func setParent(t *testing.T, s *Store, child, parent Task) {
	t.Helper()
	p := parent.ID
	if _, _, err := s.SetTaskParent(t.Context(), child.ID, &p, "tester"); err != nil {
		t.Fatalf("parent of %d: %v", child.ID, err)
	}
}

func eventsOfKind(x Task, kind string) []TaskEvent {
	var out []TaskEvent
	for _, e := range x.Events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestParentLinkIsSingleAndAcyclic(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	a, b, c := newTask(t, s, pool, "backlog"), newTask(t, s, pool, "backlog"), newTask(t, s, pool, "backlog")
	setParent(t, s, b, a)
	setParent(t, s, c, b)
	aID := a.ID
	// a under c would make a its own ancestor.
	if _, _, err := s.SetTaskParent(ctx, a.ID, &c.ID, "tester"); !errors.Is(err, ErrTaskParentCycle) {
		t.Fatalf("cycle: err=%v, want ErrTaskParentCycle", err)
	}
	if _, _, err := s.SetTaskParent(ctx, a.ID, &a.ID, "tester"); !errors.Is(err, ErrTaskParentInvalid) {
		t.Fatalf("self parent: err=%v, want ErrTaskParentInvalid", err)
	}
	missing := int64(999999999)
	if _, _, err := s.SetTaskParent(ctx, a.ID, &missing, "tester"); !errors.Is(err, ErrTaskParentInvalid) {
		t.Fatalf("missing parent: err=%v, want ErrTaskParentInvalid", err)
	}
	if got := stateOf(t, s, a.ID); got.ParentID != nil {
		t.Fatalf("refused parent changes must leave the task alone: %+v", got.ParentID)
	}
	// Setting the same parent again changes nothing and writes no event.
	n := len(eventsOfKind(stateOf(t, s, b.ID), TaskEventParent))
	if _, changed, err := s.SetTaskParent(ctx, b.ID, &aID, "tester"); err != nil || changed {
		t.Fatalf("same parent: changed=%t err=%v", changed, err)
	}
	if got := len(eventsOfKind(stateOf(t, s, b.ID), TaskEventParent)); got != n {
		t.Fatalf("parent events %d -> %d", n, got)
	}
	// One parent: moving b under c replaces a, and the old parent no longer lists it.
	if _, _, err := s.SetTaskParent(ctx, c.ID, nil, "tester"); err != nil {
		t.Fatal(err)
	}
	setParent(t, s, b, c)
	if got := stateOf(t, s, b.ID); got.ParentID == nil || *got.ParentID != c.ID {
		t.Fatalf("b parent=%v, want %d", got.ParentID, c.ID)
	}
	if kids, err := s.ListTaskChildren(ctx, a.ID); err != nil || len(kids) != 0 {
		t.Fatalf("a children=%v err=%v", kids, err)
	}
	if kids, err := s.ListTaskChildren(ctx, c.ID); err != nil || len(kids) != 1 || kids[0].ID != b.ID || kids[0].Category != "backlog" {
		t.Fatalf("c children=%v err=%v", kids, err)
	}
	// Clearing works, and a new task can be filed under a parent.
	if got, changed, err := s.SetTaskParent(ctx, b.ID, nil, "tester"); err != nil || !changed || got.ParentID != nil {
		t.Fatalf("clear: %+v changed=%t err=%v", got, changed, err)
	}
	child, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "child"), Title: "child", Kind: "fix", CreatedBy: "tester", Project: projectPtr(testProjectName), ParentID: &a.ID})
	if err != nil || child.ParentID == nil || *child.ParentID != a.ID {
		t.Fatalf("create under parent: %+v err=%v", child, err)
	}
	if _, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "child"), Title: "orphan", Kind: "fix", CreatedBy: "tester", Project: projectPtr(testProjectName), ParentID: &missing}); !errors.Is(err, ErrTaskParentInvalid) {
		t.Fatalf("create under a missing parent: err=%v", err)
	}
	if detail := stateOf(t, s, a.ID); len(detail.Children) != 1 || detail.Children[0].ID != child.ID {
		t.Fatalf("detail children=%+v", detail.Children)
	}
}

func TestRelationSelfLinksAndCycles(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	a, b, c := newTask(t, s, pool, "backlog"), newTask(t, s, pool, "backlog"), newTask(t, s, pool, "backlog")
	for _, kind := range RelationKinds() {
		if _, err := s.AddTaskRelation(ctx, a.ID, kind, a.ID, "tester"); err == nil {
			t.Fatalf("%s self-link must be refused", kind)
		}
	}
	if _, err := s.AddTaskRelation(ctx, a.ID, "similar", b.ID, "tester"); err == nil {
		t.Fatal("an unknown relation kind must be refused")
	}
	if _, err := s.AddTaskRelation(ctx, a.ID, RelationBlocks, 999999999, "tester"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing endpoint: err=%v, want ErrTaskNotFound", err)
	}
	for _, edge := range [][2]Task{{a, b}, {b, c}} {
		if changed, err := s.AddTaskRelation(ctx, edge[0].ID, RelationBlocks, edge[1].ID, "tester"); err != nil || !changed {
			t.Fatalf("blocks %d->%d: changed=%t err=%v", edge[0].ID, edge[1].ID, changed, err)
		}
	}
	if _, err := s.AddTaskRelation(ctx, c.ID, RelationBlocks, a.ID, "tester"); !errors.Is(err, ErrRelationCycle) {
		t.Fatalf("blocks cycle: err=%v, want ErrRelationCycle", err)
	}
	if _, err := s.AddTaskRelation(ctx, b.ID, RelationBlocks, a.ID, "tester"); !errors.Is(err, ErrRelationCycle) {
		t.Fatalf("two-task blocks cycle: err=%v, want ErrRelationCycle", err)
	}
	// blocks is informational: it gates no claim and edits no state.
	if got, err := s.ClaimTask(ctx, b.ID, "worker-1", "job-blocked", ""); err != nil || got.State != StateDeveloping {
		t.Fatalf("a blocked task is still claimable: %+v err=%v", got, err)
	}
	// related is symmetric: the reverse add is the same edge.
	if changed, err := s.AddTaskRelation(ctx, a.ID, RelationRelated, c.ID, "tester"); err != nil || !changed {
		t.Fatalf("related: changed=%t err=%v", changed, err)
	}
	if changed, err := s.AddTaskRelation(ctx, c.ID, RelationRelated, a.ID, "tester"); err != nil || changed {
		t.Fatalf("reverse related: changed=%t err=%v, want the same edge", changed, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_relations WHERE kind='related' AND ((src_id=$1 AND dst_id=$2) OR (src_id=$2 AND dst_id=$1))`, a.ID, c.ID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("related rows=%d err=%v, want 1", rows, err)
	}
	// related has no cycle rule: a, b, c may all be related in a ring.
	for _, edge := range [][2]Task{{a, b}, {b, c}} {
		if _, err := s.AddTaskRelation(ctx, edge[0].ID, RelationRelated, edge[1].ID, "tester"); err != nil {
			t.Fatalf("related ring: %v", err)
		}
	}
	// A duplicate_of that would close a cycle is refused whole: the task does
	// not become a duplicate.
	x, y := newTask(t, s, pool, "backlog"), newTask(t, s, pool, "backlog")
	if changed, err := s.AddTaskRelation(ctx, x.ID, RelationDuplicateOf, y.ID, "tester"); err != nil || !changed {
		t.Fatalf("x duplicate_of y: changed=%t err=%v", changed, err)
	}
	if _, err := s.AddTaskRelation(ctx, y.ID, RelationDuplicateOf, x.ID, "tester"); err == nil {
		t.Fatal("y duplicate_of x must be refused: x already duplicates y")
	}
	if got := stateOf(t, s, y.ID); got.State != StateBacklog {
		t.Fatalf("a refused duplicate_of must not move the task: %s", got.State)
	}
}

func TestRelationSoftDeleteAndReAddConverge(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	a, b := newTask(t, s, pool, "backlog"), newTask(t, s, pool, "backlog")
	count := func(live bool) int {
		t.Helper()
		q := `SELECT count(*) FROM task_relations WHERE kind='blocks' AND src_id=$1 AND dst_id=$2`
		if live {
			q += ` AND archived_at IS NULL`
		}
		var n int
		if err := pool.QueryRow(ctx, q, a.ID, b.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	steps := []struct {
		add         bool
		wantChanged bool
		wantLive    int
	}{
		{true, true, 1}, {true, false, 1}, // repeated add converges
		{false, true, 0}, {false, false, 0}, // repeated remove converges
		{true, true, 1}, {false, true, 0}, {true, true, 1}, // re-add revives the archived row
	}
	for i, step := range steps {
		var changed bool
		var err error
		if step.add {
			changed, err = s.AddTaskRelation(ctx, a.ID, RelationBlocks, b.ID, "tester")
		} else {
			changed, err = s.RemoveTaskRelation(ctx, a.ID, RelationBlocks, b.ID, "tester")
		}
		if err != nil || changed != step.wantChanged || count(true) != step.wantLive || count(false) != 1 {
			t.Fatalf("step %d add=%t: changed=%t err=%v live=%d total=%d", i, step.add, changed, err, count(true), count(false))
		}
	}
	// Removing an edge that never existed changes nothing.
	if changed, err := s.RemoveTaskRelation(ctx, b.ID, RelationBlocks, a.ID, "tester"); err != nil || changed {
		t.Fatalf("remove of an absent edge: changed=%t err=%v", changed, err)
	}
	rels, err := s.ListTaskRelations(ctx, a.ID)
	if err != nil || len(rels) != 1 || rels[0].Kind != RelationBlocks || rels[0].SrcID != a.ID || rels[0].DstID != b.ID {
		t.Fatalf("relations of a: %+v err=%v", rels, err)
	}
	if detail := stateOf(t, s, b.ID); len(detail.Relations) != 1 {
		t.Fatalf("detail of the other end: %+v", detail.Relations)
	}
	// Removing and re-adding writes relation events.
	if got := len(eventsOfKind(stateOf(t, s, a.ID), TaskEventRelation)); got != 5 {
		t.Fatalf("relation events=%d, want 5 (3 adds, 2 removes)", got)
	}
}

func TestDuplicateAndDuplicateOfTogether(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	target := newTask(t, s, pool, "backlog")
	// Without duplicate_of the state is refused and nothing changes.
	x := newTask(t, s, pool, "backlog")
	if _, err := s.TransitionTask(ctx, x.ID, "duplicate", "tester", "same as the target", nil, ""); !errors.Is(err, ErrDuplicateOfRequired) {
		t.Fatalf("duplicate without duplicate_of: err=%v, want ErrDuplicateOfRequired", err)
	}
	if got := stateOf(t, s, x.ID); got.State != StateBacklog || len(got.Relations) != 0 {
		t.Fatalf("refused duplicate changed the task: %+v", got)
	}
	// With it, both appear in one step.
	got, err := s.TransitionTaskWith(ctx, x.ID, TransitionInput{To: "duplicate", By: "tester", Note: "same", DuplicateOf: target.ID})
	if err != nil || got.State != StateDuplicate || got.Category != "canceled" || got.CanceledAt == nil {
		t.Fatalf("duplicate with duplicate_of: %+v err=%v", got, err)
	}
	if detail := stateOf(t, s, x.ID); len(detail.Relations) != 1 || detail.Relations[0].Kind != RelationDuplicateOf || detail.Relations[0].DstID != target.ID {
		t.Fatalf("relations: %+v", detail.Relations)
	}
	// A bad target rolls the whole step back: no relation and no state change.
	y := newTask(t, s, pool, "backlog")
	if _, err := s.TransitionTaskWith(ctx, y.ID, TransitionInput{To: "duplicate", By: "tester", Note: "x", DuplicateOf: 999999999}); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("duplicate of a missing task: err=%v, want ErrTaskNotFound", err)
	}
	if got := stateOf(t, s, y.ID); got.State != StateBacklog || len(got.Relations) != 0 {
		t.Fatalf("a failed duplicate must change nothing: %+v", got)
	}
	// duplicate_of given to another transition is invalid.
	if _, err := s.TransitionTaskWith(ctx, y.ID, TransitionInput{To: "icebox", By: "tester", Note: "x", DuplicateOf: target.ID}); err == nil {
		t.Fatal("duplicate_of on a non-duplicate transition must be refused")
	}
	// Adding the relation moves the task to duplicate in the same transaction.
	z := newTask(t, s, pool, "unstarted")
	if changed, err := s.AddTaskRelation(ctx, z.ID, RelationDuplicateOf, target.ID, "tester"); err != nil || !changed {
		t.Fatalf("add duplicate_of: changed=%t err=%v", changed, err)
	}
	if got := stateOf(t, s, z.ID); got.State != StateDuplicate {
		t.Fatalf("add duplicate_of left the task in %s", got.State)
	}
	if changed, err := s.AddTaskRelation(ctx, z.ID, RelationDuplicateOf, target.ID, "tester"); err != nil || changed {
		t.Fatalf("repeated add duplicate_of: changed=%t err=%v", changed, err)
	}
	// A duplicate keeps its edge until it is reopened.
	if _, err := s.RemoveTaskRelation(ctx, z.ID, RelationDuplicateOf, target.ID, "tester"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("removing the edge of a duplicate: err=%v, want ErrTaskConflict", err)
	}
	if _, err := s.ReopenTask(ctx, z.ID, "tester", "it is not a duplicate"); err != nil {
		t.Fatal(err)
	}
	if reread := stateOf(t, s, z.ID); reread.State != StateUnstarted || len(reread.Relations) != 0 {
		t.Fatalf("reopened duplicate: %+v", reread)
	}
	// The same pair can become a duplicate again after the archive.
	if changed, err := s.AddTaskRelation(ctx, z.ID, RelationDuplicateOf, target.ID, "tester"); err != nil || !changed {
		t.Fatalf("re-add duplicate_of: changed=%t err=%v", changed, err)
	}
	// A task that is already completed cannot become a duplicate by edge.
	done := newTask(t, s, pool, "merged")
	if _, err := s.AddTaskRelation(ctx, done.ID, RelationDuplicateOf, target.ID, "tester"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("duplicate_of on a merged task: err=%v, want ErrTaskConflict", err)
	}
}

// autoCloseFamily files a parent in parentState with children seeded in the
// given states.
func autoCloseFamily(t *testing.T, s *Store, pool *pgxpool.Pool, parentState string, childStates ...string) (Task, []Task) {
	t.Helper()
	parent := newTask(t, s, pool, parentState)
	kids := make([]Task, 0, len(childStates))
	for _, st := range childStates {
		kid := newTask(t, s, pool, st)
		setParent(t, s, kid, parent)
		kids = append(kids, kid)
	}
	return parent, kids
}

func move(t *testing.T, s *Store, id int64, to string) {
	t.Helper()
	if _, err := s.TransitionTask(t.Context(), id, to, "tester", "settle", nil, ""); err != nil {
		t.Fatalf("task %d -> %s: %v", id, to, err)
	}
}

// TestAutoCloseTable covers each row of the design's Hierarchy table, Q5 and Q6.
func TestAutoCloseTable(t *testing.T) {
	s, pool := searchTestStore(t)

	t.Run("a child not settled leaves the parent alone", func(t *testing.T) {
		parent, kids := autoCloseFamily(t, s, pool, "developing", "reviewing", "developing")
		move(t, s, kids[0].ID, "merged")
		if got := stateOf(t, s, parent.ID); got.State != StateDeveloping {
			t.Fatalf("parent=%s, want developing", got.State)
		}
	})

	for _, blocker := range []string{"triage", "backlog", "unstarted", "researching", "developing", "reviewing", "icebox"} {
		blocker := blocker
		t.Run("a "+blocker+" child blocks the close", func(t *testing.T) {
			parent, kids := autoCloseFamily(t, s, pool, "backlog", "reviewing", blocker)
			move(t, s, kids[0].ID, "merged")
			if got := stateOf(t, s, parent.ID); got.State != StateBacklog {
				t.Fatalf("a %s child must block the close: parent=%s", blocker, got.State)
			}
		})
	}

	t.Run("every completed child is deployed -> parent deployed", func(t *testing.T) {
		parent, kids := autoCloseFamily(t, s, pool, "reviewing", "deployed", "deployed", "reviewing")
		// A dropped child counts as settled and is ignored by the target rule.
		move(t, s, kids[2].ID, "dropped")
		got := stateOf(t, s, parent.ID)
		if got.State != StateDeployed || got.Category != "completed" || got.CompletedAt == nil {
			t.Fatalf("parent=%+v, want deployed", got)
		}
		ev := eventsOfKind(got, TaskEventTransition)
		last := ev[len(ev)-1]
		if last.Source != "hk.auto_close" || !last.Auto || last.To != StateDeployed || last.From != StateReviewing {
			t.Fatalf("auto-close event: %+v", last)
		}
	})

	t.Run("some completed child only merged -> parent merged", func(t *testing.T) {
		parent, kids := autoCloseFamily(t, s, pool, "developing", "deployed", "reviewing")
		move(t, s, kids[1].ID, "merged")
		if got := stateOf(t, s, parent.ID); got.State != StateMerged {
			t.Fatalf("parent=%s, want merged", got.State)
		}
	})

	t.Run("Q5: every child dropped or duplicate -> parent stays, skip recorded", func(t *testing.T) {
		parent, kids := autoCloseFamily(t, s, pool, "developing", "backlog", "backlog")
		other := newTask(t, s, pool, "backlog")
		move(t, s, kids[0].ID, "dropped")
		if _, err := s.TransitionTaskWith(t.Context(), kids[1].ID, TransitionInput{To: "duplicate", By: "tester", Note: "same", DuplicateOf: other.ID}); err != nil {
			t.Fatal(err)
		}
		got := stateOf(t, s, parent.ID)
		if got.State != StateDeveloping {
			t.Fatalf("parent=%s, want developing", got.State)
		}
		skips := eventsOfKind(got, TaskEventAutoClose)
		if len(skips) != 1 || skips[0].Applied || skips[0].Source != "hk.auto_close" || !skips[0].Auto || skips[0].From != StateDeveloping || skips[0].To != StateDeveloping {
			t.Fatalf("skip events: %+v", skips)
		}
	})

	t.Run("Q6: an icebox child is parked work, not finished work", func(t *testing.T) {
		parent, kids := autoCloseFamily(t, s, pool, "reviewing", "icebox", "reviewing")
		move(t, s, kids[1].ID, "merged")
		if got := stateOf(t, s, parent.ID); got.State != StateReviewing {
			t.Fatalf("parent=%s, want reviewing", got.State)
		}
		// Once the parked child is dropped, the rest settle and the parent closes.
		// icebox -> dropped is not a manual edge; revive it to backlog first.
		move(t, s, kids[0].ID, "backlog")
		move(t, s, kids[0].ID, "dropped")
		if got := stateOf(t, s, parent.ID); got.State != StateMerged {
			t.Fatalf("parent=%s, want merged after the parked child was dropped", got.State)
		}
	})

	t.Run("reopening a child records an event on a completed parent and does not reopen it", func(t *testing.T) {
		parent, kids := autoCloseFamily(t, s, pool, "reviewing", "reviewing")
		move(t, s, kids[0].ID, "merged")
		if got := stateOf(t, s, parent.ID); got.State != StateMerged {
			t.Fatalf("parent=%s, want merged", got.State)
		}
		if _, err := s.ReopenTask(t.Context(), kids[0].ID, "tester", "regressed"); err != nil {
			t.Fatal(err)
		}
		got := stateOf(t, s, parent.ID)
		if got.State != StateMerged {
			t.Fatalf("parent reopened with its child: %s", got.State)
		}
		notes := eventsOfKind(got, TaskEventAutoClose)
		if len(notes) != 1 || notes[0].Applied || notes[0].From != StateMerged || notes[0].To != StateMerged {
			t.Fatalf("reopen notice: %+v", notes)
		}
	})

	t.Run("it does not bypass triage or revive parked or canceled parents", func(t *testing.T) {
		for _, state := range []string{"triage", "icebox", "dropped", "duplicate", "merged"} {
			parent, kids := autoCloseFamily(t, s, pool, state, "reviewing")
			move(t, s, kids[0].ID, "merged")
			if got := stateOf(t, s, parent.ID); got.State != state {
				t.Fatalf("%s parent moved to %s", state, got.State)
			}
		}
	})

	t.Run("a closed parent closes its own parent", func(t *testing.T) {
		grand := newTask(t, s, pool, "developing")
		parent := newTask(t, s, pool, "developing")
		setParent(t, s, parent, grand)
		kid := newTask(t, s, pool, "reviewing")
		setParent(t, s, kid, parent)
		move(t, s, kid.ID, "merged")
		if got := stateOf(t, s, parent.ID); got.State != StateMerged {
			t.Fatalf("parent=%s", got.State)
		}
		if got := stateOf(t, s, grand.ID); got.State != StateMerged {
			t.Fatalf("grandparent=%s, want merged", got.State)
		}
	})

	t.Run("auto_close_parent off closes nothing", func(t *testing.T) {
		s.SetAutoCloseParent(false)
		defer s.SetAutoCloseParent(true)
		parent, kids := autoCloseFamily(t, s, pool, "developing", "reviewing")
		move(t, s, kids[0].ID, "merged")
		if got := stateOf(t, s, parent.ID); got.State != StateDeveloping {
			t.Fatalf("parent=%s, want developing with the setting off", got.State)
		}
	})
}
