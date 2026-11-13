package store

import (
	"fmt"
	"testing"
	"time"
)

// Every applied gate decision is one change_gate_events row. A decision that
// repeats the held verdict is recorded with changed=false, and a stale one is
// not recorded at all (hk 4000).
func TestChangeGateDecisionsAreAppendOnly(t *testing.T) {
	s := schemaStore(t, "change_gate_events")
	ctx := t.Context()
	ref := ChangeRef{Repo: "example/app", PR: 21}
	now := time.Now().UTC()
	gate := func(state string, at time.Time) bool {
		t.Helper()
		res, err := s.ApplyChangeGate(ctx, ref, ChangeGateWrite{HeadSHA: testSHA1, State: state, DecidedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		return res.Sections[SectionGate].Applied
	}
	if !gate("blocked", now.Add(-3*time.Minute)) || !gate("ready", now.Add(-2*time.Minute)) {
		t.Fatal("a fresh decision was not applied")
	}
	if gate("ready", now.Add(-4*time.Minute)) {
		t.Fatal("a decision older than the snapshot applied")
	}
	if !gate("ready", now.Add(-2*time.Minute)) {
		t.Fatal("the same decision again was refused")
	}
	if !gate("ready", now.Add(-time.Minute)) {
		t.Fatal("a repeat of the verdict, decided later, was not applied")
	}
	rows, err := s.pool.Query(ctx, `SELECT e.state,e.changed FROM change_gate_events e JOIN changes c ON c.id=e.change_id
		WHERE c.repo=$1 AND c.pr_number=$2 ORDER BY e.id`, ref.Repo, ref.PR)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var state string
		var changed bool
		if err := rows.Scan(&state, &changed); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s/%t", state, changed))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"blocked/true", "ready/true", "ready/false"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("gate events %v, want %v", got, want)
	}
}

// A gate write that bypasses applyGateTx, as the previous release's writer
// does, still leaves a history row: the trigger records it (hk 4000).
func TestChangeGateEventsRecordEveryWriter(t *testing.T) {
	s := schemaStore(t, "change_gate_events_writer")
	ctx := t.Context()
	ref := ChangeRef{Repo: "example/app", PR: 23}
	now := time.Now().UTC()
	if _, err := s.ApplyChangeGate(ctx, ref, ChangeGateWrite{HeadSHA: testSHA1, State: "ready", DecidedAt: now.Add(-2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE changes SET gate_state='blocked', gate_reasons='[]', gate_decided_at=$3
		WHERE repo=$1 AND pr_number=$2`, ref.Repo, ref.PR, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT e.state,e.changed FROM change_gate_events e JOIN changes c ON c.id=e.change_id
		WHERE c.repo=$1 AND c.pr_number=$2 ORDER BY e.id`, ref.Repo, ref.PR)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var state string
		var changed bool
		if err := rows.Scan(&state, &changed); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s/%t", state, changed))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ready/true", "blocked/true"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("gate events %v, want %v", got, want)
	}
}

// The migration keeps the gate snapshot as history and rewrites the keys the
// old poster posted, so an upgrade does not post a verdict a second time (hk 4000).
func TestChangeGateEventsMigrationKeepsPostedVerdicts(t *testing.T) {
	s := schemaStore(t, "change_gate_events_migration")
	ctx := t.Context()
	ref := ChangeRef{Repo: "example/app", PR: 22}
	now := time.Now().UTC()
	if _, err := s.ApplyChangeGate(ctx, ref, ChangeGateWrite{HeadSHA: testSHA1, State: "ready", DecidedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var changeID int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM changes WHERE repo=$1 AND pr_number=$2`, ref.Repo, ref.PR).Scan(&changeID); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, Task{Lane: relaneLane(t, "gate-migration"), Title: "gate verdict posted before the upgrade", Kind: "implement", Priority: 5, CreatedBy: "tester"})
	if err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Stand in for a database that predates change_gate_events: the table
	// goes, and the delivery holds the key the old poster wrote for the verdict.
	oldKey := fmt.Sprintf("g:%d:%s:ready", changeID, testSHA1)
	exec(`INSERT INTO change_tasks(change_id,task_id,relation,created_at) VALUES($1,$2,'implements',$3)`, changeID, task.ID, now)
	exec(`DROP TRIGGER change_gate_events_record ON changes`)
	exec(`DROP TABLE change_gate_events`)
	exec(`INSERT INTO task_thread_deliveries(task_id,destination,thread_id,thread_name,start_event_id,since,last_event_id,posted,state,next_attempt_at,created_at,updated_at)
		VALUES($1,'discord','4242','#1 gate',0,$2,0,ARRAY[$3]::text[],'active',$4,$4,$4)`, task.ID, now.Add(-2*time.Hour), oldKey, now)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := applyChangeGateEventsMigration(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var eventID int64
	if err := s.pool.QueryRow(ctx, `SELECT id FROM change_gate_events WHERE change_id=$1`, changeID).Scan(&eventID); err != nil {
		t.Fatalf("the snapshot was not backfilled: %v", err)
	}
	var posted []string
	if err := s.pool.QueryRow(ctx, `SELECT posted FROM task_thread_deliveries WHERE task_id=$1`, task.ID).Scan(&posted); err != nil {
		t.Fatal(err)
	}
	newKey := fmt.Sprintf("g:%d", eventID)
	if fmt.Sprint(posted) != fmt.Sprint([]string{oldKey, newKey}) {
		t.Fatalf("posted %v, want the old key kept and %s added", posted, newKey)
	}
	if _, found, err := s.ClaimTaskThread(ctx, now); err != nil || found {
		t.Fatalf("a verdict already posted is due again: found=%v err=%v", found, err)
	}
}
