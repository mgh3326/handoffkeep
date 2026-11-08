package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261108-youtrack-attempts adds the per-key attempt state the YouTrack sync
// queues share: one row per queued item (a task id or a document key) that
// last failed or was skipped. A row decides when the key is next due
// (next_attempt_at, on the database clock), so a failing or unmappable item
// moves behind the rest instead of holding the head of a LIMIT batch. It is a new table with no
// foreign key, so it locks no existing table and the previous release does not
// read it; the migration writes no row.
func init() {
	registerMigration(migration{ID: "20261108-youtrack-attempts", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE IF NOT EXISTS youtrack_attempts (
				queue TEXT NOT NULL CHECK(queue IN ('task','doc')),
				item_key TEXT NOT NULL,
				next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
				attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
				last_error TEXT NOT NULL DEFAULT '',
				skip_reason TEXT NOT NULL DEFAULT '',
				updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
				PRIMARY KEY(queue,item_key))`,
			`CREATE INDEX IF NOT EXISTS youtrack_attempts_due ON youtrack_attempts(queue,next_attempt_at,item_key)`,
		})
	}})
}
