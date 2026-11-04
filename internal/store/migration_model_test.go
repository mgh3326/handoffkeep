package store

import (
	"testing"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestModelV33Upgrade(t *testing.T) {
	ctx := t.Context()
	pool, _ := testdb.SchemaPool(t, "model_v33")
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Rewind only the additive migration to construct a v32 schema.
	for _, q := range []string{
		`DROP TABLE task_subscribers, task_labels`,
		`ALTER TABLE tasks DROP COLUMN milestone_id, DROP COLUMN due_at, DROP COLUMN sla_started_at, DROP COLUMN sla_medium_risk_at, DROP COLUMN sla_high_risk_at, DROP COLUMN sla_breaches_at`,
		`DROP TABLE labels, project_milestones`,
		`ALTER TABLE task_projects DROP COLUMN status, DROP COLUMN start_at, DROP COLUMN target_at`,
		`DELETE FROM schema_version WHERE version=33`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	id := seedTaskRow(t, pool, "migration-test", "old task", "backlog")
	if _, err := pool.Exec(ctx, `INSERT INTO task_events(task_id,"from","to","by",at) VALUES($1,'backlog','unstarted','test',now())`, id); err != nil {
		t.Fatal(err)
	}
	var beforeTask, beforeEvent []byte
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(t) FROM tasks t WHERE id=$1`, id).Scan(&beforeTask); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(e) FROM task_events e WHERE task_id=$1`, id).Scan(&beforeEvent); err != nil {
		t.Fatal(err)
	}
	if err := s.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for table, cols := range map[string][]string{
		"task_projects":      {"status", "start_at", "target_at"},
		"tasks":              {"milestone_id", "due_at", "sla_started_at", "sla_medium_risk_at", "sla_high_risk_at", "sla_breaches_at"},
		"project_milestones": {"project", "name", "description", "target_at", "status", "sort_order"},
		"labels":             {"name", "color", "parent_id", "is_group", "retired_at", "edit_level"},
		"task_labels":        {"task_id", "label_id"},
		"task_subscribers":   {"task_id", "subscriber", "source", "created_at", "archived_at"},
	} {
		for _, col := range cols {
			var exists bool
			if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name=$1 AND column_name=$2)`, table, col).Scan(&exists); err != nil || !exists {
				t.Fatalf("%s.%s exists=%t err=%v", table, col, exists, err)
			}
		}
	}
	var title, from, to string
	if err := pool.QueryRow(ctx, `SELECT t.title,e."from",e."to" FROM tasks t JOIN task_events e ON e.task_id=t.id WHERE t.id=$1`, id).Scan(&title, &from, &to); err != nil || title != "old task" || from != "backlog" || to != "unstarted" {
		t.Fatalf("historic task/event: %q %q %q err=%v", title, from, to, err)
	}
	var sameTask, sameEvent bool
	if err := pool.QueryRow(ctx, `SELECT (to_jsonb(t) - ARRAY['milestone_id','due_at','sla_started_at','sla_medium_risk_at','sla_high_risk_at','sla_breaches_at'])=$2::jsonb FROM tasks t WHERE id=$1`, id, string(beforeTask)).Scan(&sameTask); err != nil || !sameTask {
		t.Fatalf("task changed=%t err=%v", !sameTask, err)
	}
	if err := pool.QueryRow(ctx, `SELECT to_jsonb(e)=$2::jsonb FROM task_events e WHERE task_id=$1`, id, string(beforeEvent)).Scan(&sameEvent); err != nil || !sameEvent {
		t.Fatalf("event changed=%t err=%v", !sameEvent, err)
	}
}
