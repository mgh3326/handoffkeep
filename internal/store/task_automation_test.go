package store

import (
	"fmt"
	"testing"
	"time"
)

// ageTask moves a task's activity back to at. task_events and task_comments
// are append-only, so it is used only on tasks that have neither yet: their
// activity is created_at and the jobs bound to them.
func ageTask(t *testing.T, s *Store, id int64, at time.Time) {
	t.Helper()
	ctx := t.Context()
	var events, comments int
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM task_events WHERE task_id=$1),(SELECT count(*) FROM task_comments WHERE task_id=$1)`, id).Scan(&events, &comments); err != nil {
		t.Fatal(err)
	}
	if events != 0 || comments != 0 {
		t.Fatalf("ageTask: task %d already has %d events and %d comments", id, events, comments)
	}
	for _, q := range []string{
		`UPDATE tasks SET created_at=$2 WHERE id=$1`,
		`UPDATE jobs SET updated_at=$2 WHERE task_id=$1`,
	} {
		if _, err := s.pool.Exec(ctx, q, id, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsStalledAtFixedTimes(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	after := 24 * time.Hour
	for _, tc := range []struct {
		name     string
		state    string
		awaiting bool
		activity time.Time
		want     bool
	}{
		{"just inside", StateDeveloping, false, now.Add(-after + time.Second), false},
		{"exactly at the setting", StateDeveloping, false, now.Add(-after), false},
		{"just past", StateDeveloping, false, now.Add(-after - time.Second), true},
		{"reviewing past", StateReviewing, false, now.Add(-48 * time.Hour), true},
		{"awaiting decision", StateResearching, true, now.Add(-48 * time.Hour), false},
		{"backlog", StateBacklog, false, now.Add(-48 * time.Hour), false},
		{"unstarted", StateUnstarted, false, now.Add(-48 * time.Hour), false},
		{"merged", StateMerged, false, now.Add(-48 * time.Hour), false},
		{"icebox", StateIcebox, false, now.Add(-48 * time.Hour), false},
	} {
		if got := isStalled(tc.state, tc.awaiting, tc.activity, now, after); got != tc.want {
			t.Errorf("%s: stalled=%t, want %t", tc.name, got, tc.want)
		}
	}
}

func TestStartedStalledHoursSetting(t *testing.T) {
	var s Store
	if s.StartedStalledHours() != 24*time.Hour {
		t.Fatalf("default %v", s.StartedStalledHours())
	}
	s.SetStartedStalledHours(6)
	if s.StartedStalledHours() != 6*time.Hour {
		t.Fatalf("set %v", s.StartedStalledHours())
	}
	s.SetStartedStalledHours(0)
	if s.StartedStalledHours() != 24*time.Hour {
		t.Fatalf("reset %v", s.StartedStalledHours())
	}
}

func TestStalledFlagAtReadTime(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	s.SetStartedStalledHours(24)
	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour)

	inside := flowTask(t, s, pool, "implement", StateDeveloping, true)
	ageTask(t, s, inside.ID, now.Add(-24*time.Hour+time.Minute))
	past := flowTask(t, s, pool, "implement", StateDeveloping, true)
	ageTask(t, s, past.ID, now.Add(-24*time.Hour-time.Minute))
	if stateOf(t, s, inside.ID).Stalled {
		t.Fatal("a task just inside the setting is stalled")
	}
	if !stateOf(t, s, past.ID).Stalled {
		t.Fatal("a task just past the setting is not stalled")
	}

	awaiting := flowTask(t, s, pool, "implement", StateReviewing, true)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET awaiting_decision=true WHERE id=$1`, awaiting.ID); err != nil {
		t.Fatal(err)
	}
	ageTask(t, s, awaiting.ID, old)
	if stateOf(t, s, awaiting.ID).Stalled {
		t.Fatal("a task awaiting a decision is stalled")
	}

	commented := flowTask(t, s, pool, "implement", StateResearching, true)
	ageTask(t, s, commented.ID, old)
	if !stateOf(t, s, commented.ID).Stalled {
		t.Fatal("aged task is not stalled")
	}
	if _, err := s.CreateTaskComment(ctx, commented.ID, "tester", "still on it"); err != nil {
		t.Fatal(err)
	}
	if stateOf(t, s, commented.ID).Stalled {
		t.Fatal("a comment did not reset stalled")
	}

	jobbed := flowTask(t, s, pool, "implement", StateDeveloping, true)
	jobID := fmt.Sprintf("job-stalled-%d", jobbed.ID)
	if _, _, err := s.ClaimJob(ctx, Job{JobID: jobID, TaskID: jobbed.ID, Host: "test-host", CreatedBy: "tester"}); err != nil {
		t.Fatal(err)
	}
	ageTask(t, s, jobbed.ID, old)
	if !stateOf(t, s, jobbed.ID).Stalled {
		t.Fatal("aged task with an aged job is not stalled")
	}
	label := "progress"
	if _, err := s.UpdateJob(ctx, jobID, JobUpdate{Label: &label}); err != nil {
		t.Fatal(err)
	}
	if stateOf(t, s, jobbed.ID).Stalled {
		t.Fatal("a job update did not reset stalled")
	}

	var notStarted []int64
	for _, state := range []string{StateTriage, StateBacklog, StateUnstarted, StateMerged, StateDeployed, StateDropped, StateDuplicate, StateIcebox} {
		x := flowTask(t, s, pool, "implement", state, true)
		ageTask(t, s, x.ID, old)
		notStarted = append(notStarted, x.ID)
		if stateOf(t, s, x.ID).Stalled {
			t.Fatalf("%s task is stalled", state)
		}
	}

	xs, err := s.ListTasksFiltered(ctx, TaskQuery{Stalled: true, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, x := range xs {
		if !x.Stalled {
			t.Fatalf("stalled filter returned a row not marked stalled: %d", x.ID)
		}
		got[x.ID] = true
	}
	if !got[past.ID] || got[inside.ID] || got[awaiting.ID] || got[commented.ID] || got[jobbed.ID] {
		t.Fatalf("stalled list %v", got)
	}
	for _, id := range notStarted {
		if got[id] {
			t.Fatalf("stalled list has non-started task %d", id)
		}
	}
	// The unfiltered list marks the same rows.
	all, err := s.ListTasks(ctx, "", "", "", nil, nil, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range all {
		if x.Stalled != got[x.ID] {
			t.Fatalf("list stalled=%t for %d, filter says %t", x.Stalled, x.ID, got[x.ID])
		}
	}
}

func TestBacklogSweep(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	old := time.Now().UTC().Add(-15 * 24 * time.Hour)

	idle := flowTask(t, s, pool, "implement", StateBacklog, false)
	flagged := flowTask(t, s, pool, "implement", StateBacklog, false)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET awaiting_decision=true WHERE id=$1`, flagged.ID); err != nil {
		t.Fatal(err)
	}
	parent := flowTask(t, s, pool, "implement", StateBacklog, false)
	child := flowTask(t, s, pool, "implement", StateDeveloping, true)
	setParent(t, s, child, parent)
	unstarted := flowTask(t, s, pool, "implement", StateUnstarted, false)
	fresh := flowTask(t, s, pool, "implement", StateBacklog, false)
	for _, x := range []Task{idle, flagged, parent, unstarted} {
		ageTask(t, s, x.ID, old)
	}
	snapshot := func() map[int64]Task {
		out := map[int64]Task{}
		for _, x := range []Task{idle, flagged, parent, child, unstarted, fresh} {
			out[x.ID] = stateOf(t, s, x.ID)
		}
		return out
	}
	before := snapshot()
	unchanged := func(label string, ids ...int64) {
		t.Helper()
		now := snapshot()
		for _, id := range ids {
			if now[id].State != before[id].State || len(now[id].Events) != len(before[id].Events) {
				t.Fatalf("%s: task %d changed %s -> %s", label, id, before[id].State, now[id].State)
			}
		}
	}

	// Off by default: days 0 moves nothing.
	if moved, err := s.SweepBacklog(ctx, 0, false); err != nil || len(moved) != 0 {
		t.Fatalf("off: moved=%v err=%v", moved, err)
	}
	unchanged("off", idle.ID, flagged.ID, parent.ID, unstarted.ID, fresh.ID)

	// The dry run names the idle task and writes nothing.
	preview, err := s.SweepBacklog(ctx, 14, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview) != 1 || preview[0].ID != idle.ID {
		t.Fatalf("dry run %+v", preview)
	}
	unchanged("dry run", idle.ID, flagged.ID, parent.ID, unstarted.ID, fresh.ID)

	moved, err := s.SweepBacklog(ctx, 14, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved) != 1 || moved[0].ID != idle.ID {
		t.Fatalf("moved %+v", moved)
	}
	if got := stateOf(t, s, idle.ID).State; got != StateIcebox {
		t.Fatalf("idle backlog -> %s", got)
	}
	rows := sourceRows(t, s, idle.ID, SourceSweep)
	if len(rows) != 1 || !rows[0].Applied || rows[0].From != StateBacklog || rows[0].To != StateIcebox {
		t.Fatalf("sweep rows %+v", rows)
	}
	unchanged("skips", flagged.ID, parent.ID, child.ID, unstarted.ID, fresh.ID)

	// A second run finds nothing.
	if again, err := s.SweepBacklog(ctx, 14, false); err != nil || len(again) != 0 {
		t.Fatalf("second run moved=%v err=%v", again, err)
	}
}
