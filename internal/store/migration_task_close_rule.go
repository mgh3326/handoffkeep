package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// taskCloseRuleMigrationID adds tasks.close_rule (hk 5112 track B-2). The column
// is expand-only: the previous release inserts and updates tasks without naming
// it, and the default keeps every existing and newly inserted row on 'auto'.
const taskCloseRuleMigrationID = "20261118-task-close-rule"

func init() {
	registerMigration(migration{ID: taskCloseRuleMigrationID, Apply: applyTaskCloseRuleMigration})
}

// applyTaskCloseRuleMigration adds the column and its CHECK. ADD COLUMN with a
// constant default is metadata-only, but both ALTERs still take ACCESS EXCLUSIVE
// on tasks for the moment of the change, and the CHECK scans the table once under
// it. A bounded lock wait keeps a long reader from stalling the task API behind
// the pending lock, as the closure and research migrations do.
func applyTaskCloseRuleMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, taskCloseRuleMigration); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}

var taskCloseRuleMigration = []string{
	`ALTER TABLE tasks ADD COLUMN close_rule TEXT NOT NULL DEFAULT 'auto', ADD CONSTRAINT tasks_close_rule_check CHECK(close_rule IN ('auto','explicit'))`,
}
