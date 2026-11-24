package store

import (
	"context"
	"testing"
)

// ListMergedTaskEvents returns one row per merged task (its latest merge),
// newest first, and leaves out tasks that are not merged. Both dialects run
// it; the MySQL read is a LATERAL per merged task (hk 6222).
func TestListMergedTaskEventsLatestMergeFirst(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	lane := relaneLane(t, "merged-events")
	merge := func(x Task, job string) {
		t.Helper()
		if _, err := s.ClaimTask(ctx, x.ID, "builder", job, ""); err != nil {
			t.Fatal(err)
		}
		for _, to := range []string{"in_progress", "join", "merged"} {
			if _, err := s.TransitionTask(ctx, x.ID, to, "tester", "step", nil, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	first := relaneTask(t, s, lane, "merged first")
	second := relaneTask(t, s, lane, "merged second")
	open := relaneTask(t, s, lane, "never merged")
	merge(first, "job-first-1")
	merge(second, "job-second-1")
	if _, err := s.ReopenTask(ctx, first.ID, "tester", "re-run after smoke"); err != nil {
		t.Fatal(err)
	}
	merge(first, "job-first-2")

	events, err := s.ListMergedTaskEvents(ctx, 2000)
	if err != nil {
		t.Fatal(err)
	}
	var order []int64
	for _, e := range events {
		if e.TaskID == open.ID {
			t.Fatalf("task that never merged listed: %+v", e)
		}
		if e.TaskID == first.ID || e.TaskID == second.ID {
			order = append(order, e.TaskID)
		}
	}
	// first's second merge is the newest merge, so first leads the page; it
	// appears once even though it has two merge transitions.
	if len(order) != 2 || order[0] != first.ID || order[1] != second.ID {
		t.Fatalf("merged order=%v, want [%d %d]", order, first.ID, second.ID)
	}
}
