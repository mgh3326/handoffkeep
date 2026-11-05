package store

import (
	"testing"
	"time"
)

func TestActivityEventRule(t *testing.T) {
	s := schemaStore(t, "activity_rule")
	for _, tc := range []struct {
		auto   bool
		source string
	}{{false, ""}, {false, "mergewake.pr"}, {true, "panewire.job"}, {true, "mergewake.merge"}, {true, "hk.auto_close"}, {true, "migration"}} {
		var sql bool
		if err := s.pool.QueryRow(t.Context(), `SELECT `+activityEventSQL+` FROM (SELECT $1::bool AS auto, $2::text AS source) e`, tc.auto, tc.source).Scan(&sql); err != nil {
			t.Fatal(err)
		}
		if got := isActivityEvent(tc.auto, tc.source); got != sql {
			t.Errorf("%+v: Go=%v SQL=%v", tc, got, sql)
		}
	}
}

func TestLastActivityMigration(t *testing.T) {
	s := schemaStore(t, "activity_migration")
	ctx := t.Context()
	for _, q := range []string{`ALTER TABLE tasks DROP COLUMN last_activity_at`, `DELETE FROM schema_version WHERE version=42`} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	var ids [3]int64
	for i := range ids {
		if err := s.pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at) VALUES('migration','task','implement','backlog','test',$1,$2) RETURNING id`, base, base.Add(time.Hour)).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	events := []struct {
		id     int
		at     int
		auto   bool
		source string
	}{{0, 1, true, "state_migration"}, {0, 2, true, "mergewake.merge"}, {1, 1, false, ""}, {1, 2, true, "hk.sweep"}, {2, 3, true, "panewire.job"}}
	for _, e := range events {
		if _, err := s.pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",at,auto,source) VALUES($1,'backlog','backlog','test',$2,$3,$4)`, ids[e.id], base.Add(time.Duration(e.at)*time.Hour), e.auto, e.source); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := execAll(ctx, tx, lastActivityV42); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		var activity, updated time.Time
		if err := s.pool.QueryRow(ctx, `SELECT last_activity_at,updated_at FROM tasks WHERE id=$1`, id).Scan(&activity, &updated); err != nil {
			t.Fatal(err)
		}
		want := base.Add(time.Duration([]int{0, 1, 3}[i]) * time.Hour)
		if !activity.Equal(want) || !updated.Equal(base.Add(time.Hour)) {
			t.Errorf("task %d: activity=%v updated=%v", i, activity, updated)
		}
	}
	var nullable string
	if err := s.pool.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='tasks' AND column_name='last_activity_at'`).Scan(&nullable); err != nil || nullable != "NO" {
		t.Fatalf("nullable=%q err=%v", nullable, err)
	}
}

func TestLastActivityMoves(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	t.Run("new task", func(t *testing.T) {
		x := flowTask(t, s, pool, "implement", StateBacklog, false)
		if !x.LastActivityAt.Equal(x.CreatedAt) {
			t.Fatalf("created=%v activity=%v", x.CreatedAt, x.LastActivityAt)
		}
	})
	t.Run("manual transition", func(t *testing.T) {
		x := flowTask(t, s, pool, "implement", StateBacklog, false)
		before := x.LastActivityAt
		got, err := s.TransitionTask(ctx, x.ID, StateUnstarted, "tester", "work", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		stored := stateOf(t, s, x.ID)
		if !stored.LastActivityAt.After(before) || !got.LastActivityAt.Equal(stored.LastActivityAt) {
			t.Fatalf("before=%v returned=%v stored=%v", before, got.LastActivityAt, stored.LastActivityAt)
		}
	})
	t.Run("merge", func(t *testing.T) {
		x := flowTask(t, s, pool, "implement", StateBacklog, false)
		send(t, s, mergeEvent(x.ID, testPR(5105)))
		got := stateOf(t, s, x.ID)
		if !got.LastActivityAt.Equal(x.LastActivityAt) || !got.UpdatedAt.After(x.UpdatedAt) {
			t.Fatalf("before=%+v after=%+v", x, got)
		}
	})
	t.Run("job", func(t *testing.T) {
		x := flowTask(t, s, pool, "implement", StateResearching, true)
		applyEvent(t, s, x.ID, testEvent(SourcePanewireJob, 5105))
		if got := stateOf(t, s, x.ID); !got.LastActivityAt.After(x.LastActivityAt) {
			t.Fatalf("before=%v after=%v", x.LastActivityAt, got.LastActivityAt)
		}
	})
	t.Run("sweep and auto close", func(t *testing.T) {
		x := flowTask(t, s, pool, "implement", StateBacklog, false)
		applyEvent(t, s, x.ID, testEvent(SourceSweep, 5105))
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		var id int64
		if err = tx.QueryRow(ctx, `SELECT id FROM tasks WHERE id=$1 FOR UPDATE`, x.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: x.ID, Kind: TaskEventAutoClose, By: "hk", At: time.Now().UTC(), Auto: true, Source: SourceAutoClose}); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if got := stateOf(t, s, x.ID); !got.LastActivityAt.Equal(x.LastActivityAt) {
			t.Fatalf("before=%v after=%v", x.LastActivityAt, got.LastActivityAt)
		}
	})
	t.Run("comment", func(t *testing.T) {
		x := flowTask(t, s, pool, "implement", StateBacklog, false)
		if _, err := s.CreateTaskComment(ctx, x.ID, "tester", "hello"); err != nil {
			t.Fatal(err)
		}
		if got := stateOf(t, s, x.ID); !got.LastActivityAt.Equal(x.LastActivityAt) {
			t.Fatal("comment moved activity")
		}
	})
	t.Run("decision and rebind", func(t *testing.T) {
		x := flowTask(t, s, pool, "implement", StateDeveloping, true)
		result, err := s.RecordDecisionRequest(ctx, x.ID, "tester", decisionInput())
		if err != nil {
			t.Fatal(err)
		}
		got := stateOf(t, s, x.ID)
		if !got.LastActivityAt.After(x.LastActivityAt) {
			t.Fatal("decision did not move activity")
		}
		event := got.Events[len(got.Events)-1]
		if event.Kind != TaskEventDecision || event.Refs == nil || event.Refs.DecisionRequest == nil || event.Refs.DecisionRequest.ID != result.Request.ID || event.From != x.State || event.To != x.State || event.Note != "decision-request "+result.Request.ID+": "+decisionInput().Question {
			t.Fatalf("decision event=%+v", event)
		}
		rebound, err := s.RebindTaskJob(ctx, x.ID, "seed-job", "new-job", "tester", "replacement")
		if err != nil {
			t.Fatal(err)
		}
		after := stateOf(t, s, x.ID)
		event = after.Events[len(after.Events)-1]
		if !after.LastActivityAt.After(got.LastActivityAt) || !rebound.LastActivityAt.Equal(after.LastActivityAt) || event.Kind != TaskEventTransition || event.From != x.State || event.To != x.State || event.Refs == nil || event.Refs.JobID != "new-job" || event.Note != "replacement" || event.Rebind == nil || event.Rebind.FromJob != "seed-job" || event.Rebind.ToJob != "new-job" {
			t.Fatalf("rebind event=%+v after=%v returned=%v", event, after.LastActivityAt, rebound.LastActivityAt)
		}
	})
}

// TestLastActivityMigrationKeepsPreviousBinaryInserts: hk deploys the CLI
// first and the server after, and a rollback runs the previous binary. Its
// INSERT INTO tasks (copied from origin/main createTask, IntakeTask and
// CreateDisposition) does not name last_activity_at, so the column must
// default instead of refusing the row.
func TestLastActivityMigrationKeepsPreviousBinaryInserts(t *testing.T) {
	s := schemaStore(t, "activity_prev_binary")
	ctx := t.Context()
	now := time.Now().UTC()
	inserts := []struct {
		name string
		sql  string
		args []any
	}{
		{"create", `INSERT INTO tasks(lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at,body_doc,project,area,expected,parent_id) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
			[]any{"prev-binary", "", "create", "implement", "backlog", 0, "{}", "", "test", now, now, "", nil, nil, "", nil}},
		{"intake", `INSERT INTO tasks(lane,title,kind,state,priority,created_by,created_at,updated_at,started_triage_at,project,parent_id,source,source_ref) VALUES($1,$2,$3,$4,$5,$6,$7,$7,$7,$8,$9,$10,$11) RETURNING id`,
			[]any{"prev-binary", "intake", "implement", StateTriage, 0, "test", now, nil, nil, "", ""}},
		{"disposition", `INSERT INTO tasks(lane,parent_lane,title,kind,state,priority,refs,claimed_by,created_by,created_at,updated_at,project,awaiting_decision) VALUES($1,'',$2,'decide','backlog',0,$3::jsonb,$4,$4,$5,$5,$6,true) RETURNING id`,
			[]any{"prev-binary", "disposition", "{}", "test", now, nil}},
	}
	for _, in := range inserts {
		var id int64
		if err := s.pool.QueryRow(ctx, in.sql, in.args...).Scan(&id); err != nil {
			t.Fatalf("%s: previous binary insert failed after v%d: %v", in.name, SchemaVersion, err)
		}
		var activity time.Time
		if err := s.pool.QueryRow(ctx, `SELECT last_activity_at FROM tasks WHERE id=$1`, id).Scan(&activity); err != nil {
			t.Fatal(err)
		}
		if d := activity.Sub(now); d < -time.Minute || d > time.Minute {
			t.Errorf("%s: default last_activity_at=%v, want about %v", in.name, activity, now)
		}
	}
}

