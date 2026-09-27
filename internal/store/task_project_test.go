package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// projectPtr makes a *string for a valid task project name. Every helper-fed
// task create uses a seeded vocabulary entry so the requireTaskProject gate
// accepts it.
func projectPtr(v string) *string { return &v }

const testProjectName = "experiment"

func taskProjectLane(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("proj-%d", time.Now().UnixNano())
}

// TestCreateTaskRequiresProject is the assertion-RED seam for the required
// gate: a create without a project, with an empty one, and with a
// shape-invalid one are all refused before a row exists.
func TestCreateTaskRequiresProject(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane := taskProjectLane(t)
	for _, tc := range []struct {
		name    string
		project *string
		want    error
	}{
		{"missing", nil, ErrTaskProjectRequired},
		{"empty", projectPtr(""), ErrTaskProjectRequired},
		{"bad-shape", projectPtr("bad name!"), ErrTaskProjectUnknown},
		{"unregistered", projectPtr("not-a-project"), ErrTaskProjectUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateTask(ctx, Task{Lane: lane, Title: "gated", Kind: "implement", CreatedBy: "proj-test", Project: tc.project})
			if !errors.Is(err, tc.want) {
				t.Fatalf("CreateTask err=%v, want %v", err, tc.want)
			}
		})
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE lane=$1`, lane).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused creates left %d rows err=%v", rows, err)
	}
	x, err := s.CreateTask(ctx, Task{Lane: lane, Title: "classified", Kind: "implement", CreatedBy: "proj-test", Project: projectPtr(testProjectName)})
	if err != nil || x.Project == nil || *x.Project != testProjectName {
		t.Fatalf("CreateTask=%+v err=%v", x, err)
	}
}

// TestSetTaskProjectEvent covers the relane-like contract: a kind='project'
// event carries old→new, a no-op records nothing, unknown names are refused,
// and terminal rows may still be reclassified (project is metadata, not
// lifecycle).
func TestSetTaskProjectEvent(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane := taskProjectLane(t)
	x, err := s.CreateTask(ctx, Task{Lane: lane, Title: "reclassify me", Kind: "implement", CreatedBy: "proj-test", Project: projectPtr("handoffkeep")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SetTaskProject(ctx, x.ID, "not-a-project", "proj-test", "nope"); !errors.Is(err, ErrTaskProjectUnknown) {
		t.Fatalf("SetTaskProject(unknown) err=%v", err)
	}
	x, changed, err := s.SetTaskProject(ctx, x.ID, "wrk", "proj-test", "moving to wrk work")
	if err != nil || !changed || x.Project == nil || *x.Project != "wrk" {
		t.Fatalf("SetTaskProject=%+v changed=%t err=%v", x, changed, err)
	}
	var kind, from, to, note string
	if err = pool.QueryRow(ctx, `SELECT kind,"from","to",note FROM task_events WHERE task_id=$1 AND kind='project'`, x.ID).Scan(&kind, &from, &to, &note); err != nil {
		t.Fatalf("project event missing: %v", err)
	}
	if kind != "project" || from != "handoffkeep" || to != "wrk" || !strings.Contains(note, "wrk") {
		t.Fatalf("event=(%s,%s,%s,%s)", kind, from, to, note)
	}
	x, changed, err = s.SetTaskProject(ctx, x.ID, "wrk", "proj-test", "already there")
	if err != nil || changed {
		t.Fatalf("no-op changed=%t err=%v", changed, err)
	}
	var events int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM task_events WHERE task_id=$1 AND kind='project'`, x.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("no-op wrote an event: %d", events)
	}
	if _, _, err = s.SetTaskProject(ctx, 999999, "wrk", "proj-test", "ghost"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("SetTaskProject(missing) err=%v", err)
	}
	// Terminal rows stay reclassifiable — project is metadata, not lifecycle.
	if _, err = s.TransitionTask(ctx, x.ID, "dropped", "proj-test", "", nil, ""); err != nil {
		t.Fatalf("drop for terminal-relabel probe: %v", err)
	}
	x, changed, err = s.SetTaskProject(ctx, x.ID, "fleet-ops", "proj-test", "post-close reclass")
	if err != nil || !changed || x.Project == nil || *x.Project != "fleet-ops" || x.State != "dropped" {
		t.Fatalf("terminal SetTaskProject=%+v changed=%t err=%v", x, changed, err)
	}
	var lastTo string
	if err = pool.QueryRow(ctx, `SELECT "to" FROM task_events WHERE task_id=$1 AND kind='project' ORDER BY id DESC LIMIT 1`, x.ID).Scan(&lastTo); err != nil || lastTo != "fleet-ops" {
		t.Fatalf("terminal project event to=%q err=%v", lastTo, err)
	}
}

