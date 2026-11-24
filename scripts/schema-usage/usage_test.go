package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree writes files under a fresh root and returns it. Paths are relative
// to the root, as the scanner walks internal/ and cmd/.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func tablesOf(names ...string) map[string]bool {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return set
}

func unitNames(set map[Unit]bool) []string {
	var out []string
	for u := range set {
		out = append(out, u.String())
	}
	return out
}

func TestScanClassifiesStatementsByKeyword(t *testing.T) {
	root := writeTree(t, map[string]string{
		"internal/store/tasks.go": `package store

func (s *Store) ListTasks() {
	_ = "SELECT id FROM tasks t JOIN task_events e ON e.task_id = t.id"
}

func (s *Store) CreateTask() {
	_ = "INSERT INTO tasks(id) VALUES ($1)"
}

func (s *Store) PruneEvents() {
	_ = "DELETE FROM task_events WHERE created_at < $1"
}

func (s *Store) Touch() {
	_ = ` + "`UPDATE tasks SET title=$1 WHERE id=$2`" + `
}

func logLine() {
	println("failed to insert into tasks; select from task_events")
}
`,
	})
	u, err := Scan(root, tablesOf("tasks", "task_events"))
	if err != nil {
		t.Fatal(err)
	}

	tasks := u.Use["tasks"]
	if tasks == nil {
		t.Fatal("tasks has no usage")
	}
	if got := strings.Join(unitNames(tasks.Reads), ","); !strings.Contains(got, "internal/store/tasks.go:Store.ListTasks") {
		t.Errorf("tasks reads = %s, want Store.ListTasks", got)
	}
	writes := strings.Join(unitNames(tasks.Writes), ",")
	for _, want := range []string{"Store.CreateTask", "Store.Touch"} {
		if !strings.Contains(writes, want) {
			t.Errorf("tasks writes = %s, want %s", writes, want)
		}
	}
	if strings.Contains(writes, "logLine") {
		t.Errorf("lower-case prose must not count as SQL, got writes %s", writes)
	}

	events := u.Use["task_events"]
	if events == nil {
		t.Fatal("task_events has no usage")
	}
	if len(events.Deletes) != 1 {
		t.Fatalf("task_events deletes = %v, want one PruneEvents site", events.Deletes)
	}
	for unit, stmts := range events.Deletes {
		if unit.Func != "Store.PruneEvents" || !strings.HasPrefix(stmts[0], "DELETE FROM task_events") {
			t.Errorf("delete site = %s %v", unit, stmts)
		}
	}
	if !events.Joins["tasks"] || !tasks.Joins["task_events"] {
		t.Errorf("a JOIN must record both tables as joined: tasks=%v task_events=%v", tasks.Joins, events.Joins)
	}
}

func TestScanJoinsConcatenatedLiterals(t *testing.T) {
	root := writeTree(t, map[string]string{
		"internal/store/split.go": `package store

func (s *Store) Split() {
	_ = "SELECT id FROM " +
		"tasks WHERE" + " id=1"
}
`,
	})
	u, err := Scan(root, tablesOf("tasks"))
	if err != nil {
		t.Fatal(err)
	}
	// The pieces join to "SELECT id FROM tasks WHERE id=1": one read, by Split.
	use := u.Use["tasks"]
	if use == nil || !use.Reads[Unit{File: "internal/store/split.go", Func: "Store.Split"}] {
		t.Errorf("split literal must read tasks through its joined statement, got %+v", use)
	}
}

