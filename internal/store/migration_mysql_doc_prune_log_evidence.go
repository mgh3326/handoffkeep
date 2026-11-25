package store

// 20261125-doc-prune-log-evidence mirrors the PostgreSQL migration of the
// same id (hk 6289): doc_prune_log gains the deleted document's job and
// task_id, so the merge link still counts a job's task evidence after another
// leg deleted one of its documents. Rows logged before it keep NULL in both
// and are no evidence either way.
//
// job matches documents.job (VARCHAR(128)) and task_id documents.task_id
// (BIGINT), both NULL-able and without a foreign key. MySQL has no partial
// index, so doc_prune_log_evidence is on document_key alone: the merge link
// reads the four mwr- key ranges and filters task_id on the rows found.
//
// Expand-only: two NULL-able columns and one secondary index. The previous
// release names its doc_prune_log columns on INSERT, so its rows get NULL.
// Locks: ADD COLUMN of a NULL-able column without a default is an INSTANT
// change on InnoDB (a metadata lock at its start and end only); the index is
// built online, so reads and writes continue, with a short metadata lock at
// its start and end. Each step is guarded, so the migration is restartable.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261125-doc-prune-log-evidence", after: []string{"20261118-doc-retention"}, steps: []string{
		mysqlIfNoColumn("doc_prune_log", "job", `ALTER TABLE doc_prune_log ADD COLUMN job VARCHAR(128) NULL`),
		mysqlIfNoColumn("doc_prune_log", "task_id", `ALTER TABLE doc_prune_log ADD COLUMN task_id BIGINT NULL`),
		mysqlIfNoIndex("doc_prune_log", "doc_prune_log_evidence", `CREATE INDEX doc_prune_log_evidence ON doc_prune_log(document_key)`),
	}})
}
