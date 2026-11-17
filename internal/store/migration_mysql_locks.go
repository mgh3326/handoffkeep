package store

// hk_locks provides transaction-scoped mutual exclusion. CREATE TABLE locks
// only the new table; each holder later locks its own row through commit.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261117-mysql-locks", steps: []string{
		`CREATE TABLE IF NOT EXISTS hk_locks (lock_key BINARY(32) NOT NULL PRIMARY KEY)`,
		`INSERT INTO hk_locks(lock_key) VALUES (UNHEX(SHA2(CONCAT(UNHEX('0000000000000002'), 'id', '8241873997'), 256))) ON DUPLICATE KEY UPDATE lock_key=lock_key`,
	}})
}
