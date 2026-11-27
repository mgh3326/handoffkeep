package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
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
	for _, want := range []string{`table "jobs" has no tableComment`, `table "jobs" needs exactly one role label`, `comments names table "ghost", which no viewpoint lists: if the table exists, add it to a viewpoint in .tbls.yml; otherwise remove or correct this comments: entry`} {
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
	if !strings.Contains(out, "| [Tasks](viewpoint-tasks.md) | 2 |") || !strings.Contains(out, "| **All** | **3** |") {
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

// tblsTrigger is one trigger as tbls's JSON export names it.
func tblsTrigger(name, def string) map[string]string {
	return map[string]string{"name": name, "def": def}
}

// liveSchemaFile writes tbls's JSON export of a schema with the given tables,
// each carrying its triggers, and returns the file's path.
func liveSchemaFile(t *testing.T, triggers map[string][]map[string]string, tables ...string) string {
	t.Helper()
	var list []map[string]any
	for _, name := range tables {
		list = append(list, map[string]any{"name": name, "triggers": triggers[name]})
	}
	raw, err := json.Marshal(map[string]any{"tables": list})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "live.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// commentsFor is the comments list of .tbls.yml for the tables: each has a
// hand comment and a role label, as applyUsage requires.
func commentsFor(tables ...string) map[string]any {
	var list []any
	for _, name := range tables {
		list = append(list, map[string]any{"table": name, "tableComment": "The " + name + ".", "labels": []any{"history-log"}})
	}
	return map[string]any{"comments": list}
}

// commentOf is the generated tableComment of table after applyUsage.
func commentOf(t *testing.T, cfg map[string]any, table string) string {
	t.Helper()
	for _, c := range cfg["comments"].([]any) {
		m := c.(map[string]any)
		if m["table"] == table {
			return m["tableComment"].(string)
		}
	}
	t.Fatalf("no comment for table %s", table)
	return ""
}

func TestTriggerThatWritesAnotherTableIsStatedOnIt(t *testing.T) {
	live := liveSchemaFile(t, map[string][]map[string]string{
		"changes": {tblsTrigger("change_gate_events_record",
			"CREATE TRIGGER change_gate_events_record AFTER UPDATE ON changes\nFOR EACH ROW\nBEGIN\n"+
				"IF NEW.gate_state IS NOT NULL THEN\nINSERT INTO change_gate_events(change_id,state) VALUES(NEW.id,NEW.gate_state)\n"+
				"ON DUPLICATE KEY UPDATE id=id;\nEND IF;\nEND")},
	}, "changes", "change_gate_events")
	u := &Usage{Tables: tablesOf("changes", "change_gate_events"), Use: map[string]*TableUse{}}
	if err := attachTriggersFile(u, live); err != nil {
		t.Fatal(err)
	}
	cfg := commentsFor("changes", "change_gate_events")
	if err := applyUsage(cfg, u); err != nil {
		t.Fatal(err)
	}
	gate := commentOf(t, cfg, "change_gate_events")
	if !strings.Contains(gate, "Written by trigger: change_gate_events_record (AFTER UPDATE ON changes).") {
		t.Errorf("change_gate_events must name its writing trigger, got:\n%s", gate)
	}
	if strings.Contains(gate, "Written by: none.") {
		t.Errorf("a table a trigger writes must not read Written by: none., got:\n%s", gate)
	}
	if strings.Contains(commentOf(t, cfg, "changes"), "Written by trigger") {
		t.Errorf("the table a trigger fires on is not written by that trigger")
	}
}

func TestSignalTriggerIsStatedAsGuardOnItsTable(t *testing.T) {
	live := liveSchemaFile(t, map[string][]map[string]string{
		"quota_observations": {tblsTrigger("quota_history_bulk_update",
			"CREATE TRIGGER quota_history_bulk_update BEFORE UPDATE ON quota_observations\nFOR EACH ROW\n"+
				"BEGIN IF COALESCE(@hk_bulk_load, 0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'quota observations are append-only'; END IF; END")},
	}, "quota_observations", "tasks")
	u := &Usage{Tables: tablesOf("quota_observations", "tasks"), Use: map[string]*TableUse{}}
	if err := attachTriggersFile(u, live); err != nil {
		t.Fatal(err)
	}
	cfg := commentsFor("quota_observations", "tasks")
	if err := applyUsage(cfg, u); err != nil {
		t.Fatal(err)
	}
	quota := commentOf(t, cfg, "quota_observations")
	if !strings.Contains(quota, "Guarded by trigger: quota_history_bulk_update (BEFORE UPDATE ON quota_observations: refuses).") {
		t.Errorf("quota_observations must name its guard, got:\n%s", quota)
	}
	if strings.Contains(commentOf(t, cfg, "tasks"), "trigger") {
		t.Errorf("a table with no trigger must not mention one")
	}
}

func TestTableWithoutTriggerIsUnchanged(t *testing.T) {
	live := liveSchemaFile(t, map[string][]map[string]string{
		"jobs": {tblsTrigger("jobs_guard",
			"CREATE TRIGGER jobs_guard BEFORE DELETE ON jobs\nFOR EACH ROW\nBEGIN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'jobs are kept'; END")},
	}, "jobs", "tasks")
	u := &Usage{Tables: tablesOf("jobs", "tasks"), Use: map[string]*TableUse{}}
	if err := attachTriggersFile(u, live); err != nil {
		t.Fatal(err)
	}
	cfg := commentsFor("jobs", "tasks")
	if err := applyUsage(cfg, u); err != nil {
		t.Fatal(err)
	}
	want := "The tasks.\n\nCode (derived from internal/ and cmd/, tests and migrations excluded): no production statement reads or writes this table."
	if got := commentOf(t, cfg, "tasks"); got != want {
		t.Errorf("tasks comment changed:\n got %q\nwant %q", got, want)
	}
}

func TestParseTriggerReadsWritesDeletesAndGuardsInAnyKeywordCase(t *testing.T) {
	use, err := parseTrigger("changes", "tidy",
		"CREATE TRIGGER tidy BEFORE DELETE ON changes\nFOR EACH ROW\nbegin\n"+
			"insert into audit_log(id) values (old.id); update Counters set n = n + 1;\n"+
			"delete from change_checks where change_id = old.id; select id from tasks; end")
	if err != nil {
		t.Fatal(err)
	}
	if use.On != "BEFORE DELETE ON changes" {
		t.Errorf("On = %q", use.On)
	}
	if got := strings.Join(use.Writes, ","); got != "audit_log,counters" {
		t.Errorf("Writes = %s, want audit_log,counters (a SELECT is a read, not a write)", got)
	}
	if got := strings.Join(use.Deletes, ","); got != "change_checks" {
		t.Errorf("Deletes = %s, want change_checks", got)
	}
	if use.Guard {
		t.Errorf("a trigger with no SIGNAL is not a guard")
	}
}

// The trigger definitions below are the SQL the migrations create (hk 6261):
// migration_mysql_changes.go for change_gate_events_record and
// migration_mysql_tasks.go (mysqlTaskDecision) for the sync_task_decision pair.
const changeGateEventsRecordTrigger = `CREATE TRIGGER change_gate_events_record AFTER UPDATE ON changes FOR EACH ROW
BEGIN
	IF COALESCE(@hk_bulk_load,0) <> 1 AND NEW.gate_state IS NOT NULL AND NEW.gate_head IS NOT NULL AND NEW.gate_decided_at IS NOT NULL
	AND (NOT (OLD.gate_state <=> NEW.gate_state) OR NOT (OLD.gate_head <=> NEW.gate_head) OR NOT (OLD.gate_decided_at <=> NEW.gate_decided_at)) THEN
		INSERT INTO change_gate_events(change_id,head_sha,state,changed,reasons,policy_revision,decided_at,source)
		VALUES(NEW.id,NEW.gate_head,NEW.gate_state,NOT (OLD.gate_state <=> NEW.gate_state) OR NOT (OLD.gate_head <=> NEW.gate_head),COALESCE(NEW.gate_reasons,JSON_ARRAY()),NEW.gate_policy_revision,NEW.gate_decided_at,COALESCE(NEW.gate_source,'review'))
		ON DUPLICATE KEY UPDATE id=id;
	END IF;
END`

const syncTaskDecisionInsertTrigger = `CREATE TRIGGER sync_task_decision_insert AFTER INSERT ON tasks FOR EACH ROW BEGIN DECLARE request_id LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin; IF COALESCE(@hk_bulk_load,0) <> 1 THEN SET request_id = IF(JSON_TYPE(JSON_EXTRACT(NEW.refs,'$.decision_request.id')) COLLATE utf8mb4_0900_bin='STRING', JSON_UNQUOTE(JSON_EXTRACT(NEW.refs,'$.decision_request.id')) COLLATE utf8mb4_0900_bin, NULL); DELETE FROM sync_decisions WHERE task_id=NEW.id AND NOT (id <=> request_id); IF COALESCE(request_id,'') <> '' THEN INSERT INTO sync_decisions(id,task_id,updated_at) VALUES(request_id,NEW.id,NEW.updated_at) ON DUPLICATE KEY UPDATE task_id=VALUES(task_id), updated_at=VALUES(updated_at); END IF; END IF; END`

const syncTaskDecisionUpdateTrigger = `CREATE TRIGGER sync_task_decision_update AFTER UPDATE ON tasks FOR EACH ROW BEGIN DECLARE request_id LONGTEXT CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin; IF COALESCE(@hk_bulk_load,0) <> 1 THEN IF NOT (JSON_EXTRACT(OLD.refs,'$.decision_request') <=> JSON_EXTRACT(NEW.refs,'$.decision_request')) THEN SET request_id = IF(JSON_TYPE(JSON_EXTRACT(NEW.refs,'$.decision_request.id')) COLLATE utf8mb4_0900_bin='STRING', JSON_UNQUOTE(JSON_EXTRACT(NEW.refs,'$.decision_request.id')) COLLATE utf8mb4_0900_bin, NULL); DELETE FROM sync_decisions WHERE task_id=NEW.id AND NOT (id <=> request_id); IF COALESCE(request_id,'') <> '' THEN INSERT INTO sync_decisions(id,task_id,updated_at) VALUES(request_id,NEW.id,NEW.updated_at) ON DUPLICATE KEY UPDATE task_id=VALUES(task_id), updated_at=VALUES(updated_at); END IF; END IF; END IF; END`

// A real MySQL trigger writes only the table its INSERT names: the columns of
// ON DUPLICATE KEY UPDATE and the literals of the body are not tables.
func TestParseTriggerReadsOnlyRealTargetsOfMigratedTriggers(t *testing.T) {
	cases := []struct {
		table, name, def, writes, deletes string
	}{
		{"changes", "change_gate_events_record", changeGateEventsRecordTrigger, "change_gate_events", ""},
		{"tasks", "sync_task_decision_insert", syncTaskDecisionInsertTrigger, "sync_decisions", "sync_decisions"},
		{"tasks", "sync_task_decision_update", syncTaskDecisionUpdateTrigger, "sync_decisions", "sync_decisions"},
	}
	for _, c := range cases {
		use, err := parseTrigger(c.table, c.name, c.def)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := strings.Join(use.Writes, ","); got != c.writes {
			t.Errorf("%s: Writes = %q, want %q", c.name, got, c.writes)
		}
		if got := strings.Join(use.Deletes, ","); got != c.deletes {
			t.Errorf("%s: Deletes = %q, want %q", c.name, got, c.deletes)
		}
		if use.Guard {
			t.Errorf("%s: not a SIGNAL trigger", c.name)
		}
	}
}

// Keywords inside a string literal are data: they name no table and end no
// statement, and a doubled quote does not close the literal early.
func TestParseTriggerIgnoresKeywordsInsideLiterals(t *testing.T) {
	use, err := parseTrigger("tasks", "decoy",
		"CREATE TRIGGER decoy AFTER UPDATE ON tasks FOR EACH ROW BEGIN "+
			"SET @note = 'UPDATE audit_log SET x=1; DELETE FROM audit_log'; "+
			"UPDATE tasks SET title = 'it''s INSERT INTO other' WHERE id = NEW.id; END")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(use.Writes, ","); got != "tasks" {
		t.Errorf("Writes = %q, want tasks", got)
	}
	if got := strings.Join(use.Deletes, ","); got != "" {
		t.Errorf("Deletes = %q, want none", got)
	}
}

func TestParseTriggerRejectsAHeaderItDoesNotKnow(t *testing.T) {
	if _, err := parseTrigger("changes", "x", "CREATE TRIGGER x AFTER UPDATE ON changes BEGIN END"); err == nil {
		t.Fatal("a definition without FOR EACH ROW must fail, not be skipped")
	}
}

func TestTriggerOnUnlistedTableFailsConfig(t *testing.T) {
	live := liveSchemaFile(t, map[string][]map[string]string{
		"ghost": {tblsTrigger("ghost_guard", "CREATE TRIGGER ghost_guard BEFORE DELETE ON ghost\nFOR EACH ROW\nBEGIN SIGNAL SQLSTATE '45000'; END")},
	}, "ghost")
	u := &Usage{Tables: tablesOf("tasks"), Use: map[string]*TableUse{}}
	if err := attachTriggersFile(u, live); err != nil {
		t.Fatal(err)
	}
	err := applyUsage(commentsFor("tasks"), u)
	if err == nil || !strings.Contains(err.Error(), `trigger "ghost_guard" fires on table "ghost", which no viewpoint lists: add the table to a viewpoint in .tbls.yml and give it a comments: entry (tableComment and one role label)`) {
		t.Errorf("err = %v, want the unlisted-table refusal naming the remedy", err)
	}
}

// tblsForRun is a minimal .tbls.yml: one viewpoint, a comments entry per table.
const tblsForRun = `name: handoffkeep
viewpoints:
  - id: changes
    name: Changes
    tables:
      - changes
      - change_gate_events
comments:
  - table: changes
    tableComment: The changes.
    labels: [source-of-record]
  - table: change_gate_events
    tableComment: Gate decisions.
    labels: [history-log]
`

// The command path: run("config", OUT, LIVE_JSON) reads .tbls.yml and the code
// tree from the working directory and must carry the live file's triggers into
// OUT. A mutant that drops the live argument leaves the trigger line unstated.
func TestRunConfigStatesTriggerFactsFromLiveJSON(t *testing.T) {
	root := writeTree(t, map[string]string{
		".tbls.yml":                 tblsForRun,
		"internal/store/changes.go": "package store\n\nfunc (s *Store) Touch() { _ = \"SELECT id FROM changes\" }\n",
	})
	t.Chdir(root)
	live := liveSchemaFile(t, map[string][]map[string]string{
		"changes": {tblsTrigger("change_gate_events_record", changeGateEventsRecordTrigger)},
	}, "changes", "change_gate_events")
	out := filepath.Join(t.TempDir(), "config.yml")
	if err := run([]string{"config", out, live}, io.Discard); err != nil {
		t.Fatalf("config: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	gate := commentOf(t, cfg, "change_gate_events")
	if !strings.Contains(gate, "Written by trigger: change_gate_events_record (AFTER UPDATE ON changes).") {
		t.Errorf("config output must state the writing trigger from LIVE_JSON, got:\n%s", gate)
	}
	if strings.Contains(gate, "Written by: none.") {
		t.Errorf("a table a trigger writes must not read Written by: none., got:\n%s", gate)
	}
}
