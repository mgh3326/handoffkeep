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

// This test proves the hk 6397 rewrite of the console attention reads (hk 5584
// digests e1625189 and 5f29c6ea, about 6,700 rows examined per call every
// ~86 s): the awaiting half ranges over the awaiting_decision rows, the
// started half probes the started ids by primary key (counts) or one state's
// rows (Attention), and the same reads return the seeded waiting tasks
// unchanged. The plan statements below repeat the shape the store runs: if
// the store's text changes, these statements must change with it, and the
// rows are read through the store's own methods.
func TestAttentionReadsOnlyCandidates(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	lane := fmt.Sprintf("plan-attn-%d", now.UnixNano())

	// Two thousand merged tasks, as on production (most rows merged): a whole
	// scan would read them all, while the rewritten halves match only the
	// fixture below.
	others := make([]planTaskSeed, 0, 2000)
	for i := range 2000 {
		others = append(others, planTaskSeed{lane: lane, title: fmt.Sprintf("other %d", i), state: "merged", created: now.Add(-time.Hour)})
	}
	seedPlanTasks(t, ctx, s, others)

	a1 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "await 1", state: "developing", awaiting: true, created: now.Add(-5 * time.Hour)}})[0]
	a2 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "await 2", state: "developing", awaiting: true, created: now.Add(-3 * time.Hour)}})[0]
	a3 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "await 3", state: "developing", awaiting: true, created: now.Add(-time.Hour)}})[0]
	// Lookalikes: awaiting set but excluded by the refs conditions.
	seedPlanTasks(t, ctx, s, []planTaskSeed{
		{lane: lane, title: "disposition", state: "backlog", awaiting: true, refs: `{"disposition":{"kind":"x"}}`, created: now.Add(-time.Hour)},
		{lane: lane, title: "open request", state: "developing", awaiting: true, refs: `{"decision_request":{"status":"open"}}`, created: now.Add(-time.Hour)},
	})
	s1 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "stalled 1", state: "developing", created: now.Add(-72 * time.Hour)}})[0]
	s2 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "stalled 2", state: "researching", created: now.Add(-70 * time.Hour)}})[0]
	fresh := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "fresh", state: "developing", created: now.Add(-30 * time.Minute)}})[0]
	// startedIDs carry a fresh task (not stalled) and an awaiting task (kept
	// once, by the awaiting half: the halves are disjoint).
	startedIDs := []int64{s1, s2, fresh, a1}

	// Fresh statistics: the planner reads the index cardinalities, not the
	// rows just inserted. ANALYZE TABLE answers a result set, so it is read
	// and closed.
	ar, err := s.db().Query(ctx, `ANALYZE TABLE tasks`)
	if err != nil {
		t.Fatal(err)
	}
	ar.Close()

	// The rows, through the store's own reads. With no events, comments or
	// jobs, since is created_at, so the order is newest first.
	want := []int64{a3, a2, a1, s2, s1}
	got, err := s.AttentionTaskCandidates(ctx, now, startedIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("candidates = %d rows, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("candidate %d = task %d, want %d (order %v)", i, got[i].ID, id, want)
		}
		if got[i].Key != fmt.Sprintf("task:%d", id) {
			t.Fatalf("candidate %d key = %q", i, got[i].Key)
		}
	}
	for i, x := range got {
		wantReason := AttentionReasonAwaitingDecision
		if i >= 3 {
			wantReason = AttentionReasonStalled
		}
		if x.Reason != wantReason {
			t.Fatalf("candidate %d (task %d) reason = %q, want %q", i, x.ID, x.Reason, wantReason)
		}
	}

	att, err := s.Attention(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if att.Tasks.AwaitingDecision != 3 || att.Tasks.Stalled != 2 || att.Tasks.Count != 5 {
		t.Fatalf("attention tasks = %+v, want awaiting 3 stalled 2 count 5", att.Tasks)
	}
	if len(att.Tasks.Items) != len(want) {
		t.Fatalf("attention items = %d rows, want %d", len(att.Tasks.Items), len(want))
	}
	for i, id := range want {
		if att.Tasks.Items[i].ID != id {
			t.Fatalf("item %d = task %d, want %d", i, att.Tasks.Items[i].ID, id)
		}
	}

	// The plans: every read of tasks uses an index, never a full scan. The
	// awaiting halves range over tasks_awaiting_decision_id, the stalled
	// halves over tasks_state_id, and the candidates' started half probes the
	// primary key.
	tx, release, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer tx.Rollback(ctx)
	stalledBefore := now.Add(-s.StartedStalledHours())
	allowed := map[string]map[string]bool{
		"tasks": {"tasks_awaiting_decision_id": true, "tasks_state_id": true, "PRIMARY": true},
	}
	assertNoFullScan(t, ctx, s, "attention counts", attentionTaskCountsMySQL, allowed, 2, attentionTaskArgs(tx, stalledBefore, now, true, 0)...)
	assertNoFullScan(t, ctx, s, "attention items", attentionTaskItemsMySQL, allowed, 2, attentionTaskArgs(tx, stalledBefore, now, false, AttentionItems)...)
	candidatesQuery, ids := attentionTaskCandidatesMySQLQuery(startedIDs)
	candidatesArgs := append([]any{now}, ids...)
	candidatesArgs = append(candidatesArgs, stalledBefore, now)
	assertNoFullScan(t, ctx, s, "attention candidates", candidatesQuery, allowed, 2, candidatesArgs...)
}

