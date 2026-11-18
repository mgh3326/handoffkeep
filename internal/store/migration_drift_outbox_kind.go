package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const driftOutboxKindMigrationID = "20261118-drift-outbox-kind"

func init() {
	registerMigration(migration{
		ID:    driftOutboxKindMigrationID,
		After: []string{taskOrdersMigrationID},
		Apply: applyDriftOutboxKindMigration,
	})
}

// driftOutboxKindStatements admits 'task_drift' to notification_outbox
// (hk 5112 track B-3): the daily drift and hygiene delivery writes one row
// per lane per day plus one operator-desk summary. The statements are
// expand-only: a wider CHECK that admits the new kind and rejects nothing it
// accepted, so the previous release keeps writing its own kinds.
//
// The kind list is the union of the latest rewrite of the constraint
// (20261118-task-orders) plus task_drift. The constraint swap takes ACCESS
// EXCLUSIVE on notification_outbox for the drop and the add; NOT VALID skips
// the scan of old rows. A bounded lock wait keeps a long writer from stalling
// the previous release's outbox writes behind the pending swap, as the
// closure and research migrations do.
var driftOutboxKindStatements = []string{
	`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_kind_check`,
	`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result','deploy_lag','task_order','task_drift')) NOT VALID`,
}

func applyDriftOutboxKindMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, driftOutboxKindStatements); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}
