package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261125-outbox-unsent-poll serves the hub forwarder's two per-tick reads of
// notification_outbox (hk 6398; digests in findings/2026-11-25/hk5584-table-io):
// SkipNotificationsBefore settles rows with `created_at < ? AND sent_at IS NULL
// AND skipped_at IS NULL`, and ListUnsentNotifications polls `sent_at IS NULL
// AND skipped_at IS NULL AND created_at >= ? ORDER BY id LIMIT ?`. The only
// secondary index leads with id (notification_outbox_unsent_live), so on MySQL
// both statements walk the whole table (644k row reads in 30 min for ~890
// rows); the partial PostgreSQL index cannot range on created_at either.
//
// notification_outbox_unsent_poll (sent_at, skipped_at, created_at, id) keeps
// the two NULL equalities as the leading equality prefix, ranges on
// created_at, and carries id so the poll's ORDER BY id LIMIT reads in key
// order. Both statements become one range of the rows still owed.
//
// Expand-only: one new secondary index, no column, table or constraint change.
// The previous release's reads and writes are unchanged; its plans can only
// gain a key, and outbox writes maintain one more key. Lock: CREATE INDEX
// takes SHARE on notification_outbox for one build scan; the table holds a
// few hundred rows (under 1 MB), so the scan is short.
func init() {
	registerMigration(migration{ID: "20261125-outbox-unsent-poll", After: []string{"20261117-outbox-skipped"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE INDEX IF NOT EXISTS notification_outbox_unsent_poll ON notification_outbox(sent_at, skipped_at, created_at, id)`,
		})
	}})
}
