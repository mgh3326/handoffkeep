//go:build mysql

package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestTaskBySourceUsesSourceIndex checks the planner's choice for the
// (source, source_ref) lookup that intake and dots run on every request. The
// table is seeded so the planner has real statistics (a full scan of 6,000
// rows is what production showed); the lookup must read tasks_source_ref, the
// functional unique index over the CASE expressions, not the table.
func TestTaskBySourceUsesSourceIndex(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	const n = 2000
	const batch = 500
	const row = `(?,?,?,?,?,?,?,?,?,?)`
	when := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	seedSource := fmt.Sprintf("plan-source-%d", time.Now().UnixNano())
	lane := fmt.Sprintf("plan-lane-%d", time.Now().UnixNano())
	for start := 0; start < n; start += batch {
		end := min(start+batch, n)
		var args []any
		for i := start; i < end; i++ {
			args = append(args, lane, fmt.Sprintf("plan task %d", i), "implement", "backlog", "plan-test", when, when, when, seedSource, fmt.Sprintf("plan-ref-%d", i))
		}
		q := `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at,source,source_ref) VALUES ` +
			strings.TrimSuffix(strings.Repeat(row+",", end-start), ",")
		if _, err := s.db().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Fresh statistics, as in the relay plan test. ANALYZE answers a result
	// set, so it is read and closed.
	ar, err := s.db().Query(ctx, `ANALYZE TABLE tasks`)
	if err != nil {
		t.Fatal(err)
	}
	ar.Close()

	const want = "tasks_source_ref"
	if key := relayPlanKey(t, ctx, s, "tasks", taskBySourceQuery(s.db(), seedSource, "plan-ref-7"), seedSource, "plan-ref-7"); key != want {
		t.Errorf("source lookup uses %q, want %s", key, want)
	}

	checkTaskBySourceLookup(t, s)
}