// This test proves the hk 6397 rewrite of the console deploys read (hk 5584
// digest f9a5f28d, about 6,165 rows examined per call every ~86 s): the read
// drives from the merge transitions, newest first, keeps each task's latest
// merge while the task is still merged, and stops after its limit. The plan
// statement below is the store's own text (listMergedTaskEventsMySQL); the
// rows are read through ListMergedTaskEvents.
func TestMergedTaskEventsReadsOnlyRecent(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	lane := fmt.Sprintf("plan-deploys-%d", now.UnixNano())
	h := func(hours int) time.Time { return now.Add(time.Duration(hours) * time.Hour) }

	// Fifteen hundred merged tasks with one old merge each, as on production
	// (most rows merged): a tasks-driven read walks them all, while the
	// rewritten read stops after its limit.
	base := now.Add(-90 * 24 * time.Hour)
	otherIDs := seedPlanTasks(t, ctx, s, func() []planTaskSeed {
		out := make([]planTaskSeed, 0, 1500)
		for i := range 1500 {
			out = append(out, planTaskSeed{lane: lane, title: fmt.Sprintf("old %d", i), state: "merged", created: base})
		}
		return out
	}())
	seedPlanEvents(t, ctx, s, func() []planEventSeed {
		out := make([]planEventSeed, 0, 1500)
		for i, id := range otherIDs {
			out = append(out, planEventSeed{taskID: id, from: "developing", to: "merged", by: "tester", at: base.Add(time.Duration(i) * time.Second)})
		}
		return out
	}())

	t1 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "t1", state: "merged", created: base}})[0]
	t2 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "t2", state: "merged", created: base}})[0]
	t3 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "t3", state: "dropped", created: base}})[0]
	t4 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "t4", state: "developing", created: base}})[0]
	t5 := seedPlanTasks(t, ctx, s, []planTaskSeed{{lane: lane, title: "t5", state: "merged", created: base}})[0]
	seedPlanEvents(t, ctx, s, []planEventSeed{
		{taskID: t1, from: "developing", to: "merged", by: "tester", at: h(-19)},
		{taskID: t2, from: "developing", to: "merged", by: "tester", at: h(-22)},
		{taskID: t2, from: "merged", to: "reopened", by: "tester", at: h(-21)},
		{taskID: t2, from: "reopened", to: "merged", by: "tester", at: h(-18)},
		{taskID: t3, from: "developing", to: "merged", by: "tester", at: h(-23)},
		{taskID: t3, from: "merged", to: "dropped", by: "tester", at: h(-20)},
		{taskID: t5, from: "developing", to: "merged", by: "tester", at: h(-17)},
	})

	ar, err := s.db().Query(ctx, `ANALYZE TABLE tasks, task_events`)
	if err != nil {
		t.Fatal(err)
	}
	ar.Close()

	// The newest merges first: t5, the second merge of t2 (kept once), t1,
	// then the newest old merges. t3 merged and dropped, t4 never merged.
	newestOld := append([]int64{}, otherIDs...)
	for i, j := 0, len(newestOld)-1; i < j; i, j = i+1, j-1 {
		newestOld[i], newestOld[j] = newestOld[j], newestOld[i]
	}
	want := append([]int64{t5, t2, t1}, newestOld[:7]...)
	got, err := s.ListMergedTaskEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("merged events = %d rows, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].TaskID != id {
			t.Fatalf("event %d = task %d, want %d", i, got[i].TaskID, id)
		}
		if got[i].Refs != nil {
			t.Fatalf("event %d (task %d) refs = %+v, want nil", i, id, got[i].Refs)
		}
	}
	if !got[0].At.Equal(h(-17)) || !got[1].At.Equal(h(-18)) || !got[2].At.Equal(h(-19)) {
		t.Fatalf("first merges at %v, %v, %v, want -17h, -18h, -19h", got[0].At, got[1].At, got[2].At)
	}
	for _, x := range got {
		if x.TaskID == t3 || x.TaskID == t4 {
			t.Fatalf("merged events list task %d, which is not merged", x.TaskID)
		}
	}

	capped, err := s.ListMergedTaskEvents(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 2 || capped[0].TaskID != t5 || capped[1].TaskID != t2 {
		t.Fatalf("limit 2 = %v, want [t5 t2]", capped)
	}

	// The plan: the outer read ranges over task_events_kind_to_at_id, tasks
	// is a primary-key join, and the NOT EXISTS probe uses its per-task key.
	// No read scans a table.
	assertNoFullScan(t, ctx, s, "merged task events", listMergedTaskEventsMySQL, map[string]map[string]bool{
		"e": {"task_events_kind_to_at_id": true},
		"t": {"PRIMARY": true},
		"n": nil,
	}, 3, 10)
}

