package store

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The console's deploys read asks for 1,001 merged events. On PostgreSQL,
// listMergedTaskEventsSQL walks task_events_kind_to_at_id newest first and
// stops at the limit: the NOT EXISTS probe drops a task's older merges, so the
// event scan reads the limit plus the merges it drops, never the merged
// backlog. These tests pin that with EXPLAIN ANALYZE at the console limit, on
// a fixture with one merge per task and one with two merges per task.
func TestListMergedTaskEventsPostgresStopsAtLimit(t *testing.T) {
	const limit = 1001
	const tasks = 1200 // more merged tasks than the limit
	base := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name   string
		merges int
		// maxScan bounds the event scan's rows: each kept merge can be
		// preceded by one dropped older merge of the same task.
		maxScan int
	}{
		{name: "single merge", merges: 1, maxScan: limit},
		{name: "re-merge", merges: 2, maxScan: 2 * limit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := schemaStore(t, "merged_events_plan")
			seedMergedBacklog(t, s, tasks, tc.merges, base)

			plan := explainIndexPlan(t, s, listMergedTaskEventsSQL, []any{limit})
			t.Logf("merged task events plan (%s):\n%s", tc.name, plan)
			for _, problem := range mergedEventsPlanProblems(plan, tc.maxScan) {
				t.Errorf("%s (the backlog is %d merge events)", problem, tasks*tc.merges)
			}

			// The rows the plan stops on: one per task, newest merge first. A
			// fresh schema numbers tasks from 1, so the newest merge is task
			// `tasks`, merged last.
			got, err := s.ListMergedTaskEvents(t.Context(), limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != limit {
				t.Fatalf("merged events = %d rows, want %d", len(got), limit)
			}
			if got[0].TaskID != tasks {
				t.Fatalf("first event = task %d, want task %d (the newest merge)", got[0].TaskID, tasks)
			}
			seen := make(map[int64]bool, len(got))
			for _, x := range got {
				if seen[x.TaskID] {
					t.Fatalf("task %d listed twice", x.TaskID)
				}
				seen[x.TaskID] = true
			}
		})
	}
}

