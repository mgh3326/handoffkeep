package store

// Version 33 adds project and task metadata without rewriting existing rows.
// ALTER TABLE takes ACCESS EXCLUSIVE locks on task_projects, tasks and
// task_events; the foreign keys also lock their referenced tables.
var modelV33 = []string{
	`ALTER TABLE task_projects ADD COLUMN status TEXT NOT NULL DEFAULT '', ADD COLUMN start_at TIMESTAMPTZ, ADD COLUMN target_at TIMESTAMPTZ`,
	`CREATE TABLE project_milestones (id BIGSERIAL PRIMARY KEY, project TEXT NOT NULL REFERENCES task_projects(name), name TEXT NOT NULL, description TEXT NOT NULL DEFAULT '', target_at TIMESTAMPTZ, status TEXT NOT NULL DEFAULT '', sort_order INTEGER NOT NULL DEFAULT 0)`,
	`CREATE TABLE labels (id BIGSERIAL PRIMARY KEY, name TEXT NOT NULL UNIQUE, color TEXT NOT NULL DEFAULT '', parent_id BIGINT REFERENCES labels(id), is_group BOOLEAN NOT NULL DEFAULT false, retired_at TIMESTAMPTZ, edit_level TEXT NOT NULL DEFAULT 'anyone' CHECK (edit_level IN ('anyone','desk','operator')))`,
	`ALTER TABLE tasks ADD COLUMN milestone_id BIGINT REFERENCES project_milestones(id), ADD COLUMN due_at TIMESTAMPTZ, ADD COLUMN sla_started_at TIMESTAMPTZ, ADD COLUMN sla_medium_risk_at TIMESTAMPTZ, ADD COLUMN sla_high_risk_at TIMESTAMPTZ, ADD COLUMN sla_breaches_at TIMESTAMPTZ`,
	`CREATE TABLE task_labels (task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, label_id BIGINT NOT NULL REFERENCES labels(id), PRIMARY KEY(task_id,label_id))`,
	`CREATE TABLE task_subscribers (task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE, subscriber TEXT NOT NULL, source TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL, archived_at TIMESTAMPTZ, PRIMARY KEY(task_id,subscriber))`,
	`ALTER TABLE task_events DROP CONSTRAINT task_events_kind_check, ADD CONSTRAINT task_events_kind_check CHECK(kind IN ('transition','relane','decision','project','area','state_migration','awaiting_decision','parent','relation','auto_close','metadata')) NOT VALID`,
	`INSERT INTO schema_version(version) VALUES (33)`,
}