// planTaskSeed is one tasks row for a plan fixture (every NOT NULL column the
// seed writes; the rest keep their defaults).
type planTaskSeed struct {
	lane     string
	title    string
	state    string
	refs     string
	awaiting bool
	created  time.Time
}

// seedPlanTasks inserts tasks in batches through the store's transaction (a
// synced table: its trigger's sync_log rows are numbered at commit) and
// returns their ids in order. A multi-row INSERT takes consecutive
// AUTO_INCREMENT values, so each batch is numbered from LAST_INSERT_ID.
func seedPlanTasks(t *testing.T, ctx context.Context, s *Store, seeds []planTaskSeed) []int64 {
	t.Helper()
	tx, release, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fail := func() {
		tx.Rollback(ctx)
		t.FailNow()
	}
	ids := make([]int64, 0, len(seeds))
	const batch = 500
	const row = `(?,?,?,?,CAST(? AS JSON),?,?,?,?)`
	for start := 0; start < len(seeds); start += batch {
		end := min(start+batch, len(seeds))
		var args []any
		for _, x := range seeds[start:end] {
			refs := x.refs
			if refs == "" {
				refs = "{}"
			}
			awaiting := 0
			if x.awaiting {
				awaiting = 1
			}
			args = append(args, x.lane, x.title, "implement", x.state, refs, "test", x.created, x.created, awaiting)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tasks(lane,title,kind,state,refs,created_by,created_at,updated_at,awaiting_decision) VALUES `+
			strings.TrimSuffix(strings.Repeat(row+",", end-start), ","), args...); err != nil {
			t.Logf("insert tasks: %v", err)
			fail()
		}
		var first int64
		if err := tx.QueryRow(ctx, `SELECT LAST_INSERT_ID()`).Scan(&first); err != nil {
			t.Logf("last insert id: %v", err)
			fail()
		}
		for i := range end - start {
			ids = append(ids, first+int64(i))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return ids
}

// planEventSeed is one transition row for a plan fixture.
type planEventSeed struct {
	taskID       int64
	from, to, by string
	at           time.Time
}

// seedPlanEvents inserts transition events in batches. task_events is
// append-only (its guard triggers reject UPDATE and DELETE), but INSERT is a
// plain write.
func seedPlanEvents(t *testing.T, ctx context.Context, s *Store, seeds []planEventSeed) {
	t.Helper()
	const batch = 500
	const row = `(?,?,?,?,?)`
	for start := 0; start < len(seeds); start += batch {
		end := min(start+batch, len(seeds))
		var args []any
		for _, x := range seeds[start:end] {
			args = append(args, x.taskID, x.from, x.to, x.by, x.at)
		}
		if _, err := s.db().Exec(ctx, "INSERT INTO task_events(task_id, `from`, `to`, `by`, at) VALUES "+
			strings.TrimSuffix(strings.Repeat(row+",", end-start), ","), args...); err != nil {
			t.Fatal(err)
		}
	}
}

// assertNoFullScan EXPLAINs the query with its own arguments and requires
// that every read of a monitored table uses an allowed index: no read scans
// its table (type ALL) or uses no key. Tables not in allowed (the inbox
// joins, the UNION result) are not checked. minReads is how many monitored
// reads the plan must hold (the two halves of a UNION, the outer read with
// its join and probe).
func assertNoFullScan(t *testing.T, ctx context.Context, s *Store, name, query string, allowed map[string]map[string]bool, minReads int, args ...any) {
	t.Helper()
	rows, err := s.db().Query(ctx, "EXPLAIN FORMAT=TRADITIONAL "+query, args...)
	if err != nil {
		t.Fatalf("%s: EXPLAIN: %v", name, err)
	}
	defer rows.Close()
	checked := 0
	for rows.Next() {
		var id, selectType, table, partitions, typ, possible, key, keyLen, ref, est, filtered, extra sql.NullString
		if err := rows.Scan(&id, &selectType, &table, &partitions, &typ, &possible, &key, &keyLen, &ref, &est, &filtered, &extra); err != nil {
			t.Fatalf("%s: scan plan: %v", name, err)
		}
		want, ok := allowed[table.String]
		if !ok {
			continue
		}
		checked++
		if typ.String == "ALL" {
			t.Errorf("%s: read of %s scans the whole table (extra %q); want an index", name, table.String, extra.String)
		}
		if !key.Valid || key.String == "" {
			t.Errorf("%s: read of %s uses no index (type %s, extra %q)", name, table.String, typ.String, extra.String)
		} else if want != nil && !want[key.String] {
			t.Errorf("%s: read of %s uses %q, want one of %v", name, table.String, key.String, keysOf(want))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: plan rows: %v", name, err)
	}
	if checked < minReads {
		t.Errorf("%s: plan holds %d monitored reads, want at least %d", name, checked, minReads)
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
