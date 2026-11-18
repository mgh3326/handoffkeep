package store

// tasks.close_rule on MySQL (hk 5112 track B-2), the twin of
// 20261118-task-close-rule. It runs after the tasks area, which creates the
// table. Both steps are catalog-guarded, so a restarted run skips what is there.
//
// Locks: ALTER TABLE tasks takes a metadata lock on tasks while it runs. The
// column is NOT NULL with a default, so the previous release, which never names
// it, keeps inserting and updating tasks unchanged (expand-only).
func init() {
	registerMySQLMigration(mysqlMigration{
		id:    "20261118-task-close-rule",
		after: []string{"20261118-mysql-tasks"},
		steps: []string{
			mysqlIfNoColumn("tasks", "close_rule", `ALTER TABLE tasks ADD COLUMN close_rule VARCHAR(16) NOT NULL DEFAULT 'auto'`),
			mysqlIfNoConstraint("tasks", "tasks_close_rule_check", `ALTER TABLE tasks ADD CONSTRAINT tasks_close_rule_check CHECK(close_rule IN ('auto','explicit'))`),
		},
	})
}
