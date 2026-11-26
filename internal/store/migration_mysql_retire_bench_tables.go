package store

// MySQL half of 20261127-retire-bench-tables (hk 6390; PostgreSQL is
// migration_retire_bench_tables.go). Both tables are refused while they hold a
// row, before any DROP, so a refusal leaves both in place. MySQL DDL commits
// per statement, so a run that fails after the checks has dropped nothing; a
// rerun repeats the checks and the drops. No live writer holds either table
// (release 1, hk 6389, removed them). A fresh install never created them:
// their guards pass and DROP IF EXISTS is a no-op. The step takes a metadata
// lock on each table for its DROP; the whole run sits under the migrate lock.
//
// Window: MySQL cannot hold a table lock across these separate statements
// (DDL commits, and LOCK TABLES forbids the DROP), so a write that commits
// between a require_empty check and its DROP is not refused. The migration
// therefore assumes no writer is running against these tables during startup,
// which holds because no release writes them. A writer outside the app must be
// stopped before the migration runs.
var mysqlRetireBenchTables = mysqlMigration{id: RetireBenchTablesMigration, steps: []string{
	mysqlRequireEmpty("bench_scores"),
	mysqlRequireEmpty("bench_area_exceptions"),
	`DROP TABLE IF EXISTS bench_scores`,
	`DROP TABLE IF EXISTS bench_area_exceptions`,
}}

func init() { registerMySQLMigration(mysqlRetireBenchTables) }
