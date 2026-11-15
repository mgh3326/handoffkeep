package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261114-deploy-lag-outbox admits 'deploy_lag' to notification_outbox.
// It runs after every migration that rewrites the same constraint
// (20261113-outbox-receipts, 20261113-dots-blocked-kind): the CHECK list here
// is the union of all their kinds plus deploy_lag, so whichever runs last
// cannot drop another's kind.
// The constraint swap takes a brief ACCESS EXCLUSIVE lock on notification_outbox;
// NOT VALID avoids scanning old rows.
func init() {
	registerMigration(migration{
		ID:    "20261114-deploy-lag-outbox",
		After: []string{"20261110-yt-write-op-alert", "20261113-outbox-receipts", "20261113-dots-blocked-kind"},
		Apply: applyDeployLagOutboxMigration,
	})
}

func applyDeployLagOutboxMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_kind_check`,
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result','deploy_lag')) NOT VALID`,
	})
}
