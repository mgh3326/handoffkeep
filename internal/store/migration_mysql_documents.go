package store

// Documents and translations are carried with their bodies. These new tables
// take only MySQL metadata locks while created; document triggers participate in
// the transaction's sync allocation and are bypassed by the copy tool.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261118-mysql-documents", after: []string{"20261118-mysql-tasks"}, steps: []string{
		"CREATE TABLE IF NOT EXISTS documents (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, `key` VARCHAR(512) NOT NULL UNIQUE, kind VARCHAR(32) NOT NULL, session VARCHAR(128) NOT NULL DEFAULT '', job VARCHAR(128) NOT NULL DEFAULT '', body LONGTEXT NOT NULL, sha256 VARCHAR(64) NOT NULL, created_by TEXT NOT NULL, created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL, task_id BIGINT, origin_class VARCHAR(16), KEY documents_task(task_id), CONSTRAINT documents_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id), CHECK(kind IN ('brief','report','answer','handoff','note','other','decision','research','design','policy','evidence')), CHECK(origin_class IN ('authored','job','import')))" + mysqlTableOptions,
		`CREATE TABLE IF NOT EXISTS translations (entity VARCHAR(32) NOT NULL, entity_id VARCHAR(128) NOT NULL, field VARCHAR(32) NOT NULL, lang VARCHAR(8) NOT NULL, source_sha256 VARCHAR(64) NOT NULL, text LONGTEXT NOT NULL, translator TEXT NOT NULL, updated_at DATETIME(6) NOT NULL, PRIMARY KEY(entity,entity_id,field,lang), CHECK(entity IN ('task','decision_request','task_comment')), CHECK(field IN ('title','body','question','options')), CHECK(lang='ko'))` + mysqlTableOptions,
		`CREATE TABLE IF NOT EXISTS mcp_write_audit (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, tool VARCHAR(64) NOT NULL, actor VARCHAR(320) NOT NULL, channel VARCHAR(32) NOT NULL, author VARCHAR(128) NOT NULL, task_id BIGINT, idempotency_key VARCHAR(128) NOT NULL DEFAULT '', result JSON, at DATETIME(6) NOT NULL, UNIQUE KEY mcp_write_audit_key ((CASE WHEN idempotency_key <> '' THEN tool ELSE NULL END), (CASE WHEN idempotency_key <> '' THEN actor ELSE NULL END), (CASE WHEN idempotency_key <> '' THEN idempotency_key ELSE NULL END)))` + mysqlTableOptions,
		mysqlSyncEmit("sync_documents_insert", "documents", "INSERT", "document", "NEW", "I"),
		mysqlSyncEmit("sync_documents_update", "documents", "UPDATE", "document", "NEW", "U"),
		mysqlSyncEmit("sync_documents_delete", "documents", "DELETE", "document", "OLD", "D"),
	}})
}
