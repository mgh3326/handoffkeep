package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const youtrackMigrationID = "20261107-youtrack-sync"

func init() {
	registerMigration(migration{ID: youtrackMigrationID, Apply: applyYouTrackMigration})
}

// youtrackMigration adds the sync bookkeeping: the task ↔ issue link, the
// activities cursor, the comment ↔ comment link, the YouTrack-owned priority
// and lane cache, and the processed-activity ledger (with the refusal revert
// that still has to be written back to YouTrack).
//
// Every table is new, so no existing row is rewritten. The references to
// tasks take SHARE ROW EXCLUSIVE on that table while the constraints are
// created, which blocks its writers for the duration; the bounded lock wait
// keeps a long reader from stalling the task API behind the pending lock.
func applyYouTrackMigration(ctx context.Context, tx pgx.Tx) error {
	var priorLockTimeout string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout')`).Scan(&priorLockTimeout); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		return err
	}
	if err := execAll(ctx, tx, youtrackMigration); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, priorLockTimeout)
	return err
}

var youtrackMigration = []string{
	`CREATE TABLE youtrack_links (
		task_id BIGINT PRIMARY KEY REFERENCES tasks(id),
		issue_id TEXT NOT NULL UNIQUE,
		readable_id TEXT NOT NULL UNIQUE,
		pushed_task_updated_at TIMESTAMPTZ,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL)`,
	`CREATE TABLE youtrack_cursor (
		singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton),
		cursor TEXT NOT NULL DEFAULT '',
		last_activity_id TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	`INSERT INTO youtrack_cursor(singleton) VALUES (true) ON CONFLICT DO NOTHING`,
	// hk_comment_id has no foreign key: task_comments is optional in older
	// schemas (search must still work when it is absent), and a reference
	// would make that table impossible to drop. The link is written only by
	// the sync and is idempotent by its primary key.
	`CREATE TABLE youtrack_comment_links (
		hk_comment_id BIGINT PRIMARY KEY,
		task_id BIGINT NOT NULL REFERENCES tasks(id),
		youtrack_comment_id TEXT NOT NULL UNIQUE,
		created_at TIMESTAMPTZ NOT NULL)`,
	`CREATE INDEX youtrack_comment_links_task ON youtrack_comment_links(task_id)`,
	`CREATE TABLE youtrack_field_cache (
		task_id BIGINT PRIMARY KEY REFERENCES tasks(id),
		priority INTEGER NOT NULL,
		lane TEXT NOT NULL,
		fetched_at TIMESTAMPTZ NOT NULL,
		stale BOOLEAN NOT NULL DEFAULT false)`,
	`CREATE TABLE youtrack_processed_activities (
		activity_id TEXT PRIMARY KEY,
		outcome TEXT NOT NULL DEFAULT '',
		task_id BIGINT,
		issue_id TEXT NOT NULL DEFAULT '',
		revert_field TEXT NOT NULL DEFAULT '',
		revert_value TEXT NOT NULL DEFAULT '',
		refusal TEXT NOT NULL DEFAULT '',
		revert_done BOOLEAN NOT NULL DEFAULT true,
		processed_at TIMESTAMPTZ NOT NULL)`,
	`CREATE INDEX youtrack_processed_activities_pending ON youtrack_processed_activities(processed_at) WHERE NOT revert_done`,
}