// TestTaskProjectVocabulary covers seeding, listing, and extension.
func TestTaskProjectVocabulary(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	names, err := s.ListTaskProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seeded := map[string]bool{}
	for _, n := range names {
		seeded[n] = true
	}
	for _, want := range []string{"auto_trader", "handoffkeep", "wrk", "other", "experiment", "robin-prefect-automations"} {
		if !seeded[want] {
			t.Fatalf("seed vocabulary missing %q (have %v)", want, names)
		}
	}
	if len(names) != 18 {
		t.Fatalf("seed vocabulary has %d names, want 18: %v", len(names), names)
	}
	created, err := s.AddTaskProject(ctx, "new-probe", "proj-test")
	if err != nil || !created {
		t.Fatalf("AddTaskProject created=%t err=%v", created, err)
	}
	created, err = s.AddTaskProject(ctx, "new-probe", "proj-test")
	if err != nil || created {
		t.Fatalf("AddTaskProject duplicate created=%t err=%v", created, err)
	}
	if _, err = s.AddTaskProject(ctx, "bad name!", "proj-test"); err == nil {
		t.Fatal("AddTaskProject accepted an invalid name")
	}
	x, err := s.CreateTask(ctx, Task{Lane: taskProjectLane(t), Title: "vocabulary grows", Kind: "implement", CreatedBy: "proj-test", Project: projectPtr("new-probe")})
	if err != nil || x.Project == nil || *x.Project != "new-probe" {
		t.Fatalf("CreateTask with added project=%+v err=%v", x, err)
	}
}

// TestListTasksProjectFilter covers the tri-state filter: unset lists all,
// a name selects its rows, and "" selects only NULL-project legacy rows.
func TestListTasksProjectFilter(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane := taskProjectLane(t)
	a, err := s.CreateTask(ctx, Task{Lane: lane, Title: "alpha", Kind: "implement", CreatedBy: "proj-test", Project: projectPtr("herdr")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateTask(ctx, Task{Lane: lane, Title: "beta", Kind: "implement", CreatedBy: "proj-test", Project: projectPtr("wrk")})
	if err != nil {
		t.Fatal(err)
	}
	// A legacy row predating the field: inserted directly, no project.
	var legacy int64
	if err = pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at) VALUES($1,'legacy','implement','backlog','proj-test',now(),now()) RETURNING id`, lane).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListTasks(ctx, lane, "", "", nil, 10)
	if err != nil || len(all) != 3 {
		t.Fatalf("unfiltered=%d err=%v", len(all), err)
	}
	only, err := s.ListTasks(ctx, lane, "", "", projectPtr("wrk"), 10)
	if err != nil || len(only) != 1 || only[0].ID != b.ID || only[0].Project == nil || *only[0].Project != "wrk" {
		t.Fatalf("project=wrk filter=%+v err=%v", only, err)
	}
	none, err := s.ListTasks(ctx, lane, "", "", projectPtr(""), 10)
	if err != nil || len(none) != 1 || none[0].ID != legacy || none[0].Project != nil {
		t.Fatalf("project='' filter=%+v err=%v", none, err)
	}
	page, err := s.ListTasksPage(ctx, lane, "", "", projectPtr("herdr"), 0, 10)
	if err != nil || len(page) != 1 || page[0].ID != a.ID {
		t.Fatalf("paged project=herdr=%+v err=%v", page, err)
	}
	if _, err = s.ListTasks(ctx, lane, "", "", projectPtr("bad name!"), 10); err == nil {
		t.Fatal("invalid project filter shape accepted")
	}
	// A valid but unused name is a legitimate filter returning zero rows.
	empty, err := s.ListTasks(ctx, lane, "", "", projectPtr("admiral"), 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("unused project filter=%+v err=%v", empty, err)
	}
}

// TestCreateDispositionRequiresProject pins the director's call-site-inventory
// requirement (hk report/2026-09-28/tasks-add-inventory-763): the decide task
// behind CreateDisposition is a direct INSERT INTO tasks, so it must pass the
// same requireTaskProject gate — absent and unknown projects are refused
// before the row exists, and a valid project lands on the created row.
func TestCreateDispositionRequiresProject(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane := taskProjectLane(t)
	merged := time.Now().UTC().Add(-time.Hour)
	pull := fmt.Sprintf("https://github.com/example/proj-gate/pull/%d", time.Now().UnixNano()%1_000_000_000)
	base := DispositionInput{Lane: lane, Title: "[처분] gate probe", OriginPR: pull, MergeSHA: strings.Repeat("a", 40), MergedAt: &merged,
		Install: DispositionInstall{State: "unknown"}, Recommended: "E", CreatedBy: "proj-test"}
	for _, tc := range []struct {
		name    string
		project string
		want    error
	}{
		{"missing", "", ErrTaskProjectRequired},
		{"bad-shape", "bad name!", ErrTaskProjectUnknown},
		{"unregistered", "not-a-project", ErrTaskProjectUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			in.Project = tc.project
			_, created, err := s.CreateDisposition(ctx, in)
			if !errors.Is(err, tc.want) || created {
				t.Fatalf("CreateDisposition created=%t err=%v, want %v", created, err, tc.want)
			}
		})
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE lane=$1`, lane).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused dispositions left %d rows err=%v", rows, err)
	}
	in := base
	in.Project = "herdr"
	x, created, err := s.CreateDisposition(ctx, in)
	if err != nil || !created || x.Project == nil || *x.Project != "herdr" || x.Kind != "decide" || x.State != "needs_decision" {
		t.Fatalf("CreateDisposition=%+v created=%t err=%v", x, created, err)
	}
}

