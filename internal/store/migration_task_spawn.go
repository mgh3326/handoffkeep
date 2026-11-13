package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// taskSpawnMigrationID records the spawn request table (hk 4377). Both tables
// are new, so nothing the previous release reads or writes changes shape.
const taskSpawnMigrationID = "20261113-task-spawn-record"

func init() {
	registerMigration(migration{ID: taskSpawnMigrationID, Apply: applyTaskSpawnMigration})
}

// applyTaskSpawnMigration creates task_spawn_records and task_spawn_notices.
// CREATE TABLE ... REFERENCES tasks takes SHARE ROW EXCLUSIVE on tasks for the
// instant the foreign key is checked, which blocks task writers briefly. A
// bounded lock wait keeps a long reader from stalling the task API behind it,
// the same way the task closure migration does.
func applyTaskSpawnMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, taskSpawnMigration); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}

var taskSpawnMigration = []string{
	// One row per task: the latest spawn request by updated_at wins. start_task_id
	// and start_until are both set or both NULL (a start condition).
	`CREATE TABLE IF NOT EXISTS task_spawn_records (
		task_id BIGINT PRIMARY KEY REFERENCES tasks(id),
		request_id TEXT NOT NULL,
		job TEXT NOT NULL,
		role TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL CHECK(state IN ('waiting','pending','parked','bound','launching','reconciling','landed','refused','given_up','cancelled')),
		reason TEXT NOT NULL DEFAULT '',
		attempt INTEGER NOT NULL CHECK(attempt >= 1),
		host TEXT NOT NULL DEFAULT '',
		next_check_at TIMESTAMPTZ,
		expires_at TIMESTAMPTZ,
		updated_at TIMESTAMPTZ NOT NULL,
		start_task_id BIGINT,
		start_until TEXT CHECK(start_until IN ('merged','deployed')),
		CHECK((start_task_id IS NULL) = (start_until IS NULL)))`,
	// One row per comment hk wrote for a terminal spawn state, keyed by
	// task, request, attempt and state, so a replay writes no second comment.
	`CREATE TABLE IF NOT EXISTS task_spawn_notices (
		notice_key TEXT PRIMARY KEY,
		task_id BIGINT NOT NULL REFERENCES tasks(id),
		created_at TIMESTAMPTZ NOT NULL)`,
}
