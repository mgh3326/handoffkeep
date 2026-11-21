package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261121-task-priority-order is the PostgreSQL side of the default task list
// index (hk 5879); its MySQL counterpart carries the same ID in
// migration_mysql_task_priority_order.go. Without it a task list with no lane
// filter sorts the whole tasks table to return one page. The index runs in the
// migrating transaction and takes SHARE on tasks for the scan, which is brief
// on a table of about 6k rows.
func init() {
	registerMigration(migration{ID: "20261121-task-priority-order", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE INDEX IF NOT EXISTS tasks_priority_order ON tasks(priority DESC, created_at ASC, id ASC)`,
		})
	}})
}
