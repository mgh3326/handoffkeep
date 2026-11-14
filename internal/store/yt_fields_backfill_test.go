package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
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
	want := YTFieldsBackfillCounts{Scanned: 2, Ops: 1, Prepared: 1, Unbound: 1}
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
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{TaskID: bound}); r.Total != (YTFieldsBackfillCounts{Scanned: 1, Ops: 1, Prepared: 1}) || r.TaskID != bound {
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
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{DryRun: true}); r.Total != (YTFieldsBackfillCounts{Scanned: 6, Ops: 5, Prepared: 5, Unbound: 1}) || !r.DryRun {
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
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{DryRun: true}); r.Total != (YTFieldsBackfillCounts{Scanned: 6, Ops: 1, Prepared: 1, Unchanged: 4, Unbound: 1}) {
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

// The 18 issue_fields ops rejected field_refused before hk 4051 (hk 4057): a
// backfill re-prepares exactly the tasks whose last snapshot op was rejected,
// once, and leaves the sent ones unchanged.
func TestYTFieldsBackfillRetriesRejected(t *testing.T) {
	s, bound, _ := ytfSetup(t, "ytfb_rejected")
	tasks := []int64{bound}
	for range 3 {
		task := ytcTask(t, s, ytbProject)
		ytfBind(t, s, task)
		tasks = append(tasks, task)
	}
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{}); r.Total.Prepared != 4 {
		t.Fatalf("first run %+v", r.Total)
	}
	// Two refused, one applied, one still prepared.
	ytSetOpState(t, s, ytfOps(t, s, tasks[0])[0].ID, ytwrite.StateRejected)
	ytSetOpState(t, s, ytfOps(t, s, tasks[1])[0].ID, ytwrite.StateApplied)
	ytSetOpState(t, s, ytfOps(t, s, tasks[3])[0].ID, ytwrite.StateRejected)

	want := YTFieldsBackfillCounts{Scanned: 5, Ops: 2, Prepared: 2, Unchanged: 2, Unbound: 1}
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{DryRun: true}); r.Total != want {
		t.Fatalf("dry run %+v, want %+v", r.Total, want)
	}
	if n := ytfAllOps(t, s); n != 4 {
		t.Fatalf("dry run made ops: %d", n)
	}
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{}); r.Total != want {
		t.Fatalf("retry run %+v, want %+v", r.Total, want)
	}
	for i, wantOps := range []int{2, 1, 1, 2} {
		ops := ytfOps(t, s, tasks[i])
		if len(ops) != wantOps {
			t.Fatalf("task %d ops %+v", tasks[i], ops)
		}
		if wantOps == 2 && (ops[1].PayloadSHA != ops[0].PayloadSHA || ops[1].Key == ops[0].Key || ops[1].State != ytwrite.StatePrepared) {
			t.Fatalf("task %d retry %+v", tasks[i], ops)
		}
	}
	// The retries are pending: a re-run prepares nothing.
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{}); r.Total != (YTFieldsBackfillCounts{Scanned: 5, Unchanged: 4, Unbound: 1}) {
		t.Fatalf("re-run %+v", r.Total)
	}
}

// A moved task makes two ops, its move and its snapshot, and --limit counts
// both (hk 4112): a limit of 2 stops after the first moved task, not after
// two of them (four ops).
func TestYTFieldsBackfillLimitCountsMoves(t *testing.T) {
	s, bound, _ := ytmSetup(t, "ytfb_limit_moves")
	ytmMapped(t, s)
	tasks := []int64{bound}
	for range 2 {
		task := ytcTask(t, s, ytbProject)
		ytfBind(t, s, task)
		tasks = append(tasks, task)
	}
	for _, task := range tasks {
		ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, task, ytmProject)
	}
	r := ytfBackfill(t, s, YTFieldsBackfillOptions{Limit: 2})
	if r.Total != (YTFieldsBackfillCounts{Scanned: 1, Ops: 2, Moved: 1}) {
		t.Fatalf("limit 2 with moves: %+v", r.Total)
	}
	if moves, fields := ytmOps(t, s, tasks[0]), ytfOps(t, s, tasks[0]); len(moves) != 1 || len(fields) != 1 {
		t.Fatalf("first task: moves %+v snapshots %+v", moves, fields)
	}
	for _, task := range tasks[1:] {
		if moves, fields := ytmOps(t, s, task), ytfOps(t, s, task); len(moves) != 0 || len(fields) != 0 {
			t.Fatalf("task %d past the limit: moves %+v snapshots %+v", task, moves, fields)
		}
	}
}

