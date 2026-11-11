package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261111-yt-binding-comments-from (hk 3897) adds
// yt_task_bindings.comments_from: the highest task_comments id of the task
// that the binding does not cover. The comment worker's catch-up pass
// (Store.CatchUpYTComments) prepares an op only for a comment above it, so a
// comment written after the binding that no op carries (a writer that does
// not enqueue, or a mode other than on at the time) still reaches the
// binding's issue, and nothing at or below it is ever copied: those comments
// are the import's and the legacy push's.
//
// The backfill sets each existing row to its task's highest comment id now:
// the bindings were published by `youtrack comments-backfill` before this
// release, and every comment written since is either the import's and the
// legacy push's or already has its op, so none is copied again.
// AddYTTaskBinding sets the column itself, in the binding's transaction.
//
// After 20261110-yt-task-bindings: a real dependency, the statements alter
// and update its table.
//
// Expand-only: one column with a constant default (catalog-only on Postgres
// 11 and later, no table rewrite) and a backfill of a few thousand rows. ALTER
// TABLE takes ACCESS EXCLUSIVE on yt_task_bindings only, until the migrating
// transaction commits: comment writers in mode on and the worker's claim wait
// that long to read a binding. task_comments is only read. The previous
// release never names the column: it reads named columns and its binding
// insert gets the default 0, which the catch-up treats as "every comment
// above 0"; run comments-backfill from this release only (Rollout,
// docs/youtrack-write-protocol.md).
func init() {
	registerMigration(migration{ID: YTBindingCommentsFromMigration, After: []string{YTTaskBindingsMigration}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytBindingCommentsFromStatements)
	}})
}

// YTBindingCommentsFromMigration is the migration's registered ID. The
// comments preflight, which never migrates, refuses a schema that does not
// record it (Store.MigrationApplied).
const YTBindingCommentsFromMigration = "20261111-yt-binding-comments-from"

var ytBindingCommentsFromStatements = []string{
	`ALTER TABLE yt_task_bindings ADD COLUMN IF NOT EXISTS comments_from BIGINT NOT NULL DEFAULT 0 CHECK(comments_from >= 0)`,
	`UPDATE yt_task_bindings b SET comments_from = coalesce((SELECT max(c.id) FROM task_comments c WHERE c.task_id = b.task_id), 0)`,
}
