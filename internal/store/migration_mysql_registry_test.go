package store

import (
	"strings"
	"testing"
)

// These PostgreSQL migrations predate the MySQL baseline. New registrations
// must have an identically named MySQL migration, even for an area whose
// existing tables have not yet been ported.
const beforeMySQLBaseline = `
20261106-deploy-records 20261106-standing-orders 20261106-task-closure
20261107-deploy-services 20261107-operator-inbox-state 20261107-project-design-lane 20261107-translations 20261107-youtrack-sync
20261108-dots-run-tokens 20261108-job-selection 20261108-mcp-dots-writes 20261108-tasks-title-trgm 20261108-youtrack-attempts 20261108-youtrack-decisions 20261108-youtrack-docs
20261109-attachment-object-location 20261109-attachment-task-ref 20261109-attachment-uploads 20261109-attachment-uploads-release 20261109-deploy-record-projection 20261109-yt-comment-canary 20261109-yt-doc-legacy-writes 20261109-yt-doc-parts 20261109-yt-write-ops
20261110-yt-task-bindings 20261110-yt-write-op-alert
20261111-task-thread-deliveries 20261111-yt-binding-comments-from 20261111-yt-feed 20261111-yt-task-binding-origin
20261112-decision-record-feed 20261112-grade-scale-ss 20261112-yt-article-bindings 20261112-yt-authority 20261112-yt-comment-queued-target 20261112-yt-decision-bindings 20261112-yt-task-map-guard 20261112-yt-write-op-alert-batch
20261113-change-gate-events 20261113-decision-mcp-channel 20261113-dots-adoption-canva 20261113-dots-blocked-kind 20261113-outbox-receipts 20261113-refs-grade-tier 20261113-session-decisions 20261113-task-research 20261113-task-spawn-record 20261113-yt-binding-history 20261113-yt-binding-intents 20261113-yt-comment-hold 20261113-yt-entity-map-tenant 20261113-yt-issue-claim-target 20261113-yt-issue-intents
20261114-attachment-upload-claim 20261114-deploy-lag-outbox 20261114-deploy-record-projection-failed-step 20261114-yt-issue-adoption 20261114-yt-issue-links
20261115-event-comment-keys 20261115-yt-family-roots 20261115-yt-link-steps
20261116-attachment-archive 20261116-documents-key-prefix 20261116-yt-write-path-indexes
20261117-dots-question-answer 20261117-outbox-skipped`

func TestMySQLMigrationRegistryCoversNewPostgresMigrations(t *testing.T) {
	known := map[string]bool{}
	for _, id := range strings.Fields(beforeMySQLBaseline) {
		known[id] = true
	}
	for _, m := range mysqlMigrations {
		if err := checkMigrationID(m.id); err != nil {
			t.Error(err)
		}
		if len(m.steps) == 0 {
			t.Errorf("MySQL migration %s has no steps", m.id)
		}
		known[m.id] = true
	}
	for _, m := range migrationRegistry {
		if !known[m.ID] {
			t.Errorf("PostgreSQL migration %s has no MySQL migration", m.ID)
		}
	}
}

func TestOrderMySQLMigrations(t *testing.T) {
	a := mysqlMigration{id: "20261117-a", steps: []string{"SELECT 1"}}
	b := mysqlMigration{id: "20261117-b", after: []string{a.id}, steps: []string{"SELECT 2"}}
	ordered, err := orderMySQLMigrations([]mysqlMigration{b, a})
	if err != nil || len(ordered) != 2 || ordered[0].id != a.id || ordered[1].id != b.id {
		t.Fatalf("order: %+v %v", ordered, err)
	}
	b.after = []string{"20261117-missing"}
	if _, err := orderMySQLMigrations([]mysqlMigration{a, b}); err == nil {
		t.Fatal("missing dependency accepted")
	}
	b.after = []string{a.id}
	a.after = []string{b.id}
	if _, err := orderMySQLMigrations([]mysqlMigration{a, b}); err == nil {
		t.Fatal("cycle accepted")
	}
}

func TestMySQLStepGuardRoundTrip(t *testing.T) {
	for _, tc := range []struct{ step, kind, table, object, stmt string }{
		{mysqlIfNoColumn("tasks", "lane", "ALTER TABLE tasks ADD COLUMN lane INT"), "column", "tasks", "lane", "ALTER TABLE tasks ADD COLUMN lane INT"},
		{mysqlIfColumnNotNull("jobs", "task_id", "ALTER TABLE jobs MODIFY task_id BIGINT NULL"), "not_null", "jobs", "task_id", "ALTER TABLE jobs MODIFY task_id BIGINT NULL"},
		{mysqlIfNoIndex("tasks", "tasks_parent", "CREATE INDEX tasks_parent ON tasks(parent_id)"), "index", "tasks", "tasks_parent", "CREATE INDEX tasks_parent ON tasks(parent_id)"},
		{mysqlIfNoConstraint("tasks", "tasks_state_check", "ALTER TABLE tasks ADD CONSTRAINT tasks_state_check CHECK(state<>'')"), "constraint", "tasks", "tasks_state_check", "ALTER TABLE tasks ADD CONSTRAINT tasks_state_check CHECK(state<>'')"},
		{mysqlIfConstraint("tasks", "old_check", "ALTER TABLE tasks DROP CHECK old_check"), "has_constraint", "tasks", "old_check", "ALTER TABLE tasks DROP CHECK old_check"},
	} {
		kind, table, object, stmt, guarded := parseMySQLGuard(tc.step)
		if !guarded || kind != tc.kind || table != tc.table || object != tc.object || stmt != tc.stmt {
			t.Errorf("parse(%q) = %q %q %q %q %v", tc.step, kind, table, object, stmt, guarded)
		}
	}
	if _, _, _, stmt, guarded := parseMySQLGuard("CREATE TABLE t (id INT)"); guarded || stmt != "CREATE TABLE t (id INT)" {
		t.Errorf("an unguarded step was parsed as guarded: %q %v", stmt, guarded)
	}
}

func TestMySQLGuardExecutionForNullability(t *testing.T) {
	if !shouldRunMySQLGuard("not_null", true) {
		t.Fatal("NOT NULL column skipped the nullable conversion")
	}
	if shouldRunMySQLGuard("not_null", false) {
		t.Fatal("already nullable column reran the nullable conversion")
	}
	if !shouldRunMySQLGuard("has_constraint", true) || shouldRunMySQLGuard("has_constraint", false) {
		t.Fatal("has_constraint guard must run a DROP only while the constraint exists")
	}
	if shouldRunMySQLGuard("column", true) || !shouldRunMySQLGuard("column", false) {
		t.Fatal("column guard did not skip existing columns and run missing columns")
	}
}

// Every MySQL migration step is either plain DDL/DML or a guard; a guard with a
// missing part would silently run nothing. A require_empty guard is a refusal
// with no statement of its own.
func TestMySQLMigrationStepsAreWellFormed(t *testing.T) {
	for _, m := range mysqlMigrations {
		for i, step := range m.steps {
			if strings.HasPrefix(step, mysqlGuardSep) {
				kind, table, object, stmt, ok := parseMySQLGuard(step)
				if !ok || kind == "" || table == "" || object == "" || (strings.TrimSpace(stmt) == "" && kind != "require_empty") {
					t.Errorf("%s step %d: malformed guard %q", m.id, i+1, step)
				}
			}
		}
	}
}
