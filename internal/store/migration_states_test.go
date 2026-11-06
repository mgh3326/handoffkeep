package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// stateSeed is one row of the migration map ("Migrating existing states" in
// docs/linear-internalize.md): a legacy task, what surrounds it, and what the
// migration must make of it.
type stateSeed struct {
	name      string
	legacy    string
	kind      string
	claimedBy string
	refs      string // JSON; "" is {}
	job       string // "", "live:<role>" (a claimed or spawned job) or "ended"
	noJob     bool   // the newest claim event carries a no-job reason
	left      string // needs_decision: the state it left

	wantState    string
	wantRule     string // "" = unchanged: no snapshot row, no event
	wantAwaiting bool
	wantClaimed  string
	id           int64
}

var stateSeeds = []stateSeed{
	{name: "backlog", legacy: "backlog", kind: "implement", wantState: "backlog"},
	{name: "merged", legacy: "merged", kind: "implement", claimedBy: "w0", wantState: "merged", wantClaimed: "w0"},
	{name: "dropped", legacy: "dropped", kind: "implement", wantState: "dropped"},
	{name: "reopened", legacy: "reopened", kind: "implement", wantState: "unstarted", wantRule: "reopened_unstarted"},
	{name: "claimed live worker", legacy: "claimed", kind: "implement", claimedBy: "w1", job: "live:worker", wantState: "developing", wantRule: "claimed_started", wantClaimed: "w1"},
	{name: "claimed live consult", legacy: "claimed", kind: "implement", claimedBy: "w1", job: "live:consult", wantState: "researching", wantRule: "claimed_started", wantClaimed: "w1"},
	{name: "claimed no-job reason", legacy: "claimed", kind: "verify", claimedBy: "w2", noJob: true, wantState: "researching", wantRule: "claimed_started", wantClaimed: "w2"},
	{name: "claimed job ended", legacy: "claimed", kind: "implement", claimedBy: "w3", refs: `{"job_id":"old-job"}`, job: "ended", wantState: "unstarted", wantRule: "claimed_ended", wantClaimed: ""},
	{name: "claimed job missing", legacy: "claimed", kind: "fix", claimedBy: "w4", refs: `{"job_id":"gone-job"}`, wantState: "unstarted", wantRule: "claimed_ended", wantClaimed: ""},
	{name: "in_progress fix", legacy: "in_progress", kind: "fix", claimedBy: "w5", wantState: "developing", wantRule: "in_progress_started", wantClaimed: "w5"},
	{name: "in_progress decide", legacy: "in_progress", kind: "decide", claimedBy: "w5", wantState: "researching", wantRule: "in_progress_started", wantClaimed: "w5"},
	{name: "in_progress research job", legacy: "in_progress", kind: "implement", claimedBy: "w5", job: "live:research", wantState: "researching", wantRule: "in_progress_started", wantClaimed: "w5"},
	{name: "verifying", legacy: "verifying", kind: "implement", claimedBy: "w6", wantState: "reviewing", wantRule: "verifying_reviewing", wantClaimed: "w6"},
	{name: "join", legacy: "join", kind: "implement", claimedBy: "w6", wantState: "reviewing", wantRule: "join_reviewing", wantClaimed: "w6"},
	{name: "hold", legacy: "hold", kind: "implement", wantState: "icebox", wantRule: "hold_icebox"},
	{name: "hold mirrored", legacy: "hold", kind: "implement", refs: `{"linear":{"issue_id":"iss-1","sync":false}}`, wantState: "icebox", wantRule: "hold_icebox"},
	{name: "decision disposition", legacy: "needs_decision", kind: "decide", claimedBy: "op", refs: `{"disposition":{}}`, wantState: "backlog", wantRule: "decision_disposition", wantAwaiting: true, wantClaimed: "op"},
	{name: "decision live job left verifying", legacy: "needs_decision", kind: "implement", claimedBy: "w7", job: "live:worker", left: "verifying", wantState: "reviewing", wantRule: "decision_live_job", wantAwaiting: true, wantClaimed: "w7"},
	{name: "decision live job left claimed", legacy: "needs_decision", kind: "implement", claimedBy: "w7", job: "live:worker", left: "claimed", wantState: "developing", wantRule: "decision_live_job", wantAwaiting: true, wantClaimed: "w7"},
	{name: "decision live job left hold", legacy: "needs_decision", kind: "implement", claimedBy: "w7", job: "live:worker", left: "hold", wantState: "icebox", wantRule: "decision_live_job", wantAwaiting: true, wantClaimed: "w7"},
	{name: "decision no job", legacy: "needs_decision", kind: "implement", claimedBy: "w8", left: "claimed", wantState: "unstarted", wantRule: "decision_no_job", wantAwaiting: true, wantClaimed: "w8"},
}