// explainIndexPlan is explainAnalyzedPlan with sequential scans and sorts off
// for the transaction. On a fixture this small the planner prefers a
// sequential scan with a full sort, which is a cost choice, not the
// statement's shape. With both off, the only ordered plan left reads
// task_events_kind_to_at_id newest first, the plan the statement is written
// for, and the test measures that plan.
func explainIndexPlan(t *testing.T, s *Store, query string, args []any) string {
	t.Helper()
	ctx := t.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, set := range []string{"SET LOCAL enable_seqscan = off", "SET LOCAL enable_sort = off"} {
		if _, err := tx.Exec(ctx, set); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+query, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("explain: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return strings.Join(lines, "\n")
}

// seedMergedBacklog inserts tasks merged tasks, each merged merges times. Merge
// m of task id is at base + (id*merges + m) seconds, so a higher id is a newer
// merge and a task's last merge is its latest.
func seedMergedBacklog(t *testing.T, s *Store, tasks, merges int, base time.Time) {
	t.Helper()
	ctx := t.Context()
	if _, err := s.pool.Exec(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at)
		SELECT 'plan', 'merged ' || g, 'implement', 'merged', 'plan-test', $1::timestamptz, $1::timestamptz
		FROM generate_series(1, $2::int) g`, base, tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO task_events(task_id, "from", "to", "by", at)
		SELECT t.id, 'developing', 'merged', 'tester',
		$1::timestamptz + ((t.id * $2::bigint + m::bigint) * interval '1 second')
		FROM tasks t CROSS JOIN generate_series(0, $3::int) m
		WHERE t.lane = 'plan'`, base, merges, merges-1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, "ANALYZE tasks, task_events"); err != nil {
		t.Fatal(err)
	}
}

// mergedEventsPlanProblems lists why plan is not the events-driven plan that
// listMergedTaskEventsSQL is written for; no problems means it is. EXPLAIN
// prints rows per loop, so the event node's work is rows times loops. The node
// must be read through task_events_kind_to_at_id once (loops=1), must read at
// most maxScan rows in total, and no Sort may sit under the Limit. The loops
// check is the one a tasks-driven plan fails: it probes the events once per
// merged task, so its per-loop rows look like one or two.
//
// A full sorted scan can fail all three checks at once; each still names a
// different defect, and the loops check is the only one that holds when the
// bound is loosened for the re-merge fixture.
func mergedEventsPlanProblems(plan string, maxScan int) []string {
	if !strings.Contains(plan, "Limit") {
		return []string{"plan has no Limit node"}
	}
	e, err := findPlanNode(plan, "on task_events e")
	if err != nil {
		return []string{err.Error()}
	}
	var problems []string
	if !strings.Contains(e.line, "task_events_kind_to_at_id") {
		problems = append(problems, "the event scan does not read task_events_kind_to_at_id; the statement cannot stop at the limit")
	}
	if e.loops != 1 {
		problems = append(problems, fmt.Sprintf("the event scan runs %d times, want once: it is probed per outer row, so the statement is not driven from the merge events", e.loops))
	}
	if read := e.rows * e.loops; read > maxScan {
		problems = append(problems, fmt.Sprintf("event scan read %d rows, want at most %d: the read must stop at the limit", read, maxScan))
	}
	if planSortNode.MatchString(plan) {
		problems = append(problems, "the plan sorts under the Limit: the read does not come out of the index in order")
	}
	return problems
}

// planNode is the one plan node a check reads: its header line and the actual
// row count and loop count EXPLAIN ANALYZE printed on it.
type planNode struct {
	line        string
	rows, loops int
}

var (
	planActualRows = regexp.MustCompile(`\(actual time=[^)]*rows=(\d+) loops=(\d+)\)`)
	planSortNode   = regexp.MustCompile(`(?m)^\s*(?:->\s+)?(?:Incremental\s+)?Sort\s+\(`)
)

// findPlanNode returns the single plan line that contains scan, with its
// actual rows and loops. A missing or repeated node is an error, so a check
// never reads a different node than the one it names.
func findPlanNode(plan, scan string) (planNode, error) {
	var found []string
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, scan) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		return planNode{}, fmt.Errorf("want one plan node over %q, found %d", scan, len(found))
	}
	m := planActualRows.FindStringSubmatch(found[0])
	if m == nil {
		return planNode{}, fmt.Errorf("the %q node has no actual rows and loops (EXPLAIN ANALYZE output expected)", scan)
	}
	rows, err := strconv.Atoi(m[1])
	if err != nil {
		return planNode{}, fmt.Errorf("plan rows %q: %v", m[1], err)
	}
	loops, err := strconv.Atoi(m[2])
	if err != nil {
		return planNode{}, fmt.Errorf("plan loops %q: %v", m[2], err)
	}
	return planNode{line: found[0], rows: rows, loops: loops}, nil
}

// The two plans below are EXPLAIN (ANALYZE, BUFFERS) text in PostgreSQL's
// layout, written from the statement's shape and the fixture's row counts,
// not copied from a live run. They exercise the parser and the check; the
// live test above checks today's query on PostgreSQL in CI.

