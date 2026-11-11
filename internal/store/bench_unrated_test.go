package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestBenchUnratedValidation(t *testing.T) {
	g := BenchGrade{Profile: "example", Grade: "unrated", DeviationRef: "hk:task/2639", DecidedBy: "operator"}
	if !validBenchGrade(g) {
		t.Fatal("grade store rejected unrated")
	}
	x := BenchCatalogEntry{Profile: "codex-sol", Effort: "high", ModelID: "gpt-6.1-sol", Pool: "codex", Grade: "unrated", Gate: "default", DeviationRef: "hk:task/2639", DecidedBy: "operator"}
	if !validBenchCatalogEntry(x) {
		t.Fatal("catalog rejected an unmeasured unrated Sol rung")
	}
	g.Grade, x.Grade = "unknown", "unknown"
	if validBenchGrade(g) || validBenchCatalogEntry(x) {
		t.Fatal("unknown grade was accepted")
	}
	if benchGradeRank("unrated") <= benchGradeRank("C") {
		t.Fatal("unrated was ranked as a measured grade")
	}
}

func TestTaskGradeScaleIncludesSSAboveSPlus(t *testing.T) {
	want := []string{"SS", "S+", "S", "A+", "A", "B", "C"}
	if got := TaskGrades(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TaskGrades()=%v want %v", got, want)
	}
	if !validBenchGrade(BenchGrade{Profile: "example", Grade: "SS", DeviationRef: "hk:task/4045", DecidedBy: "operator"}) {
		t.Fatal("bench grade validation rejected SS")
	}
	if !validBenchCatalogEntry(BenchCatalogEntry{Profile: "example", ModelID: "model", Pool: "codex", Grade: "SS", Gate: "default", DeviationRef: "hk:task/4045", DecidedBy: "operator"}) {
		t.Fatal("bench catalog validation rejected SS")
	}
	for rank, grade := range want {
		if got := benchGradeRank(grade); got != rank {
			t.Errorf("benchGradeRank(%q)=%d want %d", grade, got, rank)
		}
	}
	if benchGradeRank("unrated") <= benchGradeRank("C") || validTaskGrade("unrated") {
		t.Fatalf("unrated must stay bench-only: rank=%d task-valid=%v", benchGradeRank("unrated"), validTaskGrade("unrated"))
	}
	if !validTaskLinear(TaskLinear{Grade: "SS"}) {
		t.Fatal("task grade validation rejected SS")
	}
}

