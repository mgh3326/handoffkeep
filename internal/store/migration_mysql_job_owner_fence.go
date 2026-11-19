package store

// 20261119-job-owner-fence mirrors the PostgreSQL jobs.owner_node and
// jobs.owner_epoch columns (hk 5276). Both are NOT NULL with a default, so the
// previous release, which never names them, keeps writing jobs unchanged
// (expand-only). The ALTERs take a metadata lock on jobs while they run, as
// the other jobs expansion steps do.
func init() {
	registerMySQLMigration(mysqlMigration{id: jobOwnerFenceMigrationID, after: []string{"20261118-mysql-relay-jobs"}, steps: []string{
		mysqlIfNoColumn("jobs", "owner_node", `ALTER TABLE jobs ADD COLUMN owner_node LONGTEXT NOT NULL DEFAULT ('')`),
		mysqlIfNoColumn("jobs", "owner_epoch", `ALTER TABLE jobs ADD COLUMN owner_epoch BIGINT NOT NULL DEFAULT 0`),
	}})
}
