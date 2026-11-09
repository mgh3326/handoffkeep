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
//   - yt_write_ops.tenant: the YouTrack host an op was bound to when it was
//     prepared. It is part of the op's identity, so a trigger keeps it
//     immutable like the other identity columns; only a worker on that tenant
//     sends or searches for the op. The empty string for an op prepared without one.
//
// Expand-only: two new tables, one new column with a constant default, one new
// trigger function and trigger. Locks: ADD COLUMN takes ACCESS EXCLUSIVE on
// yt_write_ops briefly (a constant default rewrites no row; its CHECK scans the
// rows the table holds, none before this release), CREATE TRIGGER and the link table's foreign key take SHARE ROW EXCLUSIVE on
// it; no released binary reads or writes yt_write_ops (its first writer is this
// slice), so nothing waits. No lock is taken on tasks or task_comments
// (neither table is referenced by a foreign key: a key on task_comments would
// block comment inserts during the migration, and both ids are append-only).
// The previous release neither reads nor writes any of these objects, and the
// migration writes no row. (Unreleased: this ID was first written for
// 86e5f20 and extended in place with the tenant column before any release.)
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
	`ALTER TABLE yt_write_ops ADD COLUMN IF NOT EXISTS tenant TEXT NOT NULL DEFAULT '' CHECK(length(tenant) <= 255)`,
	`CREATE OR REPLACE FUNCTION handoffkeep_yt_write_ops_tenant_guard() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN
		IF NEW.tenant IS DISTINCT FROM OLD.tenant THEN
			RAISE EXCEPTION 'yt_write_ops: the tenant of op % is immutable', OLD.id;
		END IF;
		RETURN NEW;
	END; $$`,
	`DROP TRIGGER IF EXISTS yt_write_ops_tenant_guard ON yt_write_ops`,
	`CREATE TRIGGER yt_write_ops_tenant_guard BEFORE UPDATE ON yt_write_ops FOR EACH ROW EXECUTE FUNCTION handoffkeep_yt_write_ops_tenant_guard()`,
}
