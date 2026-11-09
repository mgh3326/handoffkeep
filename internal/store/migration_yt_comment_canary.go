package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261109-yt-comment-canary adds what slice b1a (hk 3739) needs to copy hk
// comments to YouTrack through the write protocol:
//
//   - yt_comment_canary: the tasks enrolled in the canary, each with the issue
//     it was verified against at enrollment (tenant and stable internal id).
//   - yt_comment_write_links: which op owns which PG comment. One op per
//     comment and one comment per op; the legacy mirror and the YouTrack
//     import skip a comment that has a row here.
//
// Expand-only: two new tables. The link table's foreign key to yt_write_ops
// takes SHARE ROW EXCLUSIVE on yt_write_ops while it is created; no release
// before this one writes yt_write_ops, so nothing waits on it. No lock is taken
// on tasks or task_comments (neither table is referenced by a foreign key: a
// key on task_comments would block comment inserts during the migration, and
// both ids are append-only). The previous release neither reads nor writes
// either table, and the migration writes no row.
func init() {
	registerMigration(migration{ID: "20261109-yt-comment-canary", After: []string{"20261109-yt-write-ops"}, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytCommentCanaryStatements)
	}})
}

var ytCommentCanaryStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_comment_canary (
		task_id BIGINT PRIMARY KEY CHECK(task_id > 0),
		tenant TEXT NOT NULL CHECK(tenant <> '' AND length(tenant) <= 255),
		issue_id TEXT NOT NULL CHECK(issue_id <> '' AND length(issue_id) <= 64),
		issue_readable TEXT NOT NULL DEFAULT '' CHECK(length(issue_readable) <= 64),
		project_id TEXT NOT NULL DEFAULT '' CHECK(length(project_id) <= 64),
		added_by TEXT NOT NULL CHECK(added_by <> '' AND length(added_by) <= 200),
		added_at TIMESTAMPTZ NOT NULL,
		UNIQUE(tenant, issue_id))`,
	`CREATE TABLE IF NOT EXISTS yt_comment_write_links (
		comment_id BIGINT PRIMARY KEY CHECK(comment_id > 0),
		op_id BIGINT NOT NULL UNIQUE REFERENCES yt_write_ops(id),
		created_at TIMESTAMPTZ NOT NULL)`,
}
