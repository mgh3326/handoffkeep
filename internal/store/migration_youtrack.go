package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const youtrackMigrationID = "20261107-youtrack-sync"

func init() {
	registerMigration(migration{ID: youtrackMigrationID, Apply: applyYouTrackMigration})
}

// applyYouTrackMigration adds the sync bookkeeping: the per-hk-project
// YouTrack project mapping, the outbox that carries hk's priority and lane
// writes, the activities cursor, the comment links, the served-value cache,
// and the processed-activity ledger (with the refusal revert that still has to
// be written back to YouTrack).
//
// Links to issues are not a new table: task_origins is the one external
// reference table, generalized with tenant, role and synced_at columns and a
// YouTrack provider. The display number (HK-12) is metadata, never a key.
//
// Every table is new, but the task_origins changes touch an existing table:
// ALTER TABLE takes ACCESS EXCLUSIVE for the column adds and the provider
// CHECK swap, and the two unique indexes take SHARE while they build. The
// task_origins table is small, so the lock is brief; the bounded lock wait
// keeps a long reader from stalling the task API behind it.
//
// The references to tasks take SHARE ROW EXCLUSIVE on tasks while the
// constraints are created, which blocks its writers for the duration; the
// same bounded lock wait applies.
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
	// Generalize the one external-reference table.
	`ALTER TABLE task_origins DROP CONSTRAINT IF EXISTS task_origins_provider_check`,
	`ALTER TABLE task_origins ADD CONSTRAINT task_origins_provider_check CHECK(provider IN ('linear','github','sentry','youtrack'))`,
	`ALTER TABLE task_origins ADD COLUMN IF NOT EXISTS tenant TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE task_origins ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'origin' CHECK(role IN ('origin','mirror'))`,
	`ALTER TABLE task_origins ADD COLUMN IF NOT EXISTS synced_at TIMESTAMPTZ`,
	// One YouTrack issue per task and one task per issue. The display number
	// may change when an issue moves; the tenant + internal id do not.
	`CREATE UNIQUE INDEX task_origins_youtrack_task ON task_origins(task_id) WHERE provider='youtrack'`,
	`CREATE UNIQUE INDEX task_origins_youtrack_external ON task_origins(namespace, external_id) WHERE provider='youtrack'`,

	// One YouTrack project per hk project. An unmapped hk project uses OTHER.
	`CREATE TABLE youtrack_projects (
		hk_project TEXT PRIMARY KEY,
		yt_project_id TEXT NOT NULL DEFAULT '',
		short_name TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL)`,
	`INSERT INTO youtrack_projects(hk_project,short_name,created_at,updated_at) VALUES
		('handoffkeep','HK',now(),now()),
		('panewire','PW',now(),now()),
		('mergewake','MW',now(),now()),
		('fleet-ops','FO',now(),now()),
		('ParkingArcade','PA',now(),now()),
		('auto_trader','AT',now(),now()),
		('recallbeam','RB',now(),now()),
		('rstack','RS',now(),now()),
		('pacelever','PL',now(),now()),
		('other','OTHER',now(),now())
		ON CONFLICT(hk_project) DO NOTHING`,

	// hk's priority and lane writes land in PG first and leave through this
	// outbox. A later write to the same field replaces the pending one.
	`CREATE TABLE youtrack_outbox (
		id BIGSERIAL PRIMARY KEY,
		task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		op TEXT NOT NULL CHECK(op IN ('priority','lane')),
		value TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL,
		next_attempt_at TIMESTAMPTZ NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		done_at TIMESTAMPTZ)`,
	`CREATE UNIQUE INDEX youtrack_outbox_pending ON youtrack_outbox(task_id,op) WHERE done_at IS NULL`,
	`CREATE INDEX youtrack_outbox_due ON youtrack_outbox(next_attempt_at,id) WHERE done_at IS NULL`,

	`CREATE TABLE youtrack_cursor (
		singleton BOOLEAN PRIMARY KEY DEFAULT true CHECK(singleton),
		cursor TEXT NOT NULL DEFAULT '',
		last_activity_id TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
	`INSERT INTO youtrack_cursor(singleton) VALUES (true) ON CONFLICT DO NOTHING`,

	// hk_comment_id has no foreign key: task_comments is optional in older
	// schemas (search must still work when it is absent).
	`CREATE TABLE youtrack_comment_links (
		hk_comment_id BIGINT PRIMARY KEY,
		task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		youtrack_comment_id TEXT NOT NULL UNIQUE,
		created_at TIMESTAMPTZ NOT NULL)`,
	`CREATE INDEX youtrack_comment_links_task ON youtrack_comment_links(task_id)`,

	`CREATE TABLE youtrack_field_cache (
		task_id BIGINT PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
		priority INTEGER NOT NULL,
		lane TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT 'youtrack' CHECK(source IN ('hk','youtrack')),
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
