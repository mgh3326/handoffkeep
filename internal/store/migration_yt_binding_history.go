package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-yt-binding-history (hk 4490, design/2026-11-13/yt-comment-binding-lifecycle
// §6.9) adds yt_binding_history: one append-only row per binding published
// (AddYTTaskBinding) and per binding the operator removed
// (UnbindYTTaskBinding). The latest row of a task is how an operator unbind
// outlives the deleted binding row, which is the only reason the table exists:
// the automatic binder (a later slice) reads it so it never undoes an unbind.
// It also widens yt_task_bindings.origin to admit 'auto_verified', the origin
// that binder will publish; nothing in this release writes it.
//
// After the bindings table and the origin migration: the statements alter the
// table and replace the CHECK the origin migration put on its column. Without
// that dependency the swap could run first and the older CHECK would be added
// back afterwards.
//
// Expand-only: CREATE TABLE, CREATE INDEX and CREATE TRIGGER touch the new,
// empty table only. The constraint swap takes ACCESS EXCLUSIVE on
// yt_task_bindings for a statement and validates the few thousand rows it
// holds; the new CHECK admits every value the old one did. The previous
// release inserts the column's default 'import_verified' (passes), scans
// origin into a string (accepts 'auto_verified') and never names the new
// table. There is no foreign key to tasks: no lane holder takes a task row for
// this table (lockorder.go), and a task id is append-only. The migration
// writes no row.
func init() {
	registerMigration(migration{ID: YTBindingHistoryMigration, After: []string{YTTaskBindingsMigration, YTTaskBindingOriginMigration}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytBindingHistoryStatements)
	}})
}

// YTBindingHistoryMigration is the migration's registered ID.
const YTBindingHistoryMigration = "20261113-yt-binding-history"

var ytBindingHistoryStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_binding_history (
		id BIGSERIAL PRIMARY KEY,
		task_id BIGINT NOT NULL CHECK(task_id > 0),
		tenant TEXT NOT NULL CHECK(tenant <> '' AND length(tenant) <= 255),
		issue_id TEXT NOT NULL CHECK(issue_id <> '' AND length(issue_id) <= 64),
		origin TEXT NOT NULL CHECK(origin IN ('import_verified','op_applied','auto_verified')),
		action TEXT NOT NULL CHECK(action IN ('bound','unbound')),
		actor TEXT NOT NULL CHECK(actor <> '' AND length(actor) <= 200),
		reason TEXT NOT NULL DEFAULT '' CHECK(length(reason) <= 2000),
		at TIMESTAMPTZ NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS yt_binding_history_task ON yt_binding_history(task_id, id DESC)`,
	// Append-only, as task_comments (comments.go).
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_binding_history_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN RAISE EXCEPTION 'yt_binding_history is append-only'; END; $$`,
	`DROP TRIGGER IF EXISTS yt_binding_history_append_only ON yt_binding_history`,
	`CREATE TRIGGER yt_binding_history_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON yt_binding_history
		FOR EACH STATEMENT EXECUTE FUNCTION handoffkeep_yt_binding_history_guard()`,
	// The column CHECK the origin migration declared inline is named
	// <table>_<column>_check by Postgres.
	`ALTER TABLE yt_task_bindings DROP CONSTRAINT IF EXISTS yt_task_bindings_origin_check`,
	`ALTER TABLE yt_task_bindings ADD CONSTRAINT yt_task_bindings_origin_check
		CHECK(origin IN ('import_verified','op_applied','auto_verified'))`,
}
