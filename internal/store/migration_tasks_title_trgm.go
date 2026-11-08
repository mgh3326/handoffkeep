package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const tasksTitleTrgmMigrationID = "20261108-tasks-title-trgm"

func init() {
	registerMigration(migration{ID: tasksTitleTrgmMigrationID, Apply: applyTasksTitleTrgmMigration})
}

// searchTasks matches a task title with ILIKE, which only a pg_trgm index can
// serve; tasks had none, so every title match scanned the table (hk 2658).
//
// Lock: a plain CREATE INDEX takes SHARE on tasks for the whole build. Task
// writes (INSERT, UPDATE, DELETE) wait for it; reads continue. CREATE INDEX
// CONCURRENTLY is not possible here: every registered Apply runs inside the
// migrating transaction, and CONCURRENTLY refuses to run in one. The build runs
// under the schema lock (lock order level 1), once per database, when the
// first binary that knows this ID opens it.
func applyTasksTitleTrgmMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`CREATE INDEX IF NOT EXISTS tasks_title_trgm ON tasks USING GIN (title gin_trgm_ops)`,
	})
}
