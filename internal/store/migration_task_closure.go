package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const taskClosureMigrationID = "20261106-task-closure"

func init() {
	registerMigration(migration{ID: taskClosureMigrationID, Apply: applyTaskClosureMigration})
}

// taskClosureMigration adds the completed state and closure shape check. ALTER TABLE
// takes ACCESS EXCLUSIVE on tasks until commit; both validated CHECKs scan tasks
// once under it. Existing rows pass because closure is absent and the state
// list is a superset. A bounded lock wait prevents a long reader from
// stalling the task API behind the pending exclusive lock.
func applyTaskClosureMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, taskClosureMigration); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}

var taskClosureMigration = []string{
	`ALTER TABLE tasks DROP CONSTRAINT tasks_state_check, ADD CONSTRAINT tasks_state_check CHECK(state IN ('triage','backlog','unstarted','researching','developing','reviewing','merged','deployed','dropped','duplicate','icebox','completed'))`,
	`ALTER TABLE tasks ADD CONSTRAINT tasks_closure_check CHECK(refs->'closure' IS NULL OR (refs->'closure'->>'type' IN ('resolved','cancelled','obsolete','superseded') AND COALESCE(refs->'closure'->>'evidence','') <> ''))`,
}
