package store

// 20261118-doc-retention mirrors the PostgreSQL 20261118-doc-retention
// audit table for document retention (hk 5057). One row per deleted
// document: who deleted it, when, the key and the body's sha256. Dry runs
// write no row.
//
// Expand-only: one new table no previous release reads or writes. Locks:
// metadata lock on doc_prune_log for the CREATE only; no existing table is
// touched.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261118-doc-retention", after: []string{"20261118-mysql-documents"}, steps: []string{
		`CREATE TABLE IF NOT EXISTS doc_prune_log (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, document_key VARCHAR(512) NOT NULL, sha256 CHAR(64) NOT NULL, pruned_at DATETIME(6) NOT NULL, pruned_by LONGTEXT NOT NULL, KEY doc_prune_log_pruned_at(pruned_at))` + mysqlTableOptions,
	}})
}
