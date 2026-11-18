package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// taskWaitsMigrationID records the hand-wait table (hk 5128, track B-4). The
// table is new, so nothing the previous release reads or writes changes shape.
const taskWaitsMigrationID = "20261118-task-waits"

func init() {
	registerMigration(migration{ID: taskWaitsMigrationID, Apply: applyTaskWaitsMigration})
}

// applyTaskWaitsMigration creates task_waits. CREATE TABLE ... REFERENCES tasks
// takes SHARE ROW EXCLUSIVE on tasks for the instant the foreign key is checked,
// the same brief task-writer block the spawn record migration takes; a bounded
// lock wait stops a long reader from stalling the task API behind it. The
// partial unique index is built on the new, empty table, so it takes no lock on
// a table the previous release writes.
func applyTaskWaitsMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, taskWaitsMigration); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}

var taskWaitsMigration = []string{
	// A hand wait: the task waits on whom (on_kind) for what (reason), since
	// when. source is derived:<rule> or session:<author>. A cleared wait keeps
	// its row with cleared_at, cleared_by and clear_note.
	`CREATE TABLE IF NOT EXISTS task_waits (
		id BIGSERIAL PRIMARY KEY,
		task_id BIGINT NOT NULL REFERENCES tasks(id),
		on_kind TEXT NOT NULL CHECK(on_kind IN ('operator','lane','task','pr','ci','external')),
		on_ref TEXT NOT NULL DEFAULT '' CHECK(octet_length(on_ref) <= 256),
		reason TEXT NOT NULL CHECK(reason <> ''),
		since TIMESTAMPTZ NOT NULL,
		source TEXT NOT NULL CHECK(source <> ''),
		cleared_at TIMESTAMPTZ,
		cleared_by TEXT NOT NULL DEFAULT '',
		clear_note TEXT NOT NULL DEFAULT '',
		CHECK((on_kind = 'operator') = (on_ref = '')))`,
	// One open wait per task, kind and ref. A cleared row is outside the index,
	// so a wait can be set again after it ends.
	`CREATE UNIQUE INDEX IF NOT EXISTS task_waits_open ON task_waits(task_id, on_kind, on_ref) WHERE cleared_at IS NULL`,
}