func TestScanSkipsMigrationBodiesButKeepsRunner(t *testing.T) {
	root := writeTree(t, map[string]string{
		"internal/store/migration_tasks.go": `package store

func init() {
	registerMigration(migration{ID: "20260101-x", Apply: func() {
		_ = "UPDATE tasks SET x=1"
	}})
}
`,
		"internal/store/migration_ids.go": `package store

func migrateRegistered() {
	_ = "INSERT INTO schema_migrations(id, applied_at) VALUES ($1, now())"
}
`,
		"internal/store/store_test.go": `package store

func TestX() { _ = "DELETE FROM tasks" }
`,
	})
	u, err := Scan(root, tablesOf("tasks", "schema_migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if use := u.Use["tasks"]; use != nil && len(use.Writes) > 0 {
		t.Errorf("migration body and test file must not count, got writes %v", unitNames(use.Writes))
	}
	if use := u.Use["schema_migrations"]; use == nil || !use.Writes[Unit{File: "internal/store/migration_ids.go", Func: "migrateRegistered"}] {
		t.Errorf("the migration runner must count as a writer of schema_migrations, got %+v", use)
	}
}

func TestApplyUsageRequiresCommentAndRole(t *testing.T) {
	u := &Usage{Tables: tablesOf("tasks", "jobs"), Use: map[string]*TableUse{}}
	cfg := map[string]any{"comments": []any{
		map[string]any{"table": "tasks", "tableComment": "The task.", "labels": []any{"source-of-record"}},
		map[string]any{"table": "jobs", "labels": []any{"not-a-role"}},
		map[string]any{"table": "ghost", "tableComment": "x", "labels": []any{"config"}},
	}}
	err := applyUsage(cfg, u)
	if err == nil {
		t.Fatal("want errors for a table with no role, a comment naming an unknown table")
	}
	for _, want := range []string{`table "jobs" has no tableComment`, `table "jobs" needs exactly one role label`, `comments names table "ghost"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestApplyUsageAppendsDerivedLines(t *testing.T) {
	u := &Usage{Tables: tablesOf("tasks"), Use: map[string]*TableUse{}}
	use := u.table("tasks")
	use.Reads[Unit{File: "internal/store/tasks.go", Func: "Store.ListTasks"}] = true
	cfg := map[string]any{
		"desc": "intro",
		"comments": []any{map[string]any{
			"table": "tasks", "tableComment": "The task.\nRetention: none.", "labels": []any{"source-of-record"},
		}},
	}
	if err := applyUsage(cfg, u); err != nil {
		t.Fatal(err)
	}
	c := cfg["comments"].([]any)[0].(map[string]any)["tableComment"].(string)
	if !strings.HasPrefix(c, "The task.\nRetention: none.") || !strings.Contains(c, "Read by: internal/store/tasks.go (Store.ListTasks).") {
		t.Errorf("tableComment = %q", c)
	}
	if !strings.Contains(cfg["desc"].(string), "**Roles.**") {
		t.Errorf("desc lacks the role legend: %q", cfg["desc"])
	}
}

func TestDomainSummaryDrawsDeclaredRelationsDashed(t *testing.T) {
	vps := []viewpoint{
		{id: "tasks", name: "Tasks", tables: []string{"tasks", "jobs"}},
		{id: "decisions", name: "Decisions", tables: []string{"decision_requests", "jobs"}},
	}
	var s schemaJSON
	s.Tables = []struct {
		Name string `json:"name"`
	}{{"tasks"}, {"jobs"}, {"decision_requests"}}
	s.Relations = []struct {
		Table       string `json:"table"`
		ParentTable string `json:"parent_table"`
		Virtual     bool   `json:"virtual"`
	}{
		{Table: "jobs", ParentTable: "tasks", Virtual: false},
		{Table: "decision_requests", ParentTable: "tasks", Virtual: true},
	}
	out, err := domainSummary(vps, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "decisions -.->|\"1 declared\"| tasks") {
		t.Errorf("declared relation must be dashed, got:\n%s", out)
	}
	if strings.Contains(out, "tasks -->|\"1 foreign keys\"| tasks") {
		t.Errorf("a relation inside one domain is not an edge:\n%s", out)
	}
	if !strings.Contains(out, "| Tasks | 2 |") || !strings.Contains(out, "| **All** | **3** |") {
		t.Errorf("counts wrong:\n%s", out)
	}
}

func TestDomainSummaryRejectsUnlistedTable(t *testing.T) {
	s := schemaJSON{}
	s.Tables = []struct {
		Name string `json:"name"`
	}{{"orphan"}}
	if _, err := domainSummary([]viewpoint{{id: "tasks", name: "Tasks"}}, s); err == nil {
		t.Fatal("a table in no viewpoint must be an error")
	}
}

func TestInsertAfterTitle(t *testing.T) {
	got := insertAfterTitle("# handoffkeep\n\nbody\n", "SECTION\n")
	if !strings.HasPrefix(got, "# handoffkeep\n\nSECTION\n") || !strings.Contains(got, "body") {
		t.Errorf("got %q", got)
	}
}
