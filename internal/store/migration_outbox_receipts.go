package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-outbox-receipts gives notification_outbox its second receipt and
// admits the kinds of the remaining resolution paths (hk 3295). sent_at and
// hub_row_id stay the "received" receipt (the hub accepted the event);
// applied_at and applied_by are the "applied" receipt (the receiving lane
// acked it). applied_at needs sent_at: a lane cannot apply what the hub never
// accepted. Expand-only: two nullable columns the previous release never
// writes, a wider kind list, and a CHECK no existing row can violate. The
// ADD COLUMN and the constraint swap take a brief ACCESS EXCLUSIVE lock on
// notification_outbox; NOT VALID avoids scanning old rows.
func init() {
	registerMigration(migration{ID: "20261113-outbox-receipts", After: []string{"20261110-yt-write-op-alert"}, Apply: applyOutboxReceiptsMigration})
}

func applyOutboxReceiptsMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS applied_at TIMESTAMPTZ`,
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS applied_by TEXT`,
		`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_applied_after_sent`,
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_applied_after_sent CHECK(applied_at IS NULL OR sent_at IS NOT NULL) NOT VALID`,
		`ALTER TABLE notification_outbox DROP CONSTRAINT notification_outbox_kind_check`,
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result')) NOT VALID`,
	})
}
