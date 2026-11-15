package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261115-event-comment-keys gives system event comments their own idempotency
// key, separate from caller-controlled MCP source_ref values. The previous
// release omits the nullable column and continues writing comments. ADD COLUMN
// takes ACCESS EXCLUSIVE briefly; CREATE UNIQUE INDEX takes SHARE on
// task_comments for the duration of the index build.
func init() {
	registerMigration(migration{ID: "20261115-event-comment-keys", After: []string{mcpWritesMigrationID}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`ALTER TABLE task_comments ADD COLUMN IF NOT EXISTS system_event_key TEXT`,
			`CREATE UNIQUE INDEX IF NOT EXISTS task_comments_system_event_key ON task_comments(task_id,system_event_key) WHERE system_event_key IS NOT NULL`,
		})
	}})
}
