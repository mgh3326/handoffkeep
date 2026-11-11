package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261111-yt-feed adds the two tables of the YouTrack change-feed tracker
// (hk 3997, yt_feed.go): yt_feed_cursors, one row per feed consumer holding
// the last sync_log sync_id it handled, and yt_snapshot_state, per entity and
// kind the count of snapshot ops prepared (seq) and the SHA and op of the
// last one.
// Consumers (task fields, documents, decisions) read every change to their
// models from sync_log through one cursor instead of an enqueue in each of
// the many writers of those tables.
//
// No After: the statements need no other table (no foreign key; sync_log is an
// integer version, applied before every registered ID).
//
// Expand-only: two new tables with their primary keys. CREATE TABLE locks only
// the new, empty tables. The previous release neither reads nor writes them,
// and the migration writes no row.
func init() {
	registerMigration(migration{ID: "20261111-yt-feed", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytFeedStatements)
	}})
}

var ytFeedStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_feed_cursors (
		consumer TEXT PRIMARY KEY CHECK(consumer <> '' AND length(consumer) <= 64),
		last_sync_id BIGINT NOT NULL CHECK(last_sync_id >= 0),
		updated_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS yt_snapshot_state (
		entity_kind TEXT NOT NULL CHECK(entity_kind <> '' AND length(entity_kind) <= 64),
		entity_id TEXT NOT NULL CHECK(entity_id <> '' AND length(entity_id) <= 512),
		kind TEXT NOT NULL CHECK(kind <> '' AND length(kind) <= 64),
		seq BIGINT NOT NULL CHECK(seq > 0),
		last_sha TEXT NOT NULL,
		last_op_id BIGINT NOT NULL,
		PRIMARY KEY(entity_kind, entity_id, kind)
	)`,
}
