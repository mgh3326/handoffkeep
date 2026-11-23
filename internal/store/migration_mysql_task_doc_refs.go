package store

// 20261124-mysql-task-doc-refs adds the task-to-document reference table
// (hk 6125), the MySQL twin of 20261124-task-doc-refs. No FOREIGN KEY, for the
// same reason as the PostgreSQL migration.
//
// Expand-only: one new table no previous release reads or writes. Locks:
// metadata lock on task_doc_refs for the CREATE only; no existing table is
// touched.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261124-mysql-task-doc-refs", steps: []string{
		`CREATE TABLE IF NOT EXISTS task_doc_refs (task_id BIGINT NOT NULL, doc_key VARCHAR(512) NOT NULL, created_at DATETIME(6) NOT NULL, created_by TEXT NOT NULL, PRIMARY KEY(task_id,doc_key))` + mysqlTableOptions,
	}})
}
