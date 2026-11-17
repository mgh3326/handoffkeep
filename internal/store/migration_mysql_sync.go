package store

// These tables support the MySQL commit-time allocator. The DDL only locks
// the new tables; allocator transactions lock sync_state at commit.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261117-mysql-sync", steps: []string{
		`CREATE TABLE IF NOT EXISTS sync_state (singleton BOOLEAN NOT NULL PRIMARY KEY DEFAULT TRUE CHECK(singleton=1), max_sync_id BIGINT NOT NULL DEFAULT 0, pruned_through BIGINT NOT NULL DEFAULT 0)`,
		`INSERT INTO sync_state(singleton) VALUES(1) ON DUPLICATE KEY UPDATE singleton=singleton`,
		`CREATE TABLE IF NOT EXISTS sync_log (entry_id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, txn BINARY(8) NOT NULL, sync_id BIGINT NULL UNIQUE, model VARCHAR(32) NOT NULL CHECK(model IN ('document','task','task_comment','decision')), model_id TEXT NOT NULL, action CHAR(1) NOT NULL CHECK(action IN ('I','U','D')), at DATETIME(6) NOT NULL, KEY sync_log_pending(txn,sync_id,entry_id), KEY sync_log_at(at,sync_id)) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
	}})
}
