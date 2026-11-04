package store

// Version 36 adds the desk ledger's read cursors and project status updates
// (docs/linear-internalize.md, "Desk ledger" and "Status updates with
// health"). A ledger_cursors row is one consumer's position in the existing
// change feed: only an explicit ack moves it (ledger.go). status_updates
// holds the health a person sets on a project, with the task counts at write
// time and the change from the previous update (status_updates.go).
//
// Both are new tables with their own indexes. No foreign key is created, so
// the version locks no existing table, and it writes no row: a consumer gets
// its cursor on its first ack, and a project its first update when someone
// posts one. Neither table is a change-feed source.
var ledgerV36 = []string{
	`CREATE TABLE IF NOT EXISTS ledger_cursors (
		consumer TEXT PRIMARY KEY CHECK(consumer ~ '^[A-Za-z0-9._-]{1,128}$'),
		acked_sync_id BIGINT NOT NULL CHECK(acked_sync_id >= 0),
		acked_at TIMESTAMPTZ NOT NULL,
		updated_by TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS status_updates (
		id BIGSERIAL PRIMARY KEY,
		project TEXT NOT NULL,
		health TEXT NOT NULL CHECK(health IN ('on_track','at_risk','off_track')),
		body TEXT NOT NULL,
		snapshot JSONB NOT NULL,
		diff JSONB NOT NULL,
		created_by TEXT NOT NULL,
		created_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS status_updates_project_created ON status_updates(project, created_at DESC, id DESC)`,
	`INSERT INTO schema_version(version) VALUES (36)`,
}
