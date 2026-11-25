package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// dotsNoteKindMigrationID widens notification_outbox_kind_check to admit
// 'dots_note' (hk 6307): the design lane's note of a dots comment or intake.
// The MySQL twin shares this ID.
const dotsNoteKindMigrationID = "20261125-dots-note-outbox-kind"

func init() {
	registerMigration(migration{
		ID:    dotsNoteKindMigrationID,
		After: []string{laneCheckMigrationID},
		Apply: applyDotsNoteKindMigration,
	})
}

// applyDotsNoteKindMigration runs after 20261118-lane-check: the new CHECK
// lists that migration's kinds plus dots_note, so it must follow that one.
//
// The constraint swap takes ACCESS EXCLUSIVE on notification_outbox for the
// drop and the add; NOT VALID skips the scan of old rows. A bounded lock wait
// keeps a long writer from stalling the previous release's outbox writes
// behind the pending swap, as the lane-check migration does. The previous
// release never writes 'dots_note', and the wider list admits every kind it
// does write, so it keeps serving on the widened CHECK.
func applyDotsNoteKindMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, []string{
		`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_kind_check`,
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result','deploy_lag','task_order','task_drift','lane_check','dots_note')) NOT VALID`,
	}); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}
