package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-yt-binding-intents (hk 4492, design/2026-11-13/yt-comment-binding-lifecycle
// §6.6, §6.9) adds the automatic binder's durable state: yt_binding_intents,
// one row per task whose comments the binder has seen but not yet bound or
// refused, carrying the binder's own backoff, and yt_binding_cursor, the
// one-row watermark over task_comments that makes "seen" survive a restart.
//
// Additive: two new, empty tables and one row of the second, no lock on an
// existing table, and no foreign key to tasks or yt_task_bindings (no lane
// holder takes a task row for these, lockorder.go). The previous release never
// reads or writes them, so it keeps serving on the migrated schema.
func init() {
	registerMigration(migration{ID: YTBindingIntentsMigration, After: []string{YTTaskBindingsMigration}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytBindingIntentsStatements)
	}})
}

// YTBindingIntentsMigration is the migration's registered ID.
const YTBindingIntentsMigration = "20261113-yt-binding-intents"

var ytBindingIntentsStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_binding_intents (
		task_id BIGINT PRIMARY KEY CHECK(task_id > 0),
		tenant TEXT NOT NULL CHECK(tenant <> ''),
		state TEXT NOT NULL CHECK(state IN ('pending','refused','error','excluded')),
		reason TEXT NOT NULL DEFAULT '' CHECK(length(reason) <= 200),
		attempts INTEGER NOT NULL DEFAULT 0 CHECK(attempts >= 0),
		last_error TEXT NOT NULL DEFAULT '' CHECK(length(last_error) <= 2000),
		next_attempt_at TIMESTAMPTZ NOT NULL,
		recorded_at TIMESTAMPTZ NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS yt_binding_intents_due ON yt_binding_intents(next_attempt_at, task_id)`,
	`CREATE TABLE IF NOT EXISTS yt_binding_cursor (
		id SMALLINT PRIMARY KEY CHECK(id = 1),
		last_comment_id BIGINT NOT NULL DEFAULT 0 CHECK(last_comment_id >= 0))`,
	`INSERT INTO yt_binding_cursor(id, last_comment_id) VALUES (1, 0) ON CONFLICT DO NOTHING`,
}
