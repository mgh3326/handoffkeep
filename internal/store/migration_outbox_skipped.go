package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261117-outbox-skipped settles the notification backlog the forwarder was
// never started against (hk 4986): a row queued before the forwarder's cutoff
// is marked skipped instead of posted, so the operator does not receive weeks of
// stale notices on the first start. sent_at and hub_row_id keep their one
// meaning — the hub accepted this event — and this release never sets them on a
// skipped row (MarkNotificationSent refuses one).
//
// No CHECK ties skipped_at to sent_at: the previous release's forwarder does
// not know skipped_at, and a receipt it stores for a row it posted must still
// succeed during blue-green overlap. The rollout turns forwarding off on the
// colour being replaced before this release settles the backlog; that step is
// in docs/image.md. The applied_after_sent CHECK is unchanged.
//
// Expand-only: two nullable columns the previous release never writes, one
// CHECK on rows only this release writes, and a partial index. The ADD COLUMN
// and the constraint swap take a brief ACCESS EXCLUSIVE lock on
// notification_outbox and the index build a SHARE lock for one scan; the table
// holds a few hundred rows. NOT VALID avoids scanning old rows.
func init() {
	registerMigration(migration{ID: "20261117-outbox-skipped", Apply: applyOutboxSkippedMigration})
}

func applyOutboxSkippedMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS skipped_at TIMESTAMPTZ`,
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS skip_reason TEXT`,
		`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_skip_reason`,
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_skip_reason CHECK(skipped_at IS NULL OR skip_reason IS NOT NULL) NOT VALID`,
		`CREATE INDEX IF NOT EXISTS notification_outbox_unsent_live ON notification_outbox(id) WHERE sent_at IS NULL AND skipped_at IS NULL`,
	})
}