func TestBenchCatalogStoresAndOrdersSS(t *testing.T) {
	pool, _ := testdb.SchemaPool(t, "bench_grade_ss")
	s := &Store{pool: pool}
	ctx := t.Context()
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	row := func(profile, grade string) BenchCatalogEntry {
		return BenchCatalogEntry{Profile: profile, ModelID: "model", Pool: "codex", Grade: grade, Gate: "default", DeviationRef: "hk:task/4045", DecidedBy: "operator", DecidedAt: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
	}
	if _, err := s.UpsertBenchCatalog(ctx, []BenchCatalogEntry{row("ss-profile", "SS"), row("s-plus-profile", "S+")}); err != nil {
		t.Fatalf("store SS and S+ catalog entries: %v", err)
	}
	rows, err := s.ListBenchCatalog(ctx, "", false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		if r.Profile == "ss-profile" || r.Profile == "s-plus-profile" {
			got = append(got, r.Grade)
		}
	}
	if !reflect.DeepEqual(got, []string{"SS", "S+"}) {
		t.Fatalf("catalog grades in order=%v want [SS S+]", got)
	}
	title := "SS grade task (grade SS, T2)"
	task, err := s.CreateTask(ctx, Task{Lane: "test-grade-ss", Title: title, Kind: "implement", CreatedBy: "test-grade-ss", Project: projectPtr(testProjectName), Refs: TaskRefs{Linear: &TaskLinear{Grade: "SS", Tier: "T2"}}})
	if err != nil {
		t.Fatalf("create SS task: %v", err)
	}
	stored, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found || stored.Title != title || stored.Refs.Linear == nil || stored.Refs.Linear.Grade != "SS" {
		t.Fatalf("stored task=%+v found=%v err=%v", stored, found, err)
	}
}

// A Sol profile carries any grade the other profiles may: a ladder that tops
// out at S is accepted, and a Sol ladder that falls off is still refused by
// the monotonicity rule.
func TestBenchSolGradeLadder(t *testing.T) {
	pool, _ := testdb.SchemaPool(t, "bench_sol_ladder")
	s := &Store{pool: pool}
	ctx := t.Context()
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	row := func(effort, grade string) BenchCatalogEntry {
		return BenchCatalogEntry{Profile: "codex-sol", Effort: effort, ModelID: "sol-model", Pool: "codex", Grade: grade, Gate: "default", DeviationRef: "hk:task/3920", DecidedBy: "operator", DecidedAt: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	}
	if _, err := s.UpsertBenchCatalog(ctx, []BenchCatalogEntry{row("low", "B"), row("medium", "A"), row("high", "A+"), row("max", "S")}); err != nil {
		t.Fatalf("sol ladder topping out at S: %v", err)
	}
	if _, err := s.UpsertBenchCatalog(ctx, []BenchCatalogEntry{row("max", "B")}); !errors.Is(err, ErrBenchCatalogMonotonicity) {
		t.Fatalf("non-monotonic sol ladder: %v", err)
	}
}

// Database-backed behavior and migration checks run in CI through testdb.
func TestBenchUnratedStore(t *testing.T) {
	pool, _ := testdb.SchemaPool(t, "bench_unrated")
	s := &Store{pool: pool}
	ctx := t.Context()
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	row := func(profile, effort, grade string) BenchCatalogEntry {
		return BenchCatalogEntry{Profile: profile, Effort: effort, ModelID: "example-model", Pool: "codex", Grade: grade, Gate: "default", DeviationRef: "hk:task/2639", DecidedBy: "operator", DecidedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	}
	if _, err := s.UpsertBenchCatalog(ctx, []BenchCatalogEntry{
		row("example", "low", "B"), row("example", "medium", "unrated"), row("example", "high", "A"), row("example", "max", "unrated"), row("codex-sol", "high", "unrated"),
	}); err != nil {
		t.Fatalf("unrated catalog/monotonicity: %v", err)
	}
	if _, err := s.UpsertBenchCatalog(ctx, []BenchCatalogEntry{row("example", "high", "C")}); !errors.Is(err, ErrBenchCatalogMonotonicity) {
		t.Fatalf("rated violation across unrated rung: %v", err)
	}
	for _, profile := range []string{"legacy", "codex-sol"} {
		if _, err := s.UpsertBenchGrades(ctx, []BenchGrade{{Profile: profile, Grade: "unrated", DecidedBy: "operator", DeviationRef: "hk:task/2639"}}); err != nil {
			t.Fatalf("grades set %s: %v", profile, err)
		}
	}
	grades, err := s.ListBenchGrades(ctx)
	if err != nil || len(grades) != 2 {
		t.Fatalf("grade projection: rows=%+v err=%v", grades, err)
	}
	for _, g := range grades {
		if g.Grade != "unrated" {
			t.Fatalf("grade projection changed unrated: %+v", g)
		}
	}
	rows, err := s.ListBenchCatalog(ctx, "", false)
	if err != nil || len(rows) != 7 {
		t.Fatalf("catalog listing: rows=%+v err=%v", rows, err)
	}
	seenUnrated := false
	for _, r := range rows {
		if r.Pool != "codex" {
			continue
		}
		if r.Grade == "unrated" {
			seenUnrated = true
		} else if seenUnrated {
			t.Fatalf("rated row sorted after unrated: %+v", rows)
		}
	}
}

func TestBenchUnratedSolGradeMirrorPreservesScore(t *testing.T) {
	for _, profile := range []string{"codex-sol", "kiro-sol"} {
		t.Run(profile, func(t *testing.T) {
			pool, _ := testdb.SchemaPool(t, "bench_unrated_sol_mirror")
			s := &Store{pool: pool}
			ctx := t.Context()
			if err := s.migrate(ctx); err != nil {
				t.Fatal(err)
			}
			score, source := 67.0, "measured-source"
			row := BenchCatalogEntry{Profile: profile, ModelID: "sol-model", Pool: "codex", Grade: "S+", Score: &score, BenchmarkSource: &source, Gate: "default", DeviationRef: "measured-decision", DecidedBy: "operator"}
			if _, err := s.UpsertBenchCatalog(ctx, []BenchCatalogEntry{row}); err != nil {
				t.Fatal(err)
			}
			beforeCatalog, err := s.ListBenchCatalog(ctx, "", false)
			if err != nil {
				t.Fatal(err)
			}
			beforeGrades, err := s.ListBenchGrades(ctx)
			if err != nil {
				t.Fatal(err)
			}
			grade := BenchGrade{Profile: profile, Grade: "unrated", DeviationRef: "hk:task/2639", DecidedBy: "operator"}
			_, err = s.UpsertBenchGrades(ctx, []BenchGrade{
				{Profile: "earlier-in-batch", Grade: "unrated", DeviationRef: "hk:task/2639", DecidedBy: "operator"},
				grade,
			})
			if !errors.Is(err, ErrBenchCatalogScoredSolUnrated) {
				t.Fatalf("scored Sol became unrated through the grade mirror: %v", err)
			}
			afterCatalog, err := s.ListBenchCatalog(ctx, "", false)
			if err != nil || !reflect.DeepEqual(afterCatalog, beforeCatalog) {
				t.Fatalf("rejected grade batch changed catalog: before=%+v after=%+v err=%v", beforeCatalog, afterCatalog, err)
			}
			afterGrades, err := s.ListBenchGrades(ctx)
			if err != nil || !reflect.DeepEqual(afterGrades, beforeGrades) {
				t.Fatalf("rejected grade batch changed grade projection: before=%+v after=%+v err=%v", beforeGrades, afterGrades, err)
			}
			var storedGrade string
			if err := pool.QueryRow(ctx, `SELECT grade FROM bench_grades WHERE profile=$1`, profile).Scan(&storedGrade); err != nil || storedGrade != "S+" {
				t.Fatalf("rejected grade batch changed legacy grade: grade=%s err=%v", storedGrade, err)
			}
			row.Score = nil
			if _, err := s.UpsertBenchCatalog(ctx, []BenchCatalogEntry{row}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpsertBenchGrades(ctx, []BenchGrade{grade}); err != nil {
				t.Fatalf("unscored Sol rejected after explicit score removal: %v", err)
			}
			rows, err := s.ListBenchCatalog(ctx, "", false)
			if err != nil || len(rows) != 1 || rows[0].Grade != "unrated" || rows[0].Score != nil || rows[0].ModelID != row.ModelID || rows[0].BenchmarkSource == nil || *rows[0].BenchmarkSource != source {
				t.Fatalf("unscored Sol grade mirror: rows=%+v err=%v", rows, err)
			}
		})
	}
}

func TestBenchUnratedV28Upgrade(t *testing.T) {
	pool, _ := testdb.SchemaPool(t, "bench_unrated_v28")
	s := &Store{pool: pool}
	ctx := t.Context()
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM schema_version WHERE version=28`,
		`ALTER TABLE bench_grades DROP CONSTRAINT bench_grades_grade_check, ADD CONSTRAINT bench_grades_grade_check CHECK(grade IN ('S+','S','A+','A','B','C'))`,
		`ALTER TABLE bench_catalog DROP CONSTRAINT bench_catalog_grade_check, ADD CONSTRAINT bench_catalog_grade_check CHECK(grade IN ('S+','S','A+','A','B','C'))`,
		`INSERT INTO bench_grades(profile,grade,deviation_ref,decided_by,decided_at) VALUES('historic','C','old-decision','operator',now())`,
		`INSERT INTO bench_catalog(profile,effort,model_id,pool,grade,deviation_ref,decided_by,decided_at) VALUES('historic','','model','codex','C','old-decision','operator',now())`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var before []uint32
	if err := pool.QueryRow(ctx, `SELECT array_agg(oid ORDER BY conname) FROM pg_constraint WHERE conrelid IN ('bench_grades'::regclass,'bench_catalog'::regclass) AND conname IN ('bench_grades_grade_check','bench_catalog_grade_check')`).Scan(&before); err != nil || len(before) != 2 {
		t.Fatalf("grade constraints: %v %v", before, err)
	}
	for _, table := range []string{"bench_grades", "bench_catalog"} {
		var grade string
		if err := pool.QueryRow(ctx, `SELECT grade FROM `+table+` WHERE profile='historic'`).Scan(&grade); err != nil || grade != "C" {
			t.Fatalf("migration changed historic %s grade=%s err=%v", table, grade, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE `+table+` SET grade='unrated' WHERE profile='historic'`); err != nil {
			t.Fatalf("migrated %s constraint rejects unrated: %v", table, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE `+table+` SET grade='unknown' WHERE profile='historic'`); err == nil {
			t.Fatalf("migrated %s constraint accepts unknown grade", table)
		}
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var same bool
	if err := pool.QueryRow(ctx, `SELECT array_agg(oid ORDER BY conname)=$1::oid[] FROM pg_constraint WHERE conrelid IN ('bench_grades'::regclass,'bench_catalog'::regclass) AND conname IN ('bench_grades_grade_check','bench_catalog_grade_check')`, before).Scan(&same); err != nil || !same {
		t.Fatalf("v28 replaced constraints on repeat migration: same=%t err=%v", same, err)
	}
}
