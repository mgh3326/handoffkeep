package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const laneCheckMigrationID = "20261118-lane-check"

func init() {
	registerMigration(migration{
		ID:    laneCheckMigrationID,
		After: []string{driftOutboxKindMigrationID},
		Apply: applyLaneCheckMigration,
	})
}

func applyLaneCheckMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, laneCheckStatements); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}

// lane_looks records that a design lane looked at its lane check (operator
// decision 418, hk 5201): the notice it was answering (or, for the row the
// check writes when a lane's findings go empty, its own instant-named id), the
// stable findings hash the look leaves behind (wait_cleared excluded, because
// the look advances that baseline), who looked and when. The check skips a lane whose
// last look carries the current stable hash, so only a change of state
// re-notices it, not the clock. The notification_outbox widening admits
// 'lane_check' and rejects nothing it accepted, so the previous release keeps
// writing its own kinds.
//
// The CREATE TABLE and its index lock only the new table. The constraint swap
// takes ACCESS EXCLUSIVE on notification_outbox for the drop and the add;
// NOT VALID skips the scan of old rows. The kind list is the union of the
// latest rewrite of the constraint (20261118-drift-outbox-kind) plus
// lane_check. A bounded lock wait keeps a long writer from stalling the
// previous release's outbox writes behind the pending swap, as the drift
// migration does.
var laneCheckStatements = []string{
	`CREATE TABLE lane_looks (
 id BIGSERIAL PRIMARY KEY,
 lane TEXT NOT NULL CHECK (lane <> ''),
 notice_event_id TEXT NOT NULL CHECK (notice_event_id <> ''),
 findings_hash TEXT NOT NULL CHECK (findings_hash ~ '^[0-9a-f]{8}$'),
 note TEXT NOT NULL DEFAULT '',
 actor TEXT NOT NULL CHECK (actor <> ''),
 created_at TIMESTAMPTZ NOT NULL,
 CONSTRAINT lane_looks_notice UNIQUE (lane, notice_event_id))`,
	`CREATE INDEX lane_looks_lane ON lane_looks (lane, id DESC)`,
	`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_kind_check`,
	`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result','deploy_lag','task_order','task_drift','lane_check')) NOT VALID`,
}
