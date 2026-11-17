package store

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Dialect-neutral cases of the project owner and overview reads (hk 5074,
// porter A5). They run on PostgreSQL in TestTasksAreaPostgres and on MySQL in
// TestMySQLTasksArea and use only public store methods (and, for a due date,
// the per-dialect fixture of task_threads_fixture_test.go).

func init() {
	registerTaskAreaCase("project/owner-events", projectCaseOwnerEvents)
	registerTaskAreaCase("project/owner-refusals", projectCaseOwnerRefusals)
	registerTaskAreaCase("project/owner-concurrent-changes", projectCaseOwnerConcurrent)
	registerTaskAreaCase("project/overview-counts-and-health", projectCaseOverview)
	registerTaskAreaCase("project/overview-assignees-are-bounded", projectCaseAssigneeBound)
}

func projectCaseOwnerEvents(t *testing.T, s *Store) {
	ctx := t.Context()
	name, other := a5Project(t, s), a5Project(t, s)
	got, err := s.GetProjectOwner(ctx, name)
	if err != nil || got.Name != name || got.DesignLane != "" || got.EffectiveOwner != OperatorDeskLane || got.UpdatedAt != nil {
		t.Fatalf("fresh owner %+v err=%v", got, err)
	}
	if hist, err := s.ProjectOwnerHistory(ctx, name, 0); err != nil || len(hist) != 0 {
		t.Fatalf("fresh history %+v err=%v", hist, err)
	}
	before := time.Now().UTC().Add(-time.Minute)
	set, err := s.SetProjectDesignLane(ctx, name, "design-1", "a5-actor", "first owner")
	if err != nil || set.DesignLane != "design-1" || set.EffectiveOwner != "design-1" || set.UpdatedAt == nil || set.UpdatedAt.Before(before) {
		t.Fatalf("set %+v err=%v", set, err)
	}
	got, err = s.GetProjectOwner(ctx, name)
	if err != nil || got.DesignLane != "design-1" || got.UpdatedAt == nil || got.UpdatedAt.Sub(*set.UpdatedAt).Abs() > time.Second {
		t.Fatalf("owner after set %+v err=%v", got, err)
	}
	// Setting the lane it already has writes no event.
	same, err := s.SetProjectDesignLane(ctx, name, "design-1", "a5-actor", "again")
	if err != nil || same.DesignLane != "design-1" || same.UpdatedAt == nil {
		t.Fatalf("same lane %+v err=%v", same, err)
	}
	if hist, err := s.ProjectOwnerHistory(ctx, name, 20); err != nil || len(hist) != 1 {
		t.Fatalf("history after a no-op set: %+v err=%v", hist, err)
	}
	if _, err := s.SetProjectDesignLane(ctx, name, "design-2", "a5-actor", "handover"); err != nil {
		t.Fatal(err)
	}
	cleared, err := s.SetProjectDesignLane(ctx, name, "", "a5-actor", "")
	if err != nil || cleared.DesignLane != "" || cleared.EffectiveOwner != OperatorDeskLane || cleared.UpdatedAt == nil {
		t.Fatalf("clear %+v err=%v", cleared, err)
	}
	hist, err := s.ProjectOwnerHistory(ctx, name, 20)
	if err != nil || len(hist) != 3 {
		t.Fatalf("history %+v err=%v", hist, err)
	}
	// Newest first, the empty lane is a real value, each event names the lane before it.
	want := []struct{ prev, lane, note string }{{"design-2", "", ""}, {"design-1", "design-2", "handover"}, {"", "design-1", "first owner"}}
	for i, w := range want {
		e := hist[i]
		if e.Project != name || e.PreviousLane != w.prev || e.DesignLane != w.lane || e.Note != w.note || e.Actor != "a5-actor" || e.CreatedAt.IsZero() || (i > 0 && e.ID >= hist[i-1].ID) {
			t.Fatalf("event %d = %+v, want %+v", i, e, w)
		}
	}
	if two, err := s.ProjectOwnerHistory(ctx, name, 2); err != nil || len(two) != 2 || two[0].ID != hist[0].ID {
		t.Fatalf("limited history %+v err=%v", two, err)
	}
	// The owners list holds every project, the claimed one with its lane.
	if _, err := s.SetProjectDesignLane(ctx, name, "design-3", "a5-actor", ""); err != nil {
		t.Fatal(err)
	}
	owners, err := s.ListProjectOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ProjectOwner{}
	for _, o := range owners {
		byName[o.Name] = o
	}
	if o := byName[name]; o.DesignLane != "design-3" || o.EffectiveOwner != "design-3" || o.UpdatedAt == nil {
		t.Fatalf("claimed project in list: %+v", o)
	}
	if o, ok := byName[other]; !ok || o.DesignLane != "" || o.EffectiveOwner != OperatorDeskLane || o.UpdatedAt != nil {
		t.Fatalf("unclaimed project in list: %+v ok=%v", o, ok)
	}
}

func projectCaseOwnerRefusals(t *testing.T, s *Store) {
	ctx := t.Context()
	name := a5Project(t, s)
	if _, err := s.SetProjectDesignLane(ctx, "no-such-"+name, "design-1", "a5-actor", ""); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project err=%v", err)
	}
	if _, err := s.GetProjectOwner(ctx, "no-such-"+name); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("get unknown project err=%v", err)
	}
	for _, bad := range []struct{ project, lane, actor, note string }{
		{"bad name", "design-1", "a5-actor", ""},
		{name, "bad lane", "a5-actor", ""},
		{name, "design-1", "", ""},
		{name, "design-1", "a5-actor", "token ghp_" + strings.Repeat("a", 36)},
	} {
		if _, err := s.SetProjectDesignLane(ctx, bad.project, bad.lane, bad.actor, bad.note); err == nil || errors.Is(err, ErrProjectNotFound) {
			t.Fatalf("SetProjectDesignLane(%+v) err=%v, want a validation refusal", bad, err)
		}
	}
	if _, err := s.GetProjectOwner(ctx, "bad name"); err == nil || errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("get malformed project err=%v", err)
	}
	if _, err := s.ProjectOwnerHistory(ctx, "bad name", 5); err == nil {
		t.Fatal("history of a malformed project was read")
	}
	// A refused change wrote nothing.
	if hist, err := s.ProjectOwnerHistory(ctx, name, 20); err != nil || len(hist) != 0 {
		t.Fatalf("history after refusals %+v err=%v", hist, err)
	}
	if got, err := s.GetProjectOwner(ctx, name); err != nil || got.DesignLane != "" {
		t.Fatalf("owner after refusals %+v err=%v", got, err)
	}
}

