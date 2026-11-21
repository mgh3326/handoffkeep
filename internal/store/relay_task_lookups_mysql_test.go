//go:build mysql

package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// relayPlanKey returns the index the planner picks for the outer read of table
// in one statement (EXPLAIN FORMAT=TRADITIONAL, the same format as
// explainOpenRead). Subqueries are skipped; an outer read with no key fails.
func relayPlanKey(t *testing.T, ctx context.Context, s *Store, table, query string, args ...any) string {
	t.Helper()
	rows, err := s.db().Query(ctx, "EXPLAIN FORMAT=TRADITIONAL "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v", query, err)
	}
	defer rows.Close()
	key, outer := "", 0
	for rows.Next() {
		var id, selectType, tbl, partitions, typ, possible, k, keyLen, ref, est, filtered, extra sql.NullString
		if err := rows.Scan(&id, &selectType, &tbl, &partitions, &typ, &possible, &k, &keyLen, &ref, &est, &filtered, &extra); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		if tbl.String != table {
			continue
		}
		outer++
		key = k.String
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if outer != 1 {
		t.Fatalf("plan has %d outer reads of %s, want 1: %s", outer, table, query)
	}
	if key == "" {
		t.Fatalf("outer read of %s uses no index: %s", table, query)
	}
	return key
}

// TestMySQLRelayEventReadsUseLookupIndexes seeds lane events for one lane, most
// of them delivered, so the planner has real statistics (hk 5928). It checks
// the three relay_events reads that the digests showed scanning the lane or
// the kind: the undelivered polls by kind and by lane, and the lane-event
// idempotency read that appendRelayEventOnce runs under FOR UPDATE. Each must
// use its index, and the undelivered poll must return only the undelivered rows
// of its lane, in id order.
func TestMySQLRelayEventReadsUseLookupIndexes(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	lane := fmt.Sprintf("plan-lane-%d", time.Now().UnixNano())
	const n = 2000
	const batch = 500
	when := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	const row = `(?,?,?,?,?,?)`
	for start := 0; start < n; start += batch {
		end := min(start+batch, n)
		var args []any
		for i := start; i < end; i++ {
			var delivered any = when
			if i%400 == 0 {
				delivered = nil // five undelivered rows, at i = 0, 400, ..., 1600
			}
			args = append(args, "lane.event", lane, fmt.Sprintf("evt-%d", i), "text", when, delivered)
		}
		q := `INSERT INTO relay_events(kind,owner_lane,event_id,text,received_at,delivered_at) VALUES ` +
			strings.TrimSuffix(strings.Repeat(row+",", end-start), ",")
		if _, err := s.db().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}

	// The undelivered poll of one lane, as ListRelayEventsPage runs it.
	got, err := s.ListRelayEventsPage(ctx, lane, "", true, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("undelivered rows for %s = %d, want 5", lane, len(got))
	}
	for i, x := range got {
		if x.DeliveredAt != nil || x.OwnerLane != lane {
			t.Fatalf("row %d: delivered=%v lane=%q, want undelivered %s", x.ID, x.DeliveredAt, x.OwnerLane, lane)
		}
		if i > 0 && got[i-1].ID >= x.ID {
			t.Fatalf("undelivered rows not in id order: %d then %d", got[i-1].ID, x.ID)
		}
	}

	// The three reads the digests showed scanning relay_events. The poll by
	// kind is the global one, so only its plan is checked here.
	if key := relayPlanKey(t, ctx, s, "relay_events",
		`SELECT id FROM relay_events WHERE 1=1 AND kind=? AND delivered_at IS NULL ORDER BY id ASC LIMIT ?`,
		"lane.event", 100); key != "relay_events_undelivered_kind" {
		t.Errorf("undelivered by kind uses %q, want relay_events_undelivered_kind", key)
	}
	if key := relayPlanKey(t, ctx, s, "relay_events",
		`SELECT id FROM relay_events WHERE 1=1 AND owner_lane=? AND delivered_at IS NULL ORDER BY id ASC LIMIT ?`,
		lane, 100); key != "relay_events_undelivered_owner" {
		t.Errorf("undelivered by lane uses %q, want relay_events_undelivered_owner", key)
	}
	if key := relayPlanKey(t, ctx, s, "relay_events",
		`SELECT id FROM relay_events WHERE owner_lane=? AND event_id=? AND kind='lane.event' FOR UPDATE`,
		lane, "evt-1234"); key != "relay_events_lane_lookup" {
		t.Errorf("lane-event idempotency read uses %q, want relay_events_lane_lookup", key)
	}
}

// TestMySQLTaskBoardReadUsesProjectStateIndex seeds tasks in one project and
// state, so the planner has real statistics (hk 5928). The console board pages
// tasks by id within a project and state (internal/ui/board.go). The plan
// statement below repeats the shape ListTasksFiltered builds for that page: if
// that builder changes, this statement must change with it. The page itself is
// read through ListTasksFiltered, so its rows are checked as well.
func TestMySQLTaskBoardReadUsesProjectStateIndex(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	project := fmt.Sprintf("plan-proj-%d", time.Now().UnixNano())
	when := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	// One task in twenty is in the board's project and state; the rest are in
	// another project or state, as on production (a project's backlog is a
	// small share of all tasks). With every row matching, a walk of PRIMARY in
	// id order is the cheaper plan and the planner rightly takes it.
	other := project + "-other"
	const n = 4000
	const batch = 500
	const row = `(?,?,?,?,?,?,?,?,?)`
	for start := 0; start < n; start += batch {
		end := min(start+batch, n)
		var args []any
		for i := start; i < end; i++ {
			p, st := other, "developing"
			switch {
			case i%20 == 0:
				p, st = project, "backlog"
			case i%2 == 0:
				p, st = project, "completed"
			case i%3 == 0:
				p, st = other, "backlog"
			}
			args = append(args, "lane-plan", fmt.Sprintf("plan task %d", i), "implement", st, "test", when, when, when, p)
		}
		q := `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at,last_activity_at,project) VALUES ` +
			strings.TrimSuffix(strings.Repeat(row+",", end-start), ",")
		if _, err := s.db().Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}

	// Fresh statistics: the planner reads the index cardinalities, not the
	// rows just inserted.
	// ANALYZE TABLE answers a result set, so it is read and closed.
	ar, err := s.db().Query(ctx, `ANALYZE TABLE tasks`)
	if err != nil {
		t.Fatal(err)
	}
	ar.Close()
	if key := relayPlanKey(t, ctx, s, "tasks",
		`SELECT id FROM tasks WHERE 1=1 AND id>? AND state IN (?) AND project=? ORDER BY id ASC LIMIT ?`,
		int64(0), "backlog", project, 50); key != "tasks_project_state_id" {
		t.Errorf("board page uses %q, want tasks_project_state_id", key)
	}

	// Two pages through the store's own id-ordered read: 50 rows, then the
	// next 50 after the last id seen. Each row is in the project and state.
	start := int64(0)
	first, err := s.ListTasksFiltered(ctx, TaskQuery{State: "backlog", Project: &project, AfterID: &start, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 50 {
		t.Fatalf("first page = %d rows, want 50", len(first))
	}
	after := first[len(first)-1].ID
	second, err := s.ListTasksFiltered(ctx, TaskQuery{State: "backlog", Project: &project, AfterID: &after, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 50 || second[0].ID <= after {
		t.Fatalf("second page = %d rows starting at %d, want 50 after %d", len(second), second[0].ID, after)
	}
	for _, x := range append(first, second...) {
		if x.State != "backlog" || x.Project == nil || *x.Project != project {
			t.Fatalf("task %d: state=%q project=%v, want backlog in %s", x.ID, x.State, x.Project, project)
		}
	}
}