// A moved task needs two ops. A one-op budget must leave it untouched instead
// of preparing both ops and exceeding the limit.
func TestYTFieldsBackfillLimitDoesNotSplitMovedTask(t *testing.T) {
	s, bound, _ := ytmSetup(t, "ytfb_limit_one_move")
	ytmMapped(t, s)
	ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, bound, ytmProject)

	r := ytfBackfill(t, s, YTFieldsBackfillOptions{Limit: 1})
	if r.Total != (YTFieldsBackfillCounts{}) {
		t.Fatalf("limit 1 prepared part of a moved task: %+v", r.Total)
	}
	if moves, fields := ytmOps(t, s, bound), ytfOps(t, s, bound); len(moves) != 0 || len(fields) != 0 {
		t.Fatalf("limit 1 wrote partial task ops: moves %+v snapshots %+v", moves, fields)
	}

	r = ytfBackfill(t, s, YTFieldsBackfillOptions{Limit: 2})
	if r.Total != (YTFieldsBackfillCounts{Scanned: 1, Ops: 2, Moved: 1}) {
		t.Fatalf("limit 2 did not resume the moved task: %+v", r.Total)
	}
}

// A task whose ops do not fit what is left of --limit is skipped, not a stop
// (PR #385 review): a one-op task behind a two-op moved one is still prepared
// by a --limit 1 run, so that run cannot stall behind the moved task, and the
// report says the moved task was left over.
func TestYTFieldsBackfillLimitSkipsOverBudgetTask(t *testing.T) {
	s, bound, unbound := ytmSetup(t, "ytfb_limit_skip")
	ytmMapped(t, s)
	// The moved task comes first by id and needs two ops; the later one needs one.
	ytFeedExec(t, s, `UPDATE tasks SET project=$2 WHERE id=$1`, bound, ytmProject)
	later := ytcTask(t, s, ytbProject)
	ytfBind(t, s, later)
	logs := ytbLog(t)
	// enqueued counts the ytwrite.fields_enqueued lines logged for a task: one per
	// snapshot op that was kept, none for a rolled-back or counted-only one.
	enqueued := func(task int64) int {
		n := 0
		for _, line := range strings.Split(logs(), "\n") {
			if strings.Contains(line, "ytwrite.fields_enqueued ") && strings.Contains(line, fmt.Sprintf(" task_id=%d ", task)) {
				n++
			}
		}
		return n
	}

	r := ytfBackfill(t, s, YTFieldsBackfillOptions{Limit: 1})
	if want := (YTFieldsBackfillCounts{Scanned: 2, Ops: 1, Prepared: 1, Unbound: 1}); r.Total != want || r.OverLimit != 1 {
		t.Fatalf("limit 1: total %+v over_limit %d, want %+v and 1 (the unbound task %d is read, the moved task %d is not)", r.Total, r.OverLimit, want, unbound, bound)
	}
	if moves, fields := ytmOps(t, s, bound), ytfOps(t, s, bound); len(moves) != 0 || len(fields) != 0 {
		t.Fatalf("over-limit moved task was touched: moves %+v snapshots %+v", moves, fields)
	}
	if fields := ytfOps(t, s, later); len(fields) != 1 {
		t.Fatalf("later one-op task snapshots %+v, want 1", fields)
	}
	if got, rolledBack := enqueued(later), enqueued(bound); got != 1 || rolledBack != 0 {
		t.Fatalf("fields_enqueued lines: later %d (want 1), over-limit moved task %d (want 0, its ops were rolled back)", got, rolledBack)
	}

	// With room for the two ops the moved task is prepared; a dry run reports the
	// same over-limit count as a real one.
	if r := ytfBackfill(t, s, YTFieldsBackfillOptions{Limit: 1, DryRun: true}); r.OverLimit != 1 {
		t.Fatalf("dry run over_limit %d, want 1", r.OverLimit)
	}
	if ytfBackfill(t, s, YTFieldsBackfillOptions{DryRun: true}); enqueued(bound) != 0 {
		t.Fatal("a dry run logged an enqueued op it never made")
	}
	r = ytfBackfill(t, s, YTFieldsBackfillOptions{Limit: 2})
	if r.Total != (YTFieldsBackfillCounts{Scanned: 1, Ops: 2, Moved: 1}) || r.OverLimit != 0 {
		t.Fatalf("limit 2: total %+v over_limit %d", r.Total, r.OverLimit)
	}
	if got := enqueued(bound); got != 1 {
		t.Fatalf("fields_enqueued lines for the moved task after it was prepared: %d, want 1", got)
	}
}
