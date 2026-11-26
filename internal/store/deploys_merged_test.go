package store

import (
	"context"
	"testing"
	"time"
)

// ListMergedTaskEvents returns one row per merged task (its latest merge),
// newest first, and leaves out tasks that are not merged. Both dialects run
// it; the read drives from merged tasks and their per-task latest merge.
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
	if len(order) != 2 || order[0] != first.ID || order[1] != second.ID {
		t.Fatalf("merged order=%v, want [%d %d]", order, first.ID, second.ID)
	}
	// Add same-time latest merges to both tasks. The id tie-break means the
	// second inserted merge leads, while each task still appears only once.
	tiedAt := time.Now().UTC().Add(time.Hour)
	insertMerge := `INSERT INTO task_events(task_id, "from", "to", "by", at) VALUES($1,'join','merged','tester',$2)`
	insertMergeMySQL := "INSERT INTO task_events(task_id, `from`, `to`, `by`, at) VALUES(?, 'join','merged','tester',?)"
	for _, id := range []int64{first.ID, second.ID} {
		if _, err := s.db().Exec(ctx, s.q(insertMerge, insertMergeMySQL), id, tiedAt); err != nil {
			t.Fatal(err)
		}
	}
	events, err = s.ListMergedTaskEvents(ctx, 2000)
	if err != nil {
		t.Fatal(err)
	}
	order = order[:0]
	for _, e := range events {
		if e.TaskID == first.ID || e.TaskID == second.ID {
			order = append(order, e.TaskID)
		}
	}
	if len(order) != 2 || order[0] != second.ID || order[1] != first.ID {
		t.Fatalf("merged order with tied timestamps=%v, want [%d %d]", order, second.ID, first.ID)
	}
}
