package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261112-yt-write-op-alert-batch adds yt_write_op_alerts, the per-op marker
// the batched alert worker (EnqueueYTWriteOpAlerts) reads, so one op is
// announced at most once per class. The worker now writes one lane notice per
// 10-minute window and class instead of one per op; the markers keep an op
// from reappearing in a later window.
//
// The backfill copies the markers of the per-op notices the 20261110 worker
// wrote (event ids yt-op-<op id>-pending and yt-op-<op id>-rejected), so an op
// that was already announced is not announced again after the upgrade.
//
// Expand-only: one new table with a primary key, written only by the alert
// worker. It has no foreign key to yt_write_ops, because REFERENCES takes
// SHARE ROW EXCLUSIVE on yt_write_ops; the alert pass reads each op under a row
// lock in the same transaction that writes its marker, so none is needed.
// Locks: ACCESS EXCLUSIVE on the new table only, which is empty when created,
// and ACCESS SHARE on notification_outbox for the backfill read.
func init() {
	registerMigration(migration{ID: "20261112-yt-write-op-alert-batch", Apply: applyYTWriteOpAlertBatchMigration})
}

func applyYTWriteOpAlertBatchMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`CREATE TABLE IF NOT EXISTS yt_write_op_alerts (
			op_id BIGINT NOT NULL,
			alert_class TEXT NOT NULL CHECK(alert_class IN ('stuck','rejected')),
			created_at TIMESTAMPTZ NOT NULL,
			PRIMARY KEY (op_id, alert_class))`,
		`INSERT INTO yt_write_op_alerts(op_id, alert_class, created_at)
			SELECT substring(event_id FROM '^yt-op-([0-9]+)-')::bigint,
				CASE WHEN event_id LIKE '%-pending' THEN 'stuck' ELSE 'rejected' END,
				created_at
			FROM notification_outbox
			WHERE event_id ~ '^yt-op-[0-9]+-(pending|rejected)$'
			ON CONFLICT DO NOTHING`,
	})
}
