package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261117-outbox-skipped settles the notification backlog the forwarder was
// never started against (hk 4986): a row queued before the forwarder's cutoff
// is marked skipped instead of posted, so the operator does not receive weeks of
// stale notices on the first start. sent_at and hub_row_id keep their one
// meaning — the hub accepted this event — and a skipped row never gains them.
//
// Expand-only: two nullable columns the previous release never writes, CHECKs
// no existing row can violate, and a partial index. The ADD COLUMN and the
// constraint swaps take a brief ACCESS EXCLUSIVE lock on notification_outbox
// and the index build a SHARE lock for one scan; the table holds a few hundred
// rows. NOT VALID avoids scanning old rows.
func init() {
	registerMigration(migration{ID: "20261117-outbox-skipped", Apply: applyOutboxSkippedMigration})
}

func applyOutboxSkippedMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS skipped_at TIMESTAMPTZ`,
		`ALTER TABLE notification_outbox ADD COLUMN IF NOT EXISTS skip_reason TEXT`,
		`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_skip_reason`,
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_skip_reason CHECK(skipped_at IS NULL OR skip_reason IS NOT NULL) NOT VALID`,
		`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_skipped_not_sent`,
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_skipped_not_sent CHECK(skipped_at IS NULL OR sent_at IS NULL) NOT VALID`,
		`CREATE INDEX IF NOT EXISTS notification_outbox_unsent_live ON notification_outbox(id) WHERE sent_at IS NULL AND skipped_at IS NULL`,
	})
}
