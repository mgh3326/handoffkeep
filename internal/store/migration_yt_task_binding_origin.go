package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261111-yt-task-binding-origin adds what the issue_create kind (hk 3898)
// needs to publish a task's binding in the transaction that records its op
// applied:
//
//   - yt_task_bindings.origin: how the binding was published. Every row
//     before this release came from `youtrack comments-backfill`'s
//     verification, so the constant default 'import_verified' is right for
//     them; an applied issue_create op writes 'op_applied'.
//   - yt_task_bindings.op_id: the issue_create op that published an
//     op_applied binding (NULL otherwise). The op's admission gate reads it.
//   - yt_write_ops_binding_conflict: the applied ops whose binding was not
//     published (reason binding_conflict), which the alert worker scans every
//     pass; without it that scan reads every applied op.
//
// After both tables' migrations: the statements alter and index them.
//
// Expand-only: two new nullable or defaulted columns and one partial index.
// The previous release does not name the columns (its inserts get the
// defaults, its reads list their columns). Locks: ADD COLUMN takes ACCESS
// EXCLUSIVE on yt_task_bindings briefly (a constant default rewrites no row;
// the CHECK scans the few rows it holds); CREATE INDEX takes SHARE on
// yt_write_ops for the build, holding back op writes on a table of a few
// thousand rows for that long. The migration writes no row.
func init() {
	registerMigration(migration{ID: "20261111-yt-task-binding-origin", After: []string{YTTaskBindingsMigration, "20261109-yt-write-ops"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytTaskBindingOriginStatements)
	}})
}

var ytTaskBindingOriginStatements = []string{
	`ALTER TABLE yt_task_bindings ADD COLUMN IF NOT EXISTS origin TEXT NOT NULL DEFAULT 'import_verified'
		CHECK(origin IN ('import_verified','op_applied'))`,
	`ALTER TABLE yt_task_bindings ADD COLUMN IF NOT EXISTS op_id BIGINT NULL`,
	`CREATE INDEX IF NOT EXISTS yt_write_ops_binding_conflict ON yt_write_ops(id) WHERE reason = 'binding_conflict'`,
}
