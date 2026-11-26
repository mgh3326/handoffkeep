package store

import (
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
			if !strings.Contains(plan, "Limit") {
				t.Fatalf("plan has no Limit node:\n%s", plan)
			}
			if !strings.Contains(plan, "task_events_kind_to_at_id") {
				t.Fatalf("the event scan does not read task_events_kind_to_at_id; the statement cannot stop at the limit:\n%s", plan)
			}
			scanned := maxScanRows(t, plan, "on task_events e")
			backlog := tasks * tc.merges
			if scanned > tc.maxScan {
				t.Errorf("event scan read %d rows, want at most %d of the %d merge events: the read must stop at the limit", scanned, tc.maxScan, backlog)
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
