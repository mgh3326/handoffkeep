package store

import (
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// `youtrack links plan` (hk 4731): the read-only report of what the first
// issue-link sweep would write, against the ytwritetest fake and a throwaway
// schema. Every test runs with the mode off.

// ytlpTask creates a task in the mapped project (ytbBinding needs it).
func ytlpTask(t *testing.T, s *Store, title string) Task {
	t.Helper()
	project := ytbProject
	task, err := s.CreateTask(t.Context(), Task{Lane: "ytl-plan-test", Title: title, Kind: "implement",
		CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func ytlpIssue(task Task) string { return "2-" + strconv.FormatInt(task.ID, 10) }

// ytlpOrder returns the pair ascending by id: a related edge is stored with
// src < dst, and the lower id is its writer.
func ytlpOrder(a, b Task) (Task, Task) {
	if a.ID > b.ID {
		return b, a
	}
	return a, b
}

// ytlpEnv is one schema with its project, mapping and mode.
func ytlpEnv(t *testing.T, prefix string) *Store {
	t.Helper()
	s := schemaStore(t, prefix)
	if _, err := s.AddTaskProject(t.Context(), ytbProject, "test"); err != nil {
		t.Fatal(err)
	}
	ytbMapped(t, s)
	s.SetYTLinksMode(YTLinksOff, ytcTenant)
	return s
}

// ytlpOwned seeds an open yt_link_owned row, the record that lets the send
// delete a link (I1).
func ytlpOwned(t *testing.T, s *Store, linkType, kind, from, to string, writer, target int64) {
	t.Helper()
	ytFeedExec(t, s, `INSERT INTO yt_link_owned(tenant, link_type, from_issue, to_issue, writer_task_id, target_task_id,
		kind, intended_op_id, intended_at) VALUES ($1,$2,$3,$4,$5,$6,$7,1,$8)`,
		ytcTenant, linkType, from, to, writer, target, kind, time.Now().UTC())
}

// ytlpRowCounts is every table the plan must leave unchanged.
func ytlpRowCounts(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, table := range []string{"yt_write_ops", "yt_link_owned", "yt_link_unowned", "yt_link_plans",
		"yt_link_steps", "yt_links_sweep", "sync_log", "task_events", "yt_feed_cursors"} {
		var n int64
		if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

// ytlpTaskStamps is every task's updated_at: a plan never touches a task.
func ytlpTaskStamps(t *testing.T, s *Store) map[int64]time.Time {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), `SELECT id, updated_at FROM tasks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			t.Fatal(err)
		}
		out[id] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func ytlpLink(kind string, task Task) YTLinkPlanLink {
	return YTLinkPlanLink{Kind: kind, TargetTaskID: task.ID, TargetIssueID: ytlpIssue(task)}
}

// A relates edge whose link the issue does not show is a would_add, and the
// run writes nothing: no row of any table, no task row, and the fake sees
// reads only (I1, one GET per bound task).
func TestYTLinksPlanWouldAddAndWritesNothing(t *testing.T) {
	s := ytlpEnv(t, "ytl_plan_add")
	ctx := t.Context()
	lo, hi := ytlpOrder(ytlpTask(t, s, "lower"), ytlpTask(t, s, "higher"))
	ytbBind(t, s, ytbBinding(lo.ID))
	ytbBind(t, s, ytbBinding(hi.ID))
	if _, err := s.AddTaskRelation(ctx, hi.ID, RelationRelated, lo.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	fake := ytwritetest.NewFake()

	rowsBefore := ytlpRowCounts(t, s)
	stampsBefore := ytlpTaskStamps(t, s)
	rep, err := s.PlanYTLinks(ctx, YTLinksPlanOptions{}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ytlpRowCounts(t, s), rowsBefore; !reflect.DeepEqual(got, want) {
		t.Fatalf("plan wrote rows: before %v, after %v", want, got)
	}
	if got, want := ytlpTaskStamps(t, s), stampsBefore; len(got) != len(want) {
		t.Fatalf("plan changed the task set: before %v, after %v", want, got)
	} else {
		for id, at := range want {
			if !got[id].Equal(at) {
				t.Fatalf("plan touched task %d: %v → %v", id, at, got[id])
			}
		}
	}
	for _, c := range fake.LinkCalls() {
		if c.Op != ytwritetest.LinkOpRead {
			t.Fatalf("plan called YouTrack with %q", c.Op)
		}
	}
	if got := len(fake.LinkCalls()); got != 2 {
		t.Fatalf("link calls = %d, want one read per bound task (2)", got)
	}

	if rep.Mode != YTLinksOff || rep.Tenant != ytcTenant || rep.Tasks != 2 || rep.Skipped != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %+v, want only the task with a write", rep.Rows)
	}
	row := rep.Rows[0]
	if row.TaskID != lo.ID || row.IssueID != ytlpIssue(lo) {
		t.Fatalf("row task = %d %q, want %d %q", row.TaskID, row.IssueID, lo.ID, ytlpIssue(lo))
	}
	if row.WouldAdd != 1 || row.WouldDeleteOwned != 0 || row.UnownedLeft != 0 || row.Held != 0 || row.AlreadyOK != 0 {
		t.Fatalf("row counts = %+v", row.YTLinkPlanCounts)
	}
	if want := []YTLinkPlanLink{ytlpLink("relates", hi)}; !reflect.DeepEqual(row.Add, want) {
		t.Fatalf("add = %+v, want %+v", row.Add, want)
	}
	if len(row.Delete) != 0 {
		t.Fatalf("delete = %+v", row.Delete)
	}
	if want := (YTLinkPlanCounts{WouldAdd: 1}); rep.Totals != want {
		t.Fatalf("totals = %+v, want %+v", rep.Totals, want)
	}
}

// An owned link the edge no longer wants is a would_delete_owned; the record,
// not the edge, decides (I1).
func TestYTLinksPlanWouldDeleteOwned(t *testing.T) {
	s := ytlpEnv(t, "ytl_plan_del")
	ctx := t.Context()
	lo, hi := ytlpOrder(ytlpTask(t, s, "lower"), ytlpTask(t, s, "higher"))
	ytbBind(t, s, ytbBinding(lo.ID))
	ytbBind(t, s, ytbBinding(hi.ID))
	if _, err := s.AddTaskRelation(ctx, lo.ID, RelationRelated, hi.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	fake := ytwritetest.NewFake()
	fake.SeedIssueLink(ytlpIssue(lo), ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: ytlpIssue(hi)})
	ytlpOwned(t, s, "relates", "related", ytlpIssue(lo), ytlpIssue(hi), lo.ID, hi.ID)
	if _, err := s.RemoveTaskRelation(ctx, lo.ID, RelationRelated, hi.ID, "tester"); err != nil {
		t.Fatal(err)
	}

	rep, err := s.PlanYTLinks(ctx, YTLinksPlanOptions{}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %+v", rep.Rows)
	}
	row := rep.Rows[0]
	if row.TaskID != lo.ID || row.WouldDeleteOwned != 1 || row.WouldAdd != 0 || row.UnownedLeft != 0 {
		t.Fatalf("row = %+v", row)
	}
	if want := []YTLinkPlanLink{ytlpLink("relates", hi)}; !reflect.DeepEqual(row.Delete, want) {
		t.Fatalf("delete = %+v, want %+v", row.Delete, want)
	}
	if want := (YTLinkPlanCounts{WouldDeleteOwned: 1}); rep.Totals != want {
		t.Fatalf("totals = %+v, want %+v", rep.Totals, want)
	}
	// The plan deleted nothing from the fake either.
	if got := fake.IssueLinks(ytlpIssue(lo)); len(got) != 1 {
		t.Fatalf("fake links after plan = %+v", got)
	}
}

// A managed link with no owned row is unowned_left, import-era links included:
// it is counted and never deleted (I7).
func TestYTLinksPlanUnownedLeftIsNeverDeleted(t *testing.T) {
	s := ytlpEnv(t, "ytl_plan_unowned")
	child := ytlpTask(t, s, "child")
	ytbBind(t, s, ytbBinding(child.ID))
	fake := ytwritetest.NewFake()
	fake.SeedIssueLink(ytlpIssue(child), ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: "2-999"})

	rep, err := s.PlanYTLinks(t.Context(), YTLinksPlanOptions{}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 0 {
		t.Fatalf("rows = %+v, want none: an unowned link is not a write", rep.Rows)
	}
	if want := (YTLinkPlanCounts{UnownedLeft: 1}); rep.Totals != want {
		t.Fatalf("totals = %+v, want %+v", rep.Totals, want)
	}
	if got := fake.IssueLinks(ytlpIssue(child)); len(got) != 1 {
		t.Fatalf("unowned link removed by the plan: %+v", got)
	}
}

// An edge whose target task has no binding is held: neither written nor
// removed (I14).
func TestYTLinksPlanHeldTarget(t *testing.T) {
	s := ytlpEnv(t, "ytl_plan_held")
	child, parent := ytlpTask(t, s, "child"), ytlpTask(t, s, "unbound parent")
	ytbBind(t, s, ytbBinding(child.ID))
	if _, _, err := s.SetTaskParent(t.Context(), child.ID, &parent.ID, "tester"); err != nil {
		t.Fatal(err)
	}

	rep, err := s.PlanYTLinks(t.Context(), YTLinksPlanOptions{}, ytwritetest.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tasks != 1 || rep.Skipped != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Rows) != 0 {
		t.Fatalf("rows = %+v, want none for a held edge", rep.Rows)
	}
	if want := (YTLinkPlanCounts{Held: 1}); rep.Totals != want {
		t.Fatalf("totals = %+v, want %+v", rep.Totals, want)
	}
}

// A re-parent is a delete of the owned old parent plus an add of the new one.
func TestYTLinksPlanReparentIsDeleteAndAdd(t *testing.T) {
	s := ytlpEnv(t, "ytl_plan_reparent")
	ctx := t.Context()
	child, oldParent, newParent := ytlpTask(t, s, "child"), ytlpTask(t, s, "old"), ytlpTask(t, s, "new")
	for _, task := range []Task{child, oldParent, newParent} {
		ytbBind(t, s, ytbBinding(task.ID))
	}
	if _, _, err := s.SetTaskParent(ctx, child.ID, &oldParent.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	fake := ytwritetest.NewFake()
	fake.SeedIssueLink(ytlpIssue(child), ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: ytlpIssue(oldParent)})
	ytlpOwned(t, s, "subtask", "parent", ytlpIssue(child), ytlpIssue(oldParent), child.ID, oldParent.ID)
	if _, _, err := s.SetTaskParent(ctx, child.ID, &newParent.ID, "tester"); err != nil {
		t.Fatal(err)
	}

	rep, err := s.PlanYTLinks(ctx, YTLinksPlanOptions{}, fake)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Rows) != 1 {
		t.Fatalf("rows = %+v", rep.Rows)
	}
	row := rep.Rows[0]
	if row.TaskID != child.ID || row.WouldDeleteOwned != 1 || row.WouldAdd != 1 {
		t.Fatalf("row = %+v", row)
	}
	if want := []YTLinkPlanLink{ytlpLink("subtask", oldParent)}; !reflect.DeepEqual(row.Delete, want) {
		t.Fatalf("delete = %+v, want %+v", row.Delete, want)
	}
	if want := []YTLinkPlanLink{ytlpLink("subtask", newParent)}; !reflect.DeepEqual(row.Add, want) {
		t.Fatalf("add = %+v, want %+v", row.Add, want)
	}
	if want := (YTLinkPlanCounts{WouldAdd: 1, WouldDeleteOwned: 1}); rep.Totals != want {
		t.Fatalf("totals = %+v, want %+v", rep.Totals, want)
	}
}

// --task plans that one bound task; an unknown task is ErrTaskNotFound.
func TestYTLinksPlanTaskSelection(t *testing.T) {
	s := ytlpEnv(t, "ytl_plan_task")
	ctx := t.Context()
	lo, hi := ytlpOrder(ytlpTask(t, s, "lower"), ytlpTask(t, s, "higher"))
	ytbBind(t, s, ytbBinding(lo.ID))
	ytbBind(t, s, ytbBinding(hi.ID))
	if _, err := s.AddTaskRelation(ctx, lo.ID, RelationRelated, hi.ID, "tester"); err != nil {
		t.Fatal(err)
	}

	rep, err := s.PlanYTLinks(ctx, YTLinksPlanOptions{TaskID: lo.ID}, ytwritetest.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tasks != 1 || len(rep.Rows) != 1 || rep.Rows[0].TaskID != lo.ID {
		t.Fatalf("--task %d report = %+v", lo.ID, rep)
	}
	// The other side wants nothing: no row, no add.
	rep, err = s.PlanYTLinks(ctx, YTLinksPlanOptions{TaskID: hi.ID}, ytwritetest.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tasks != 1 || len(rep.Rows) != 0 || rep.Totals != (YTLinkPlanCounts{}) {
		t.Fatalf("--task %d report = %+v", hi.ID, rep)
	}

	_, err = s.PlanYTLinks(ctx, YTLinksPlanOptions{TaskID: 987654321}, ytwritetest.NewFake())
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown task: err = %v, want ErrTaskNotFound", err)
	}
}

// --limit bounds the walk like one sweep slice; a task without a binding is
// never planned (the walk is the sweep's).
func TestYTLinksPlanLimitAndUnbound(t *testing.T) {
	s := ytlpEnv(t, "ytl_plan_limit")
	one, two, unbound := ytlpTask(t, s, "one"), ytlpTask(t, s, "two"), ytlpTask(t, s, "unbound")
	ytbBind(t, s, ytbBinding(one.ID))
	ytbBind(t, s, ytbBinding(two.ID))

	rep, err := s.PlanYTLinks(t.Context(), YTLinksPlanOptions{Limit: 1}, ytwritetest.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tasks != 1 {
		t.Fatalf("limit 1 planned %d tasks", rep.Tasks)
	}
	rep, err = s.PlanYTLinks(t.Context(), YTLinksPlanOptions{TaskID: unbound.ID}, ytwritetest.NewFake())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Tasks != 0 || len(rep.Rows) != 0 {
		t.Fatalf("unbound task planned: %+v", rep)
	}
}
