package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const attachmentUploadsMigrationID = "20261109-attachment-uploads"

func init() {
	registerMigration(migration{ID: attachmentUploadsMigrationID, Apply: applyAttachmentUploadsMigration})
}

// applyAttachmentUploadsMigration creates attachment_uploads, one row per
// direct upload begun through attachment_upload_begin. The table is new and
// locks no existing row; the previous release never reads or writes it, so
// the migration is expand-only.
func applyAttachmentUploadsMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, attachmentUploadsMigration)
}

var attachmentUploadsMigration = []string{
	`CREATE TABLE IF NOT EXISTS attachment_uploads (
		id TEXT PRIMARY KEY CHECK(id ~ '^[0-9a-f]{32}$'),
		staging_key TEXT NOT NULL,
		name TEXT NOT NULL,
		mime TEXT NOT NULL,
		size_bytes BIGINT NOT NULL CHECK(size_bytes >= 0),
		sha256 TEXT NOT NULL CHECK(sha256 ~ '^[0-9a-f]{64}$'),
		ref_kind TEXT NOT NULL CHECK(ref_kind IN ('checkpoint','document','memory','task','none')),
		ref_id TEXT NOT NULL DEFAULT '',
		created_by TEXT NOT NULL,
		status TEXT NOT NULL CHECK(status IN ('pending','completing','done','refused')),
		reason TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL)`,
}
