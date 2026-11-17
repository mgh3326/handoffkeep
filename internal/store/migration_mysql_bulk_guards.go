package store

// Replace the existing quota guards so the future copy tool can load rows.
// DROP and CREATE TRIGGER take metadata locks on quota_observations. MySQL
// commits DDL separately, so each step is safe to repeat after interruption.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261117-mysql-bulk-guards", after: []string{"20261117-quota-bench-baseline"}, steps: []string{
		`DROP TRIGGER IF EXISTS quota_history_immutable_update`,
		`CREATE TRIGGER IF NOT EXISTS quota_history_immutable_update BEFORE UPDATE ON quota_observations FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load, 0) <> 1 AND COALESCE(@hk_quota_prune, 0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'quota observations are append-only'; END IF; END`,
		`DROP TRIGGER IF EXISTS quota_history_immutable_delete`,
		`CREATE TRIGGER IF NOT EXISTS quota_history_immutable_delete BEFORE DELETE ON quota_observations FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load, 0) <> 1 AND COALESCE(@hk_quota_prune, 0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'quota observations are append-only'; END IF; END`,
	}})
}
