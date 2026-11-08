package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// A job records why panewire chose its model and host (hk 3516): one nullable
// jsonb column holding panewire's selection record. ADD COLUMN without a
// default is catalog-only: it takes the jobs table's ACCESS EXCLUSIVE lock for
// the instant of the catalog update and rewrites no row. NULL means no
// selection was recorded.
func init() {
	registerMigration(migration{ID: "20261108-job-selection", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS selection JSONB`,
		})
	}})
}