// legacyStates is the old vocabulary, as tasks_state_check allowed it before
// the migration.
const legacyStateList = `'backlog','reopened','claimed','in_progress','verifying','join','hold','needs_decision','merged','dropped'`

// rewindStateMigration returns a fully migrated schema to the shape it had at
// version 31, so the seed can be written in the old vocabulary and migrated by
// the real code path.
func rewindStateMigration(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, q := range []string{
		`DELETE FROM schema_version WHERE version=32`,
		`DELETE FROM schema_migrations WHERE id='20261106-task-closure'`,
		`ALTER TABLE tasks DROP CONSTRAINT tasks_closure_check`,
		`DROP TABLE task_state_migration`,
		`DROP TABLE task_relations`,
		`DROP INDEX task_events_source_ref`,
		`ALTER TABLE task_events DROP COLUMN source, DROP COLUMN source_ref, DROP COLUMN field, DROP COLUMN from_value, DROP COLUMN to_value, DROP COLUMN auto, DROP COLUMN applied`,
		`ALTER TABLE tasks DROP COLUMN awaiting_decision, DROP COLUMN parent_id, DROP COLUMN started_at, DROP COLUMN completed_at, DROP COLUMN canceled_at, DROP COLUMN triaged_at, DROP COLUMN started_triage_at`,
		`ALTER TABLE task_events DROP CONSTRAINT task_events_kind_check, ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane','decision','project','area')) NOT VALID`,
		`ALTER TABLE tasks DROP CONSTRAINT tasks_state_check, ADD CONSTRAINT tasks_state_check CHECK(state IN (` + legacyStateList + `))`,
	} {
		if _, err := pool.Exec(context.Background(), q); err != nil {
			t.Fatalf("rewind %q: %v", q, err)
		}
	}
}

