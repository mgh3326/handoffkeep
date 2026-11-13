package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const taskResearchMigrationID = "20261113-task-research"

func init() {
	registerMigration(migration{ID: taskResearchMigrationID, Apply: applyTaskResearchMigration})
}

// taskResearchMigration admits the research task kind while retaining every
// kind already accepted by the previous release. Replacing the CHECK takes
// ACCESS EXCLUSIVE on tasks; bound the wait so migration cannot queue writers
// behind a long-running task query.
func applyTaskResearchMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, taskResearchMigration); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}

var taskResearchMigration = []string{
	`ALTER TABLE tasks DROP CONSTRAINT tasks_kind_check, ADD CONSTRAINT tasks_kind_check CHECK(kind IN ('implement','verify','research','fix','decide','ops') OR (kind='' AND state IN ('triage','dropped','duplicate'))) NOT VALID`,
}