// Concurrent changes of one project's lane serialize on the project row: every
// change is one event, and each event's previous lane is the lane the event
// before it set.
func projectCaseOwnerConcurrent(t *testing.T, s *Store) {
	ctx := t.Context()
	name := a5Project(t, s)
	const writers = 6
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.SetProjectDesignLane(ctx, name, a5Name("design"), "a5-actor", "")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	hist, err := s.ProjectOwnerHistory(ctx, name, ProjectOwnerHistoryMax)
	if err != nil || len(hist) != writers {
		t.Fatalf("history %d events err=%v, want %d", len(hist), err, writers)
	}
	for i := len(hist) - 1; i >= 0; i-- {
		prev := ""
		if i < len(hist)-1 {
			prev = hist[i+1].DesignLane
		}
		if hist[i].PreviousLane != prev {
			t.Fatalf("event %d previous lane %q, want %q (history %+v)", hist[i].ID, hist[i].PreviousLane, prev, hist)
		}
	}
	if got, err := s.GetProjectOwner(ctx, name); err != nil || got.DesignLane != hist[0].DesignLane {
		t.Fatalf("owner %+v err=%v, want the newest event's lane %q", got, err, hist[0].DesignLane)
	}
}

func projectCaseOverview(t *testing.T, s *Store) {
	ctx := t.Context()
	project, empty := a5Project(t, s), a5Project(t, s)
	idle := a5Task(t, s, project, "waiting and overdue")
	claim := func(by, title string) Task {
		t.Helper()
		x := a5Task(t, s, project, title)
		x, err := s.ClaimTask(ctx, x.ID, by, a5Name("a5job"), "")
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	first, second, third := claim("alice", "first"), claim("alice", "second"), claim("bob", "third")
	blocked := a5Task(t, s, project, "blocked")
	for _, blocker := range []Task{second, first} {
		if _, err := s.AddTaskRelation(ctx, blocker.ID, RelationBlocks, blocked.ID, "a5-test"); err != nil {
			t.Fatal(err)
		}
	}
	done := a5Task(t, s, project, "done")
	a5Merge(t, s, done.ID, 5101)
	now := time.Now().UTC().Truncate(time.Second)
	due := now.Add(-48 * time.Hour)
	a5SetDue(t, s, idle.ID, due)
	if _, err := s.SetProjectDesignLane(ctx, project, "design-9", "a5-actor", ""); err != nil {
		t.Fatal(err)
	}

	overviews, err := s.ListProjectOverviews(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ProjectOverview{}
	for _, o := range overviews {
		byName[o.Name] = o
	}
	o, ok := byName[project]
	if !ok {
		t.Fatalf("project %s missing from %d overviews", project, len(overviews))
	}
	if o.DesignLane != "design-9" || o.EffectiveOwner != "design-9" || o.UpdatedAt == nil {
		t.Fatalf("owner fields %+v", o)
	}
	// The claimed tasks' states come from the claim itself.
	wantOpen := map[string]int{StateBacklog: 2}
	started := 0
	for _, x := range []Task{first, second, third} {
		wantOpen[x.State]++
		if IsStartedState(x.State) {
			started++
		}
	}
	if !maps.Equal(o.OpenCounts, wantOpen) || o.OpenTotal != 5 {
		t.Fatalf("open counts %v total %d, want %v total 5", o.OpenCounts, o.OpenTotal, wantOpen)
	}
	h := o.Health
	if h.InProgress != started || h.Waiting != 2 || h.Done != 1 || h.Blocked != 1 || h.Overdue != 1 {
		t.Fatalf("health %+v, want in_progress=%d waiting=2 done=1 blocked=1 overdue=1", h, started)
	}
	if len(h.Assignees) != 2 || h.Assignees[0] != (ProjectAssignee{Name: "alice", Open: 2}) || h.Assignees[1] != (ProjectAssignee{Name: "bob", Open: 1}) || h.AssigneesMore {
		t.Fatalf("assignees %+v more=%v", h.Assignees, h.AssigneesMore)
	}
	if len(h.TopBlocked) != 1 || h.TopBlocked[0].TaskID != blocked.ID || h.TopBlocked[0].Title != "blocked" || !slices.Equal(h.TopBlocked[0].BlockedBy, []int64{first.ID, second.ID}) {
		t.Fatalf("top blocked %+v, want %d blocked by %d,%d", h.TopBlocked, blocked.ID, first.ID, second.ID)
	}
	if len(h.TopOverdue) != 1 || h.TopOverdue[0].TaskID != idle.ID || !h.TopOverdue[0].DueAt.Equal(due) {
		t.Fatalf("top overdue %+v, want task %d due %s", h.TopOverdue, idle.ID, due)
	}
	// As of a time before the due date the task is not overdue.
	earlier, err := s.ListProjectOverviews(ctx, due.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range earlier {
		if e.Name == project && (e.Health.Overdue != 0 || len(e.Health.TopOverdue) != 0) {
			t.Fatalf("overdue before its due date: %+v", e.Health)
		}
	}
	// A project with no tasks has empty, non-nil lists and zero counts.
	e, ok := byName[empty]
	if !ok || e.OpenTotal != 0 || len(e.OpenCounts) != 0 || e.Health.Assignees == nil || e.Health.TopBlocked == nil || e.Health.TopOverdue == nil ||
		e.Health.InProgress+e.Health.Waiting+e.Health.Blocked+e.Health.Done+e.Health.Overdue != 0 {
		t.Fatalf("empty project %+v ok=%v", e, ok)
	}
	if e.DesignLane != "" || e.EffectiveOwner != OperatorDeskLane {
		t.Fatalf("empty project owner %+v", e.ProjectOwner)
	}
}

func projectCaseAssigneeBound(t *testing.T, s *Store) {
	ctx := t.Context()
	project := a5Project(t, s)
	const claimers = projectAssigneeLimit + 2
	for i := range claimers {
		x := a5Task(t, s, project, "claimed "+a5Name("n"))
		if _, err := s.ClaimTask(ctx, x.ID, "worker-"+string(rune('a'+i)), a5Name("a5job"), ""); err != nil {
			t.Fatal(err)
		}
	}
	overviews, err := s.ListProjectOverviews(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range overviews {
		if o.Name != project {
			continue
		}
		if len(o.Health.Assignees) != projectAssigneeLimit || !o.Health.AssigneesMore || o.OpenTotal != claimers {
			t.Fatalf("assignees %d more=%v open=%d, want %d more=true open=%d", len(o.Health.Assignees), o.Health.AssigneesMore, o.OpenTotal, projectAssigneeLimit, claimers)
		}
		// Equal counts list by name.
		if !slices.IsSortedFunc(o.Health.Assignees, func(a, b ProjectAssignee) int { return strings.Compare(a.Name, b.Name) }) {
			t.Fatalf("assignees not in name order: %+v", o.Health.Assignees)
		}
		return
	}
	t.Fatalf("project %s missing", project)
}
