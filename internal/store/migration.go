package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Schema versions are recorded in schema_version and claimed in one place
// each; the next free number is one past the highest below.
//
//	1-7, 9-19, 21, 22  Store.migrate (store.go); 8 was never used
//	20                 migrateWrk (wrk_migration.go), owner-only via MigrateWrk
//	23, 24             migrateQuota (quota.go)
//
// A new lock-taking or data-changing step goes through migrateOnce and ends
// its statements with the INSERT INTO schema_version that records it.

// SchemaVersion is the highest version a fully migrated database records.
// Bump it with every new version; tests assert max(version) against it.
const SchemaVersion = 24

// migrationLockID serializes migrations across processes (one
// pg_advisory_xact_lock per migrating transaction).
const migrationLockID = 824180045

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