func seedLegacyTasks(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for i := range stateSeeds {
		sd := &stateSeeds[i]
		refs := sd.refs
		if refs == "" {
			refs = "{}"
		}
		if err := pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,claimed_by,refs,created_by,created_at,updated_at,last_activity_at) VALUES('mig-lane',$1,$2,$3,$4,$5::jsonb,'seed',now(),now(),now()) RETURNING id`,
			sd.name, sd.kind, sd.legacy, sd.claimedBy, refs).Scan(&sd.id); err != nil {
			t.Fatalf("seed %q: %v", sd.name, err)
		}
		switch {
		case strings.HasPrefix(sd.job, "live:"):
			if _, err := pool.Exec(ctx, `INSERT INTO jobs(job_id,task_id,host,state,role,created_by,created_at,updated_at) VALUES($1,$2,'host-a','spawned',$3,'seed',now(),now())`, fmt.Sprintf("job-%d", sd.id), sd.id, strings.TrimPrefix(sd.job, "live:")); err != nil {
				t.Fatal(err)
			}
		case sd.job == "ended":
			if _, err := pool.Exec(ctx, `INSERT INTO jobs(job_id,task_id,host,state,role,created_by,created_at,updated_at) VALUES($1,$2,'host-a','completed','worker','seed',now(),now())`, fmt.Sprintf("job-%d", sd.id), sd.id); err != nil {
				t.Fatal(err)
			}
		}
		if sd.noJob {
			if _, err := pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,at,no_job) VALUES($1,'backlog','claimed','seed','',now(),'operator started it by hand')`, sd.id); err != nil {
				t.Fatal(err)
			}
		}
		if sd.legacy == "needs_decision" && sd.left != "" {
			if _, err := pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",note,at) VALUES($1,$2,'needs_decision','seed','which?',now())`, sd.id, sd.left); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func expectedPlan() []StateMigrationCount {
	counts := map[[3]string]int{}
	for _, sd := range stateSeeds {
		if sd.wantRule != "" {
			counts[[3]string{sd.legacy, sd.wantRule, sd.wantState}]++
		}
	}
	out := []StateMigrationCount{}
	for k, n := range counts {
		out = append(out, StateMigrationCount{LegacyState: k[0], Rule: k[1], NewState: k[2], Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.LegacyState != b.LegacyState {
			return a.LegacyState < b.LegacyState
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.NewState < b.NewState
	})
	return out
}

func rowCount(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// legacySeededStore returns a throwaway schema holding the seed in the old
// vocabulary at schema version 31.
func legacySeededStore(t *testing.T, prefix string) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool, _ := testdb.SchemaPool(t, prefix)
	s := &Store{pool: pool}
	if err := s.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	rewindStateMigration(t, pool)
	for i := range stateSeeds {
		stateSeeds[i].id = 0
	}
	seedLegacyTasks(t, pool)
	return s, pool
}

// TestStateMigrationDryRunWritesNothing proves the dry-run counts per
// (legacy_state, rule, new_state) and leaves the database exactly as it was.
func TestStateMigrationDryRunWritesNothing(t *testing.T) {
	s, pool := legacySeededStore(t, "state_plan")
	ctx := t.Context()
	counts := func() [6]int {
		return [6]int{
			rowCount(t, pool, `SELECT count(*) FROM tasks`),
			rowCount(t, pool, `SELECT count(*) FROM task_events`),
			rowCount(t, pool, `SELECT count(*) FROM sync_log`),
			rowCount(t, pool, `SELECT count(*) FROM tasks WHERE state IN (`+legacyStateList+`)`),
			rowCount(t, pool, `SELECT count(*) FROM schema_version WHERE version=32`),
			rowCount(t, pool, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ('task_state_migration','task_relations')`),
		}
	}
	before := counts()
	plan, err := s.PlanStateMigration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after := counts(); after != before {
		t.Fatalf("the dry-run wrote: before=%v after=%v", before, after)
	}
	if before[4] != 0 || before[5] != 0 {
		t.Fatalf("the seed is not an unmigrated database: %v", before)
	}
	if plan.Migrated {
		t.Fatal("an unmigrated database reported as migrated")
	}
	want := expectedPlan()
	if fmt.Sprint(plan.Counts) != fmt.Sprint(want) {
		t.Fatalf("plan counts:\n got %v\nwant %v", plan.Counts, want)
	}
	// One mirrored hold task would show as Canceled in Linear.
	if plan.HoldMirroredToCanceled != 1 {
		t.Fatalf("mirrored hold tasks=%d, want 1", plan.HoldMirroredToCanceled)
	}
	// After the migration the dry-run has nothing left to plan.
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if again, err := s.PlanStateMigration(ctx); err != nil || !again.Migrated || len(again.Counts) != 0 {
		t.Fatalf("plan after migration: %+v err=%v", again, err)
	}
}

// TestStateMigrationMap seeds one row per row of the migration map and checks
// the new state, rule, snapshot row and event of each, and that no task keeps
// an old name.
func TestStateMigrationMap(t *testing.T) {
	s, pool := legacySeededStore(t, "state_map")
	ctx := t.Context()
	syncBefore := rowCount(t, pool, `SELECT count(*) FROM sync_log WHERE model='task' AND action='U'`)
	eventsBefore := map[int64]int{}
	for _, sd := range stateSeeds {
		eventsBefore[sd.id] = rowCount(t, pool, `SELECT count(*) FROM task_events WHERE task_id=$1`, sd.id)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	changed := 0
	for _, sd := range stateSeeds {
		var state, claimedBy string
		var awaiting bool
		var jobID *string
		if err := pool.QueryRow(ctx, `SELECT state, claimed_by, awaiting_decision, refs->>'job_id' FROM tasks WHERE id=$1`, sd.id).Scan(&state, &claimedBy, &awaiting, &jobID); err != nil {
			t.Fatalf("%s: %v", sd.name, err)
		}
		if state != sd.wantState || claimedBy != sd.wantClaimed || awaiting != sd.wantAwaiting {
			t.Errorf("%s: state=%q claimed_by=%q awaiting=%t, want %q %q %t", sd.name, state, claimedBy, awaiting, sd.wantState, sd.wantClaimed, sd.wantAwaiting)
		}
		snapshots := rowCount(t, pool, `SELECT count(*) FROM task_state_migration WHERE task_id=$1`, sd.id)
		events := rowCount(t, pool, `SELECT count(*) FROM task_events WHERE task_id=$1 AND kind='state_migration'`, sd.id)
		if sd.wantRule == "" {
			if snapshots != 0 || events != 0 {
				t.Errorf("%s is unchanged but has snapshot=%d events=%d", sd.name, snapshots, events)
			}
			continue
		}
		changed++
		var legacy, legacyClaimed, newState, rule string
		var newAwaiting bool
		if err := pool.QueryRow(ctx, `SELECT legacy_state, legacy_claimed_by, new_state, rule, new_awaiting FROM task_state_migration WHERE task_id=$1`, sd.id).Scan(&legacy, &legacyClaimed, &newState, &rule, &newAwaiting); err != nil {
			t.Errorf("%s: no task_state_migration row: %v", sd.name, err)
			continue
		}
		if legacy != sd.legacy || legacyClaimed != sd.claimedBy || newState != sd.wantState || rule != sd.wantRule || newAwaiting != sd.wantAwaiting {
			t.Errorf("%s: snapshot=%q/%q/%q/%q/%t", sd.name, legacy, legacyClaimed, newState, rule, newAwaiting)
		}
		var from, to, by, note, source, field, fromValue, toValue string
		var auto, applied bool
		if events != 1 {
			t.Errorf("%s: %d state_migration events, want 1", sd.name, events)
			continue
		}
		if err := pool.QueryRow(ctx, `SELECT "from","to","by",note,source,field,from_value,to_value,auto,applied FROM task_events WHERE task_id=$1 AND kind='state_migration'`, sd.id).Scan(&from, &to, &by, &note, &source, &field, &fromValue, &toValue, &auto, &applied); err != nil {
			t.Fatal(err)
		}
		if from != sd.legacy || to != sd.wantState || by != "migration" || note != sd.wantRule || source != "migration" || field != "state" || fromValue != sd.legacy || toValue != sd.wantState || !auto || !applied {
			t.Errorf("%s: event %q->%q by %q note %q source %q field %q %q->%q auto=%t applied=%t", sd.name, from, to, by, note, source, field, fromValue, toValue, auto, applied)
		}
		// History stays as written: the migration only appends.
		if got := rowCount(t, pool, `SELECT count(*) FROM task_events WHERE task_id=$1`, sd.id); got != eventsBefore[sd.id]+1 {
			t.Errorf("%s: %d events after, %d before", sd.name, got, eventsBefore[sd.id])
		}
		// A claimed task whose job ended keeps refs.job_id until the next claim.
		if sd.wantRule == "claimed_ended" && (jobID == nil || *jobID == "") {
			t.Errorf("%s: refs.job_id dropped", sd.name)
		}
	}
	if old := rowCount(t, pool, `SELECT count(*) FROM tasks WHERE state IN ('reopened','claimed','in_progress','verifying','join','hold','needs_decision')`); old != 0 {
		t.Fatalf("%d tasks still hold an old state name", old)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET state='claimed' WHERE id=$1`, stateSeeds[0].id); err == nil {
		t.Fatal("tasks_state_check still allows an old name")
	}
	// Each changed task costs exactly one sync_log row: the migration states it.
	if got := rowCount(t, pool, `SELECT count(*) FROM sync_log WHERE model='task' AND action='U'`) - syncBefore; got != changed {
		t.Fatalf("sync_log rows=%d, want %d (one per changed task)", got, changed)
	}
	if rowCount(t, pool, `SELECT count(*) FROM schema_version WHERE version=32`) != 1 {
		t.Fatal("schema version 32 not recorded")
	}
	// A second run changes nothing.
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rowCount(t, pool, `SELECT count(*) FROM task_events WHERE kind='state_migration'`); got != changed {
		t.Fatalf("state_migration events after a second migrate: %d, want %d", got, changed)
	}
}

// TestStateMigrationRollbackRoundTrip migrates the seed, then changes the
// world (a task created after, tasks changed after), runs the rollback script
// and checks the doc's inverse table.
func TestStateMigrationRollbackRoundTrip(t *testing.T) {
	s, pool := legacySeededStore(t, "state_rollback")
	ctx := t.Context()
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	byName := map[string]stateSeed{}
	for _, sd := range stateSeeds {
		byName[sd.name] = sd
	}
	project := projectPtr(testProjectName)

	// Tasks created after the migration, one per new state and one flagged.
	created := map[string]string{ // new state -> expected legacy state
		"triage": "backlog", "backlog": "backlog", "unstarted": "reopened",
		"researching": "in_progress", "developing": "in_progress", "reviewing": "verifying",
		"merged": "merged", "deployed": "merged", "dropped": "dropped", "duplicate": "dropped", "icebox": "hold",
	}
	createdIDs := map[string]int64{}
	for state := range created {
		x, err := s.CreateTask(ctx, Task{Lane: "new-lane", Title: "new " + state, Kind: "implement", CreatedBy: "tester", Project: project})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE tasks SET state=$2 WHERE id=$1`, x.ID, state); err != nil {
			t.Fatal(err)
		}
		createdIDs[state] = x.ID
	}
	flagged, err := s.CreateTask(ctx, Task{Lane: "new-lane", Title: "new flagged", Kind: "implement", CreatedBy: "tester", Project: project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tasks SET state='developing', awaiting_decision=true WHERE id=$1`, flagged.ID); err != nil {
		t.Fatal(err)
	}

	// Tasks changed after the migration no longer match their snapshot, so the
	// inverse table decides them, not the snapshot.
	progressed := byName["in_progress fix"] // migrated to developing
	if _, err := s.TransitionTask(ctx, progressed.id, "reviewing", "tester", "pr", nil, ""); err != nil {
		t.Fatal(err)
	}
	reopened := byName["reopened"] // migrated to unstarted
	if _, err := s.ClaimTask(ctx, reopened.id, "w9", "job-after", ""); err != nil {
		t.Fatal(err)
	}
	answered := byName["decision live job left verifying"] // migrated to reviewing + flag
	if _, err := s.ClearAwaitingDecision(ctx, answered.id, "operator:o", "go"); err != nil {
		t.Fatal(err)
	}

	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "rollback-state-migration.sql"))
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore := rowCount(t, pool, `SELECT count(*) FROM task_events`)
	if _, err := pool.Exec(ctx, string(script)); err != nil {
		t.Fatalf("rollback script: %v", err)
	}

	read := func(id int64) (state, claimedBy string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT state, claimed_by FROM tasks WHERE id=$1`, id).Scan(&state, &claimedBy); err != nil {
			t.Fatal(err)
		}
		return state, claimedBy
	}
	// 1. Unchanged since the migration: the snapshot restores state and claimed_by.
	for _, sd := range stateSeeds {
		switch sd.name {
		case "in_progress fix", "reopened", "decision live job left verifying":
			continue
		}
		state, claimedBy := read(sd.id)
		if state != sd.legacy || claimedBy != sd.claimedBy {
			t.Errorf("%s: restored to %q/%q, want %q/%q", sd.name, state, claimedBy, sd.legacy, sd.claimedBy)
		}
	}
	// 2. Everything else maps by the inverse table.
	for state, want := range created {
		if got, _ := read(createdIDs[state]); got != want {
			t.Errorf("task created after the migration in %s: rolled back to %q, want %q", state, got, want)
		}
	}
	if got, _ := read(flagged.ID); got != "needs_decision" {
		t.Errorf("flagged developing task: rolled back to %q, want needs_decision", got)
	}
	if got, _ := read(progressed.id); got != "verifying" {
		t.Errorf("task moved to reviewing after the migration: %q, want verifying", got)
	}
	if got, _ := read(reopened.id); got != "in_progress" {
		t.Errorf("task claimed after the migration: %q, want in_progress", got)
	}
	if got, _ := read(answered.id); got != "verifying" {
		t.Errorf("task answered after the migration: %q, want verifying", got)
	}
	// 3. The old constraint is back, the version row is gone, no flag is set.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET state='reviewing' WHERE id=$1`, flagged.ID); err == nil {
		t.Fatal("the new state names are still allowed after the rollback")
	}
	if rowCount(t, pool, `SELECT count(*) FROM schema_version WHERE version=32`) != 0 {
		t.Fatal("schema version 32 still recorded")
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM tasks WHERE awaiting_decision`); n != 0 {
		t.Fatalf("%d tasks still flagged", n)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM tasks WHERE state NOT IN (`+legacyStateList+`)`); n != 0 {
		t.Fatalf("%d tasks hold a new state name", n)
	}
	// One rollback event per changed task; earlier events are not rewritten.
	rollbackEvents := rowCount(t, pool, `SELECT count(*) FROM task_events WHERE kind='state_migration' AND source='rollback'`)
	if rollbackEvents == 0 || rowCount(t, pool, `SELECT count(*) FROM task_events`) != eventsBefore+rollbackEvents {
		t.Fatalf("rollback events=%d", rollbackEvents)
	}
	if rowCount(t, pool, `SELECT count(*) FROM (SELECT task_id FROM task_events WHERE source='rollback' GROUP BY task_id HAVING count(*)>1) dup`) != 0 {
		t.Fatal("more than one rollback event for a task")
	}
	// The added tables and columns stay, unused.
	if rowCount(t, pool, `SELECT count(*) FROM information_schema.tables WHERE table_schema=current_schema() AND table_name IN ('task_state_migration','task_relations')`) != 2 {
		t.Fatal("the rollback must leave the new tables in place")
	}
	// Migrating again after a rollback works and converges.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("re-migrate after rollback: %v", err)
	}
	if n := rowCount(t, pool, `SELECT count(*) FROM tasks WHERE state NOT IN ('triage','backlog','unstarted','researching','developing','reviewing','merged','deployed','dropped','duplicate','icebox')`); n != 0 {
		t.Fatalf("%d tasks hold an old state after the second migration", n)
	}
}

func TestStateRollbackHandlesCompleted(t *testing.T) {
	s := schemaStore(t, "rollback_completed")
	x, err := s.CreateTask(t.Context(), Task{Lane: relaneLane(t, "rollback"), Title: "completed non-code task", Kind: "decide", CreatedBy: "tester", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionTaskWith(t.Context(), x.ID, TransitionInput{To: StateCompleted, By: "tester", Evidence: "hk:doc research/x"}); err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "rollback-state-migration.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), string(script)); err != nil {
		t.Fatalf("rollback completed task: %v", err)
	}
	var state, closureType string
	if err := s.pool.QueryRow(t.Context(), `SELECT state, refs->'closure'->>'type' FROM tasks WHERE id=$1`, x.ID).Scan(&state, &closureType); err != nil {
		t.Fatal(err)
	}
	if state != "merged" || closureType != "resolved" {
		t.Fatalf("rollback state=%q closure=%q", state, closureType)
	}
	if n := rowCount(t, s.pool, `SELECT count(*) FROM schema_migrations WHERE id=$1`, taskClosureMigrationID); n != 0 {
		t.Fatalf("closure migration IDs after rollback=%d, want 0", n)
	}
	if n := rowCount(t, s.pool, `SELECT count(*) FROM schema_version WHERE version=53`); n != 1 {
		t.Fatalf("schema version 53 after rollback=%d, want 1", n)
	}
	if err := s.migrate(t.Context()); err != nil {
		t.Fatalf("re-migrate after rollback: %v", err)
	}
	if n := rowCount(t, s.pool, `SELECT count(*) FROM schema_migrations WHERE id=$1`, taskClosureMigrationID); n != 1 {
		t.Fatalf("closure migration IDs after re-migration=%d, want 1", n)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE tasks SET state='completed' WHERE id=$1`, x.ID); err != nil {
		t.Fatalf("completed state after re-migration: %v", err)
	}
}