// tasksDrivenMergedPlan is the plan a tasks-driven statement gets: the merged
// tasks drive a nested loop, and each task probes task_events once. Its event
// node reports rows=1 per loop over 1200 loops and sorts under the Limit.
const tasksDrivenMergedPlan = `Limit  (cost=2950.10..2952.60 rows=1001 width=52) (actual time=18.410..18.702 rows=1001 loops=1)
  ->  Sort  (cost=2950.10..2953.10 rows=1200 width=52) (actual time=18.408..18.520 rows=1001 loops=1)
        Sort Key: e.at DESC, e.id DESC
        Sort Method: top-N heapsort  Memory: 160kB
        ->  Nested Loop  (cost=0.85..2700.00 rows=1200 width=52) (actual time=0.060..16.900 rows=1 loops=1)
              ->  Seq Scan on tasks t  (cost=0.00..30.00 rows=1200 width=40) (actual time=0.010..0.300 rows=1200 loops=1)
                    Filter: ((state)::text = 'merged'::text)
              ->  Index Scan using task_events_kind_to_at_id on task_events e  (cost=0.42..2.00 rows=1 width=24) (actual time=0.007..0.012 rows=1 loops=1200)
                    Index Cond: (task_id = t.id)
                    Filter: (((kind)::text = 'transition'::text) AND (("to")::text = 'merged'::text))
Planning Time: 0.410 ms
Execution Time: 18.900 ms
`

// eventsDrivenMergedPlan is the plan the statement is written for: the merge
// events drive, read newest first from the index, and the anti-join probes the
// task's newer merges once per event.
const eventsDrivenMergedPlan = `Limit  (cost=0.85..410.20 rows=1001 width=52) (actual time=0.090..2.410 rows=1001 loops=1)
  ->  Nested Loop Anti Join  (cost=0.85..1000.00 rows=1001 width=52) (actual time=0.089..2.300 rows=1001 loops=1)
        ->  Nested Loop  (cost=0.85..500.00 rows=1001 width=52) (actual time=0.080..1.900 rows=1001 loops=1)
              ->  Index Scan using task_events_kind_to_at_id on task_events e  (cost=0.42..200.00 rows=1001 width=24) (actual time=0.040..0.800 rows=1001 loops=1)
                    Index Cond: (((kind)::text = 'transition'::text) AND (("to")::text = 'merged'::text))
              ->  Index Scan using tasks_pkey on tasks t  (cost=0.42..0.30 rows=1 width=40) (actual time=0.001..0.001 rows=1 loops=1001)
                    Index Cond: (id = e.task_id)
                    Filter: ((state)::text = 'merged'::text)
        ->  Index Scan using task_events_task_at on task_events n  (cost=0.42..0.50 rows=1 width=8) (actual time=0.001..0.001 rows=0 loops=1001)
              Index Cond: (task_id = e.task_id)
Planning Time: 0.390 ms
Execution Time: 2.600 ms
`

func TestMergedEventsPlanCheck(t *testing.T) {
	const limit = 1001
	for _, tc := range []struct {
		name    string
		plan    string
		maxScan int
		// want lists a substring each reported problem must contain; nil means
		// the plan passes.
		want []string
	}{
		{name: "events-driven plan passes", plan: eventsDrivenMergedPlan, maxScan: limit},
		{
			name: "tasks-driven plan fails on loops, sort and rows", plan: tasksDrivenMergedPlan, maxScan: limit,
			want: []string{"runs 1200 times", "sorts under the Limit", "read 1200 rows"},
		},
		{
			// The loops check is what fails it when the bound is the loose
			// re-merge bound: 1200 rows is under 2*limit.
			name: "tasks-driven plan fails on loops under the re-merge bound", plan: tasksDrivenMergedPlan, maxScan: 2 * limit,
			want: []string{"runs 1200 times", "sorts under the Limit"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := mergedEventsPlanProblems(tc.plan, tc.maxScan)
			if len(tc.want) == 0 {
				if len(problems) != 0 {
					t.Fatalf("problems = %q, want none", problems)
				}
				return
			}
			for _, w := range tc.want {
				if !containsProblem(problems, w) {
					t.Errorf("problems = %q, want one containing %q", problems, w)
				}
			}
		})
	}
}

func containsProblem(problems []string, sub string) bool {
	for _, p := range problems {
		if strings.Contains(p, sub) {
			return true
		}
	}
	return false
}
