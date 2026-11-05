package store

// The ADD COLUMN takes ACCESS EXCLUSIVE on tasks until the migration
// transaction commits. Both backfill UPDATEs and the SET NOT NULL scan run
// while every task reader and writer waits. task_events is read under ACCESS
// SHARE only. Each updated task writes one sync_log row through sync_tasks, so
// the search feed sees one U per task once. Neither UPDATE touches updated_at.
var lastActivityV41 = []string{
	`ALTER TABLE tasks ADD COLUMN IF NOT EXISTS last_activity_at TIMESTAMPTZ`,
	`UPDATE tasks t SET last_activity_at = GREATEST(t.created_at, a.at) FROM (SELECT task_id, max(at) AS at FROM task_events WHERE ` + activityEventSQL + ` GROUP BY task_id) a WHERE a.task_id = t.id`,
	`UPDATE tasks SET last_activity_at = created_at WHERE last_activity_at IS NULL`,
	`ALTER TABLE tasks ALTER COLUMN last_activity_at SET NOT NULL`,
	`INSERT INTO schema_version(version) VALUES (41)`,
}