// TestLastActivityReturnedTaskMatchesRow: a human edit moves the activity
// clock in the row, and the Task the store returns must say so too.
func TestLastActivityReturnedTaskMatchesRow(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := t.Context()
	due := time.Now().UTC().Add(48 * time.Hour)
	edits := map[string]func(id int64) (Task, error){
		"relane": func(id int64) (Task, error) {
			x, _, err := s.RelaneTask(ctx, id, "activity-returned-2", "tester", "move", true)
			return x, err
		},
		"project": func(id int64) (Task, error) {
			x, _, err := s.SetTaskProject(ctx, id, "wrk", "tester", "move")
			return x, err
		},
		"area": func(id int64) (Task, error) {
			x, _, err := s.SetTaskArea(ctx, id, areaPtr("blender"), "tester", "move")
			return x, err
		},
		"due": func(id int64) (Task, error) {
			x, _, err := s.SetTaskDue(ctx, id, &due, "tester")
			return x, err
		},
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			x := flowTask(t, s, pool, "implement", StateBacklog, false)
			got, err := edit(x.ID)
			if err != nil {
				t.Fatal(err)
			}
			stored := stateOf(t, s, x.ID)
			if !stored.LastActivityAt.After(x.LastActivityAt) || !got.LastActivityAt.Equal(stored.LastActivityAt) {
				t.Fatalf("before=%v returned=%v stored=%v", x.LastActivityAt, got.LastActivityAt, stored.LastActivityAt)
			}
		})
	}
}