// TestTaskProjectMigrationIsAdditive runs v15 against a schema holding rows
// created before the column existed and proves the column arrives nullable,
// the legacy row keeps NULL, the seed lands, and a second migrate is a no-op.
func TestTaskProjectMigrationIsAdditive(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	lane := taskProjectLane(t)
	// Rewind to the pre-v15 shape: drop the column, the table, the marker,
	// and narrow the event CHECK back to v14.
	for _, q := range []string{
		`DELETE FROM schema_version WHERE version=15`,
		`ALTER TABLE tasks DROP COLUMN project`,
		`DROP TABLE task_projects`,
		`ALTER TABLE task_events DROP CONSTRAINT task_events_kind_check`,
		`ALTER TABLE task_events ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane','decision'))`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var legacy int64
	if err := pool.QueryRow(ctx, `INSERT INTO tasks(lane,title,kind,state,created_by,created_at,updated_at) VALUES($1,'pre-v15','implement','backlog','proj-test',now(),now()) RETURNING id`, lane).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("v15 migrate: %v", err)
	}
	var isNull bool
	if err := pool.QueryRow(ctx, `SELECT project IS NULL FROM tasks WHERE id=$1`, legacy).Scan(&isNull); err != nil || !isNull {
		t.Fatalf("legacy row project NULL=%t err=%v", isNull, err)
	}
	var seeded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM task_projects`).Scan(&seeded); err != nil || seeded != 18 {
		t.Fatalf("task_projects rows=%d err=%v", seeded, err)
	}
	var version int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=15`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 15 rows=%d err=%v", version, err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=15`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version 15 rows after restart=%d err=%v", version, err)
	}
	// A project event writes against the widened CHECK.
	x, err := s.CreateTask(ctx, Task{Lane: lane, Title: "post-migration", Kind: "implement", CreatedBy: "proj-test", Project: projectPtr("wrk")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.SetTaskProject(ctx, x.ID, "herdr", "proj-test", "check widened"); err != nil {
		t.Fatalf("SetTaskProject on v15 schema: %v", err)
	}
	// The gate still applies to a create on the migrated schema.
	if _, err = s.CreateTask(ctx, Task{Lane: lane, Title: "no project", Kind: "implement", CreatedBy: "proj-test"}); !errors.Is(err, ErrTaskProjectRequired) {
		t.Fatalf("ungated create err=%v", err)
	}
}
