package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261112-yt-issue-intents adds yt_issue_intents (hk 3942): a task created
// in issue mode on, or moved before its op was sent, whose project maps to
// no YouTrack project yet. The write worker prepares its issue_create op
// once the mapping exists (PrepareMappedYTIssues). Additive: a new table,
// with no foreign key to tasks (lockorder.go: no lane holder touches a task
// row); CREATE TABLE takes no lock on an existing table, so the previous
// release keeps serving.
func init() {
	registerMigration(migration{ID: "20261112-yt-issue-intents", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE IF NOT EXISTS yt_issue_intents (
				task_id BIGINT PRIMARY KEY CHECK(task_id > 0),
				recorded_at TIMESTAMPTZ NOT NULL)`,
		})
	}})
}
