package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261110-yt-comments-on adds what slice b1 (hk 3810) needs for mode 'on':
//
//   - yt_task_bindings: records task -> YouTrack issue bindings verified before
//     any write op exists (e.g. by comments-backfill or CLI). This keeps
//     yt_entity_map.op_id NOT NULL for full expand-only backward compatibility
//     with the previous release's MapGet scan.
//   - yt_entity_map.tenant: the YouTrack host where the entity was verified,
//     used to bind comment ops to the verified tenant when comments are created.
//   - Backfill yt_entity_map.tenant from yt_comment_canary and yt_write_ops so
//     pre-existing map rows have their tenant populated.
//
// Expand-only: a new table, an additive column with default ”, and backfills.
// Locks: ACCESS EXCLUSIVE on yt_entity_map briefly for ADD COLUMN. yt_entity_map
// holds minimal rows, so the lock completes instantaneously.
func init() {
	registerMigration(migration{
		ID:    "20261110-yt-comments-on",
		After: []string{"20261109-yt-comment-canary"},
		Apply: func(ctx context.Context, tx pgx.Tx) error {
			return execAll(ctx, tx, ytCommentsOnStatements)
		},
	})
}

var ytCommentsOnStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_task_bindings (
		task_id BIGINT PRIMARY KEY CHECK(task_id > 0),
		tenant TEXT NOT NULL CHECK(tenant <> '' AND length(tenant) <= 255),
		issue_id TEXT NOT NULL CHECK(issue_id <> '' AND length(issue_id) <= 64),
		project_id TEXT NOT NULL DEFAULT '' CHECK(length(project_id) <= 64),
		created_at TIMESTAMPTZ NOT NULL,
		UNIQUE(tenant, issue_id)
	)`,
	`ALTER TABLE yt_entity_map ADD COLUMN IF NOT EXISTS tenant TEXT NOT NULL DEFAULT '' CHECK(length(tenant) <= 255)`,
	`UPDATE yt_entity_map m SET tenant = c.tenant FROM yt_comment_canary c WHERE m.entity_kind = 'task' AND m.hk_key = c.task_id::text AND m.tenant = '' AND c.tenant <> ''`,
	`UPDATE yt_entity_map m SET tenant = o.tenant FROM yt_write_ops o WHERE m.op_id = o.id AND m.tenant = '' AND o.tenant <> ''`,
}
