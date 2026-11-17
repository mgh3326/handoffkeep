package store

// The task stub supplies only the FK target for isolated comment work; B2 must
// expand it before any task methods are used on MySQL. DDL takes metadata
// locks on these tables; comment writes take hk_locks through commit.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261117-mysql-comments", after: []string{"20261117-mysql-locks", "20261117-mysql-sync"}, steps: []string{
		`CREATE TABLE IF NOT EXISTS tasks (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS task_comments (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, task_id BIGINT NOT NULL, body LONGTEXT NOT NULL, author TEXT NOT NULL, created_at DATETIME(6) NOT NULL, source_ref VARCHAR(128) NOT NULL DEFAULT '', system_event_key VARCHAR(255), UNIQUE KEY task_comments_system_event_key(task_id,system_event_key), KEY task_comments_task_id(task_id,id), CONSTRAINT task_comments_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin`,
		`CREATE TRIGGER IF NOT EXISTS task_comments_guard_update BEFORE UPDATE ON task_comments FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='task_comments is append-only'; END IF; END`,
		`CREATE TRIGGER IF NOT EXISTS task_comments_guard_delete BEFORE DELETE ON task_comments FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='task_comments is append-only'; END IF; END`,
		`CREATE TRIGGER IF NOT EXISTS sync_comments_insert AFTER INSERT ON task_comments FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'task_comment',CAST(NEW.id AS CHAR),'I',UTC_TIMESTAMP(6)); END IF; END`,
		`CREATE TRIGGER IF NOT EXISTS sync_comments_update AFTER UPDATE ON task_comments FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'task_comment',CAST(NEW.id AS CHAR),'U',UTC_TIMESTAMP(6)); END IF; END`,
		`CREATE TRIGGER IF NOT EXISTS sync_comments_delete AFTER DELETE ON task_comments FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'task_comment',CAST(OLD.id AS CHAR),'D',UTC_TIMESTAMP(6)); END IF; END`,
	}})
}
