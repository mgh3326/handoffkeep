package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261112-yt-article-bindings adds yt_article_bindings (hk 4053): which
// YouTrack article each hk document already is, the mapping the document
// create/update kind (Y5b) needs so it updates the imported article instead
// of creating a second one. `youtrack articles-backfill` writes it after
// verifying the article (Store.AddYTArticleBinding); a binding is never
// overwritten.
//
// No After: the statement needs no other table (there is no foreign key, as
// in yt_task_bindings), so the entry runs in ID order.
//
// Expand-only: one new table with its primary key and unique index. CREATE
// TABLE takes locks on the new, empty table only; no lock is taken on
// documents or any existing table. The previous release neither reads nor
// writes the table, and the migration writes no row.
func init() {
	registerMigration(migration{ID: YTArticleBindingsMigration, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, ytArticleBindingsStatements)
	}})
}

// YTArticleBindingsMigration is the migration's registered ID. The articles
// preflight and the audit, which never migrate, read whether it is applied
// (Store.MigrationApplied).
const YTArticleBindingsMigration = "20261112-yt-article-bindings"

var ytArticleBindingsStatements = []string{
	`CREATE TABLE IF NOT EXISTS yt_article_bindings (
		document_id BIGINT PRIMARY KEY CHECK(document_id > 0),
		tenant TEXT NOT NULL CHECK(tenant <> '' AND length(tenant) <= 255),
		article_id TEXT NOT NULL CHECK(article_id <> '' AND length(article_id) <= 64),
		article_readable TEXT NOT NULL DEFAULT '' CHECK(length(article_readable) <= 64),
		project_id TEXT NOT NULL CHECK(project_id <> '' AND length(project_id) <= 64),
		part INT NOT NULL DEFAULT 0 CHECK(part >= 0),
		generation INT NOT NULL DEFAULT 0 CHECK(generation >= 0),
		content_sha TEXT CHECK(content_sha ~ '^[0-9a-f]{64}$'),
		origin TEXT NOT NULL CHECK(origin IN ('import_verified','op_applied')),
		op_id BIGINT NULL,
		needs_update BOOL NOT NULL DEFAULT false,
		bound_by TEXT NOT NULL CHECK(bound_by <> '' AND length(bound_by) <= 200),
		bound_at TIMESTAMPTZ NOT NULL,
		UNIQUE(tenant, article_id))`,
}
