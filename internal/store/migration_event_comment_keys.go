package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261115-event-comment-keys makes system event comments idempotent. The
// previous release keeps writing comments with source_ref=” and does not
// touch this partial index. CREATE INDEX takes SHARE on task_comments while
// building the small partial index; comment writers briefly wait for it.
func init() {
	registerMigration(migration{ID: "20261115-event-comment-keys", After: []string{mcpWritesMigrationID}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{`CREATE UNIQUE INDEX IF NOT EXISTS task_comments_event_key ON task_comments(task_id,source_ref) WHERE source_ref LIKE 'event:%'`})
	}})
}
