package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261110-yt-task-bindings adds what comment mode on (hk 3810) needs:
// yt_task_bindings, each task's verified YouTrack issue (tenant and stable
// internal id), the one authority mode on copies a task's comments to. It is
// written only by `youtrack comments-backfill` (Store.AddYTTaskBinding), after
// the same verification a canary enrollment makes.
//
// No After: the statements need no other table (there is no foreign key), so
// the entry runs in ID order, after every 20261109 entry. An After on
// 20261109-yt-comment-canary would only pick a position (AGENTS.md
// §Migrations), and would break the tests that open a registry without that
// entry.
//
// Expand-only: one new table with its primary key and unique index. CREATE
// TABLE takes locks on the new, empty table only; there is no foreign key, so
// no lock is taken on tasks or any existing table (a task id is append-only,
// as in yt_comment_canary). The previous release neither reads nor writes the
// table, and the migration writes no row.
func init() {
	registerMigration(migration{ID: YTTaskBindingsMigration, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytTaskBindingsStatements)
	}})
}

// YTTaskBindingsMigration is the migration's registered ID. The comments
// preflight, which never migrates, refuses a schema that does not record it
// (Store.MigrationApplied).
const YTTaskBindingsMigration = "20261110-yt-task-bindings"

var ytTaskBindingsStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_task_bindings (
		task_id BIGINT PRIMARY KEY CHECK(task_id > 0),
		tenant TEXT NOT NULL CHECK(tenant <> '' AND length(tenant) <= 255),
		issue_id TEXT NOT NULL CHECK(issue_id <> '' AND length(issue_id) <= 64),
		issue_readable TEXT NOT NULL DEFAULT '' CHECK(length(issue_readable) <= 64),
		project_id TEXT NOT NULL DEFAULT '' CHECK(length(project_id) <= 64),
		bound_by TEXT NOT NULL CHECK(bound_by <> '' AND length(bound_by) <= 200),
		bound_at TIMESTAMPTZ NOT NULL,
		UNIQUE(tenant, issue_id))`,
}
