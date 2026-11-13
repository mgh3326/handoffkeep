package store

import (
	"encoding/json"
	"testing"
)

// hk 4425: grade and tier live in refs.grade and refs.tier; a task filed
// before the move (or by a previous-release client) still carries them only
// under refs.linear, and every reader falls back to that copy.
func TestTaskRefsGradeTierFallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		refs        TaskRefs
		grade, tier string
	}{
		{"new keys", TaskRefs{Grade: "A", Tier: "T2"}, "A", "T2"},
		{"legacy only", TaskRefs{Linear: &TaskLinear{Grade: "S", Tier: "T3"}}, "S", "T3"},
		{"new wins", TaskRefs{Grade: "B", Linear: &TaskLinear{Grade: "S", Tier: "T3"}}, "B", "T3"},
		{"none", TaskRefs{}, "", ""},
	} {
		if g, tr := tc.refs.GradeTier(); g != tc.grade || tr != tc.tier {
			t.Errorf("%s: GradeTier = %q %q, want %q %q", tc.name, g, tr, tc.grade, tc.tier)
		}
	}

	// A transition patch keeps the stored keys and the legacy copy.
	old := TaskRefs{Grade: "A", Tier: "T2", Linear: &TaskLinear{Grade: "A", Tier: "T2", IssueID: "lin-1"}}
	merged := mergeTaskRefs(old, TaskRefs{PR: "o/r#1"})
	if merged.Grade != "A" || merged.Tier != "T2" || merged.Linear == nil || merged.Linear.IssueID != "lin-1" {
		t.Fatalf("merged %+v", merged)
	}
	// A previous-release client writes refs.linear.grade; it lands in
	// refs.grade too, so the new key is not left stale.
	merged = mergeTaskRefs(old, TaskRefs{Linear: &TaskLinear{Grade: "S"}})
	if g, tr := merged.GradeTier(); g != "S" || tr != "T2" || merged.Linear.Grade != "S" {
		t.Fatalf("legacy patch: %q %q %+v", g, tr, merged)
	}

	if !validTaskRefs(TaskRefs{Grade: "SS", Tier: "T0"}) || validTaskRefs(TaskRefs{Grade: "Z"}) || validTaskRefs(TaskRefs{Tier: "T9"}) {
		t.Fatal("grade/tier validation")
	}
	b, _ := json.Marshal(TaskRefs{Grade: "A", Tier: "T1"})
	if string(b) != `{"grade":"A","tier":"T1"}` {
		t.Fatalf("wire %s", b)
	}
}

// A new task writes refs.grade/refs.tier, and a transition keeps them (the
// store drops unknown refs keys, so the keys must be part of TaskRefs).
func TestTaskRefsGradeTierStored(t *testing.T) {
	s := schemaStore(t, "refs_grade_tier")
	ctx := t.Context()
	x, err := s.CreateTask(ctx, Task{Lane: "grade-lane", Title: "graded", Kind: "implement", CreatedBy: "test", Project: projectPtr(testProjectName), Refs: TaskRefs{Grade: "A", Tier: "T2"}})
	if err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT refs FROM tasks WHERE id=$1`, x.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["grade"] != "A" || stored["tier"] != "T2" || stored["linear"] != nil {
		t.Fatalf("stored refs %s", raw)
	}
	if _, err := s.TransitionTask(ctx, x.ID, "dropped", "test", "abandon", &TaskRefs{PR: "o/r#1"}, ""); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.GetTask(ctx, x.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g, tr := got.Refs.GradeTier(); g != "A" || tr != "T2" || got.Refs.PR != "o/r#1" {
		t.Fatalf("after transition %+v", got.Refs)
	}
}

// The registered migration copies refs.linear grade and tier into the new
// keys, never overwrites a key already set, and leaves refs.linear in place
// for the previous release.
func TestRefsGradeTierMigrationCopies(t *testing.T) {
	s := schemaStore(t, "refs_grade_tier_mig")
	ctx := t.Context()
	insert := func(refs string) int64 {
		var id int64
		if err := s.pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,refs,created_by,created_at,updated_at,last_activity_at)
			VALUES('mig-lane','t','implement','backlog',$1::jsonb,'test',now(),now(),now()) RETURNING id`, refs).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	legacy := insert(`{"linear":{"grade":"S+","tier":"T3","issue_id":"lin-1","sync":true}}`)
	mixed := insert(`{"grade":"B","linear":{"grade":"A","tier":"T1"}}`)
	plain := insert(`{"pr":"o/r#2"}`)
	nullLinear := insert(`{"linear":null}`)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := applyRefsGradeTierMigration(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	read := func(id int64) map[string]any {
		var raw []byte
		if err := s.pool.QueryRow(ctx, `SELECT refs FROM tasks WHERE id=$1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := read(legacy); m["grade"] != "S+" || m["tier"] != "T3" || m["linear"].(map[string]any)["issue_id"] != "lin-1" {
		t.Fatalf("legacy %v", m)
	}
	if m := read(mixed); m["grade"] != "B" || m["tier"] != "T1" || m["linear"].(map[string]any)["grade"] != "A" {
		t.Fatalf("mixed %v", m)
	}
	if m := read(plain); len(m) != 1 || m["pr"] != "o/r#2" {
		t.Fatalf("plain %v", m)
	}
	if m := read(nullLinear); m["grade"] != nil {
		t.Fatalf("null linear %v", m)
	}
}

// The YouTrack fields snapshot reads 등급/티어 from refs.grade/refs.tier, and
// from refs.linear for a task that has only the legacy copy.
func TestYTFieldsGradeTierFromRefs(t *testing.T) {
	s, bound, _ := ytfSetup(t, "ytf_refs_grade")
	check := func(step, grade, tier string) {
		t.Helper()
		ytfPass(t, s)
		ops := ytfOps(t, s, bound)
		if len(ops) == 0 {
			t.Fatalf("%s: no op", step)
		}
		p := ytfPayload(t, ops[len(ops)-1])
		if got := ytfField(p, "등급"); got != grade {
			t.Errorf("%s: 등급 = %q, want %q", step, got, grade)
		}
		if got := ytfField(p, "티어"); got != tier {
			t.Errorf("%s: 티어 = %q, want %q", step, got, tier)
		}
	}
	ytFeedExec(t, s, `UPDATE tasks SET refs='{"linear":{"grade":"B","tier":"T3"}}'::jsonb WHERE id=$1`, bound)
	check("legacy only", "B", "T3")
	ytFeedExec(t, s, `UPDATE tasks SET refs='{"grade":"S","tier":"T1","linear":{"grade":"B","tier":"T3"}}'::jsonb WHERE id=$1`, bound)
	check("new keys", "S", "T1")
}
