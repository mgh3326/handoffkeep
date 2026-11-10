package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261110-yt-write-op-alert indexes unresolved/rejected ops for the alert
// worker and admits its kind to the existing outbox. CREATE INDEX takes a
// SHARE lock on yt_write_ops; the constraint swap takes a brief ACCESS
// EXCLUSIVE lock on notification_outbox. NOT VALID avoids scanning old rows.
func init() {
	registerMigration(migration{ID: "20261110-yt-write-op-alert", After: []string{"20261109-yt-write-ops"}, Apply: applyYTWriteOpAlertMigration})
}

func applyYTWriteOpAlertMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`CREATE INDEX IF NOT EXISTS yt_write_ops_alert_scan ON yt_write_ops(created_at,id) WHERE state IN ('prepared','dispatched','remote_unknown','rejected')`,
		`ALTER TABLE notification_outbox DROP CONSTRAINT notification_outbox_kind_check`,
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_followup','yt_write_op')) NOT VALID`,
	})
}
