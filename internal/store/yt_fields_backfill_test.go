package store

import (
	"errors"
	"testing"
)

// The issue_fields backfill (hk 4022) prepares the snapshot of every bound
// task whose snapshot differs from the last one prepared, by the consumer's
// rule, so a re-run is a no-op.

func ytfBind(t *testing.T, s *Store, task int64) {
	t.Helper()
	ytFeedExec(t, s, `INSERT INTO yt_task_bindings(task_id, tenant, issue_id, project_id, bound_by, bound_at) VALUES ($1,$2,$3,$4,'test',now())`,
		task, ytcTenant, ytfIssue(task), ytbProjectID)
}

func ytfBackfill(t *testing.T, s *Store, opt YTFieldsBackfillOptions) YTFieldsBackfillReport {
	t.Helper()
	r, err := s.BackfillYTFieldsOps(t.Context(), opt)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ytfAllOps(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM yt_write_ops WHERE kind='issue_fields'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestYTFieldsBackfillPreparesOnceThenOnChange(t *testing.T) {
	s, bound, unbound := ytfSetup(t, "ytfb_once")
	// Never projected: the feed saw no change of either task.
	if ops := ytfOps(t, s, bound); len(ops) != 0 {
		t.Fatalf("ops before the backfill %+v", ops)
	}
	r := ytfBackfill(t, s, YTFieldsBackfillOptions{})
	want := YTFieldsBackfillCounts{Scanned: 2, Prepared: 1, Unbound: 1}
	if r.Total != want || r.Tenant != ytcTenant || len(r.Projects) != 1 || r.Projects[0].Project != ytbProject || r.Projects[0].YTFieldsBackfillCounts != want {
		t.Fatalf("first run %+v %+v", r, r.Projects)
	}
	ops := ytfOps(t, s, bound)
	if len(ops) != 1 || ops[0].YTTargetID != ytfIssue(bound) || ops[0].Principal != YTFieldsPrincipal {
		t.Fatalf("ops %+v", ops)
	}
	if ops := ytfOps(t, s, unbound); len(ops) != 0 {
		t.Fatalf("unbound ops %+v", ops)
	}

	// Re-run: the snapshot is the one prepared.
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{}); r.Total != (YTFieldsBackfillCounts{Scanned: 2, Unchanged: 1, Unbound: 1}) {
		t.Fatalf("re-run %+v", r.Total)
	}
	// The feed sees no change either: the backfill shares its snapshot state.
	ytfPass(t, s)
	if ops := ytfOps(t, s, bound); len(ops) != 1 {
		t.Fatalf("ops after the re-run %+v", ops)
	}

	// A changed task (not yet seen by the feed) gets one more op.
	ytFeedExec(t, s, `UPDATE tasks SET priority=4 WHERE id=$1`, bound)
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{TaskID: bound}); r.Total != (YTFieldsBackfillCounts{Scanned: 1, Prepared: 1}) || r.TaskID != bound {
		t.Fatalf("changed %+v", r)
	}
	if ops := ytfOps(t, s, bound); len(ops) != 2 || ops[1].PayloadSHA == ops[0].PayloadSHA {
		t.Fatalf("ops after the change %+v", ops)
	}
	// The feed then finds the change already prepared.
	ytfPass(t, s)
	if ops := ytfOps(t, s, bound); len(ops) != 2 {
		t.Fatalf("ops after the feed %+v", ops)
	}
}

func TestYTFieldsBackfillSkipsMovedAndOtherTenant(t *testing.T) {
	s, bound, _ := ytfSetup(t, "ytfb_skip")
	other := ytcTask(t, s, ytbProject)
	ytfBind(t, s, other)
	ytFeedExec(t, s, `UPDATE yt_task_bindings SET tenant='other.youtrack.cloud' WHERE task_id=$1`, other)
	ytFeedExec(t, s, `UPDATE tasks SET project='ytfb-elsewhere' WHERE id=$1`, bound)
	r := ytfBackfill(t, s, YTFieldsBackfillOptions{})
	if r.Total != (YTFieldsBackfillCounts{Scanned: 3, Moved: 1, Unbound: 1, OtherTenant: 1}) || len(r.Projects) != 2 {
		t.Fatalf("report %+v %+v", r.Total, r.Projects)
	}
	for _, p := range r.Projects {
		if p.Project == "ytfb-elsewhere" && p.YTFieldsBackfillCounts != (YTFieldsBackfillCounts{Scanned: 1, Moved: 1}) {
			t.Fatalf("moved project %+v", p)
		}
	}
	if n := ytfAllOps(t, s); n != 0 {
		t.Fatalf("%d ops", n)
	}
	// --project narrows the walk.
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{Project: ytbProject}); r.Total != (YTFieldsBackfillCounts{Scanned: 2, Unbound: 1, OtherTenant: 1}) {
		t.Fatalf("project %+v", r.Total)
	}
}

func TestYTFieldsBackfillLimitAndDryRun(t *testing.T) {
	s, bound, _ := ytfSetup(t, "ytfb_limit")
	due := []int64{bound}
	for range 4 {
		task := ytcTask(t, s, ytbProject)
		ytfBind(t, s, task)
		due = append(due, task)
	}
	// Dry run: the counts of a real run, no op.
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{DryRun: true}); r.Total != (YTFieldsBackfillCounts{Scanned: 6, Prepared: 5, Unbound: 1}) || !r.DryRun {
		t.Fatalf("dry run %+v", r)
	}
	if n := ytfAllOps(t, s); n != 0 {
		t.Fatalf("dry run made %d ops", n)
	}
	for i, wantOps := range []int{2, 4, 5} {
		r := ytfBackfill(t, s, YTFieldsBackfillOptions{Limit: 2})
		if r.Total.Prepared != wantOps-2*i || ytfAllOps(t, s) != wantOps {
			t.Fatalf("run %d: %+v, %d ops", i, r.Total, ytfAllOps(t, s))
		}
	}
	// The ops went to the tasks in id order.
	for _, task := range due {
		if ops := ytfOps(t, s, task); len(ops) != 1 {
			t.Fatalf("task %d ops %+v", task, ops)
		}
	}
	// A dry run after a change counts it as due, the rest unchanged.
	ytFeedExec(t, s, `UPDATE tasks SET priority=6 WHERE id=$1`, due[2])
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{DryRun: true}); r.Total != (YTFieldsBackfillCounts{Scanned: 6, Prepared: 1, Unchanged: 4, Unbound: 1}) {
		t.Fatalf("dry run after the change %+v", r.Total)
	}
	if n := ytfAllOps(t, s); n != 5 {
		t.Fatalf("dry run made ops: %d", n)
	}
}

func TestYTFieldsBackfillRefusedModeOff(t *testing.T) {
	s, _, _ := ytfSetup(t, "ytfb_off")
	s.SetYTFieldsMode(YTFieldsOff, "")
	if _, err := s.BackfillYTFieldsOps(t.Context(), YTFieldsBackfillOptions{}); !errors.Is(err, ErrYTFieldsOff) {
		t.Fatalf("mode off: %v", err)
	}
	if n := ytfAllOps(t, s); n != 0 {
		t.Fatalf("mode off made %d ops", n)
	}
}
