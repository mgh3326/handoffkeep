package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Schema versions are recorded in schema_version and claimed in one place
// each; the next free number is one past the highest below.
//
//	1-7, 9-19, 21, 22, 25-31  Store.migrate (store.go); 8 was never used
//	32                         migrateStatesV32 (migration_states.go), called from Store.migrate
//	33                         modelV33 (migration_model.go), called from Store.migrate
//	34                         eventsV34 (migration_events.go), called from Store.migrate
//	35                         decisionsV35 (migration_decisions.go), called from Store.migrate
//	36                         ledgerV36 (migration_ledger.go), called from Store.migrate
//	37                         intakeV37 (migration_intake.go), called from Store.migrate
//	38                         decisionNotifyV38 (migration_decision_notify.go), called from Store.migrate
//	39                         jobUsageV39 (migration_job_usage.go), called from Store.migrate
//	40                         benchOutcomesV40 (migration_bench_outcomes.go), called from Store.migrate
//	41                         originsV41 (migration_origins.go), called from Store.migrate
//	42                         taskClosureV42 (migration_task_closure.go), called from Store.migrate
//	20                         migrateWrk (wrk_migration.go), owner-only via MigrateWrk
//	23, 24                     migrateQuota (quota.go)
//
// A new lock-taking or data-changing step goes through migrateOnce and ends
// its statements with the INSERT INTO schema_version that records it.
//
// Open runs Store.migrate only when a version in migrateVersions is missing
// (migrateIfBehind, migrateCurrent): on a current schema it takes no table
// lock, so opening a store beside live writers cannot deadlock with them
// (lock order, lockorder.go). Every schema change therefore needs a new
// version, even an additive IF NOT EXISTS statement: an unversioned one would
// never run on a database that is already current.

// SchemaVersion is the highest version a fully migrated database records.
// Bump it with every new version; tests assert max(version) against it.
const SchemaVersion = 42

// migrationLockID serializes migrations across processes (one
// pg_advisory_xact_lock per migrating transaction).
const migrationLockID = 824180045

// migrateVersions lists every version Store.migrate records: 1 through
// SchemaVersion except 8 (never used) and 20 (MigrateWrk, owner-only).
func migrateVersions() []int {
	out := make([]int, 0, SchemaVersion)
	for v := 1; v <= SchemaVersion; v++ {
		if v != 8 && v != 20 {
			out = append(out, v)
		}
	}
	return out
}

// migrateCurrent reports whether the current schema already records every
// version in migrateVersions. It reads the current schema's schema_version
// only: under a test search_path a later schema's table must not count.
func migrateCurrent(ctx context.Context, q rowQuerier) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT current_schema() IS NOT NULL AND to_regclass(format('%I.schema_version', current_schema())) IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	want := migrateVersions()
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version = ANY($1)`, want).Scan(&n); err != nil {
		return false, err
	}
	return n == len(want), nil
}

// migrateOnce runs apply only when schema_version has no row for version.
// apply must record the version itself, inside the same transaction.
func migrateOnce(ctx context.Context, tx pgx.Tx, version int, apply func() error) error {
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=$1)`, version).Scan(&applied); err != nil {
		return err
	}
	if applied {
		return nil
	}
	return apply()
}

// execAll runs statements in order, stopping at the first error.
func execAll(ctx context.Context, tx pgx.Tx, statements []string) error {
	for _, q := range statements {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
