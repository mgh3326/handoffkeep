package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// jobOwnerFenceMigrationID adds the job owner fence (hk 5276): the node and
// epoch that may write a job. Both columns default to "no owner", so the
// previous release, which never names them, keeps inserting and updating jobs
// unchanged (expand-only).
const jobOwnerFenceMigrationID = "20261119-job-owner-fence"

// 20261119-job-owner-fence adds jobs.owner_node (TEXT, empty = no owner) and
// jobs.owner_epoch (BIGINT, 0 = unfenced). A constant default is catalog-only
// on PostgreSQL 11+, so no row is rewritten. The ALTER still takes ACCESS
// EXCLUSIVE on jobs for the instant of the catalog change, so the statement
// runs under a bounded lock wait, as the task close-rule migration does.
func init() {
	registerMigration(migration{ID: jobOwnerFenceMigrationID, Apply: applyJobOwnerFenceMigration})
}

func applyJobOwnerFenceMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, jobOwnerFenceStatements); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}

var jobOwnerFenceStatements = []string{
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS owner_node TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE jobs ADD COLUMN IF NOT EXISTS owner_epoch BIGINT NOT NULL DEFAULT 0`,
}
