package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// RetireBenchTablesMigration is the registered ID of the contract step of the
// bench-table retirement (operator decision 501; hk 6389 is release 1, hk 6390
// this step). Release 1 removed every code reference to bench_scores and
// bench_area_exceptions and stopped creating them on new installs; existing
// databases kept the empty tables until this migration.
const RetireBenchTablesMigration = "20261127-retire-bench-tables"

// retiredBenchTables are dropped in this order. Each is refused while it holds
// a row, so a database that wrote to either table keeps both.
var retiredBenchTables = []string{"bench_scores", "bench_area_exceptions"}

func init() {
	registerMigration(migration{ID: RetireBenchTablesMigration, Apply: applyRetireBenchTables})
}

// applyRetireBenchTables refuses while either table holds a row, then drops
// both. It takes ACCESS SHARE to count each table and ACCESS EXCLUSIVE to drop
// it, all inside the migrating transaction under migrationLockID. No live
// writer holds either table: release 1 removed the writers, and the release
// that ran them has been superseded. The counts run before any DROP, so a
// refusal leaves both tables in place. A fresh install never created them, so
// the to_regclass check skips them and DROP IF EXISTS is a no-op.
func applyRetireBenchTables(ctx context.Context, tx pgx.Tx) error {
	for _, table := range retiredBenchTables {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		var rows int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&rows); err != nil {
			return err
		}
		if rows > 0 {
			return fmt.Errorf("%s: %s holds %d rows; it is dropped only when empty", RetireBenchTablesMigration, table, rows)
		}
	}
	return execAll(ctx, tx, []string{
		`DROP TABLE IF EXISTS bench_scores`,
		`DROP TABLE IF EXISTS bench_area_exceptions`,
	})
}
