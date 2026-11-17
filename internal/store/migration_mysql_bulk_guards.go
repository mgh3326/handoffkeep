package store

// Install replacement quota guards before dropping the old ones. MySQL commits
// DDL separately, so interruption can temporarily leave both guards, but it
// cannot leave the table unguarded. DDL takes metadata locks on the table.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261117-mysql-bulk-guards", after: []string{"20261117-quota-bench-baseline"}, steps: []string{
		`CREATE TRIGGER IF NOT EXISTS quota_history_bulk_update BEFORE UPDATE ON quota_observations FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load, 0) <> 1 AND COALESCE(@hk_quota_prune, 0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'quota observations are append-only'; END IF; END`,
		`CREATE TRIGGER IF NOT EXISTS quota_history_bulk_delete BEFORE DELETE ON quota_observations FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load, 0) <> 1 AND COALESCE(@hk_quota_prune, 0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'quota observations are append-only'; END IF; END`,
		`DROP TRIGGER IF EXISTS quota_history_immutable_update`,
		`DROP TRIGGER IF EXISTS quota_history_immutable_delete`,
	}})
}
