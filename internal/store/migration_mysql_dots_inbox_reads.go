package store

// 20261118-dots-inbox-reads mirrors the PostgreSQL dots_inbox_reads table
// (hk 4924): one row per dots_inbox MCP call — the time, the authenticated
// principal, how many requests it listed and its result.
//
// Expand-only: one new table no previous release reads or writes. Locks:
// metadata lock on dots_inbox_reads for the CREATE only; no existing table is
// touched.
func init() {
	registerMySQLMigration(mysqlMigration{id: dotsInboxReadsMigrationID, steps: []string{
		`CREATE TABLE IF NOT EXISTS dots_inbox_reads (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, read_at DATETIME(6) NOT NULL, principal TEXT NOT NULL, listed INT NOT NULL, result TEXT NOT NULL, KEY dots_inbox_reads_read_at(read_at))` + mysqlTableOptions,
	}})
}
