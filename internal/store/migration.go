package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Integer schema versions 1-53 are recorded in schema_version and claimed in
// one place each. The list is frozen: 53 is the last integer version.
// Every later migration is identified by a unique string ID, declared in the
// registry (migration_ids.go) and recorded in schema_migrations.
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
//	42                         changesV42 (migration_changes.go), called from Store.migrate
//	43                         decisionResolutionsV43 (migration_decision_resolutions.go), called from Store.migrate
//	44                         lastActivityV44 (migration_last_activity.go), called from Store.migrate
//	45                         jobContextV45 (migration_job_context.go), called from Store.migrate
//	46                         decisionDenyV46 (migration_decision_deny.go), called from Store.migrate
//	47                         decisionSessionV47 (migration_decision_session.go), called from Store.migrate
//	48                         decisionDeliveriesV48 (migration_decision_deliveries.go), called from Store.migrate
//	49                         verificationsV49 (migration_verifications.go), called from Store.migrate
//	50                         dotsResultsV50 (migration_dots_results.go), called from Store.migrate
//	51                         changeStateV51 (migration_change_state.go), called from Store.migrate
//	52                         reviewHistoryV52 (migration_review_history.go), called from Store.migrate
//	53                         schemaMigrationsV53 (migration_ids.go), called from Store.migrate
//	20                         migrateWrk (wrk_migration.go), owner-only via MigrateWrk
//	23, 24                     migrateQuota (quota.go)
//
// Store.migrate runs the integer versions first, then the registered IDs in
// the order orderMigrations gives (sorted by ID, After respected).
//
// Open runs Store.migrate only when a version in migrateVersions or a
// registered ID is missing (migrateIfBehind, migrateCurrent): on a current
// schema it takes no table lock, so opening a store beside live writers
// cannot deadlock with them (lock order, lockorder.go). Every schema change
// therefore needs a new registered ID, even an additive IF NOT EXISTS
// statement: an unregistered one would never run on a database that is
// already current.

// SchemaVersion is the highest integer version a fully migrated database
// records. It is frozen at 53: registered IDs do not change it. Tests assert
// max(version) against it, and the sync cursor carries it.
const SchemaVersion = 53

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
// version in migrateVersions and every ID in ordered. It reads the current
// schema's schema_version first: under a test search_path a later schema's
// table must not count, and schema_migrations exists only once version 53 is
// recorded. Versions or IDs the database has beyond what this binary knows
// (a newer binary migrated it) are ignored, so a binary rollback still opens.
func migrateCurrent(ctx context.Context, q rowQuerier, ordered []migration) (bool, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT current_schema() IS NOT NULL AND to_regclass(format('%I.schema_version', current_schema())) IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	want := migrateVersions()
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version = ANY($1)`, want).Scan(&n); err != nil {
		return false, err
	}
	if n != len(want) {
		return false, nil
	}
	return registeredCurrent(ctx, q, ordered)
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
