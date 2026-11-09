package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const attachmentUploadsReleaseMigrationID = "20261109-attachment-uploads-release"

func init() {
	registerMigration(migration{ID: attachmentUploadsReleaseMigrationID, Apply: applyAttachmentUploadsReleaseMigration})
}

// applyAttachmentUploadsReleaseMigration adds the staging release time to
// attachment_uploads and two partial indexes over the rows still charged to the
// staging budget. The column is nullable with no default, so the previous
// release, which never reads it, keeps working. The table is this feature's
// own and no existing row is locked; the indexes build on rows that exist only
// in this release.
func applyAttachmentUploadsReleaseMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, attachmentUploadsReleaseMigration)
}

var attachmentUploadsReleaseMigration = []string{
	`ALTER TABLE attachment_uploads ADD COLUMN IF NOT EXISTS staging_deleted_at TIMESTAMPTZ`,
	`CREATE INDEX IF NOT EXISTS attachment_uploads_charged_created ON attachment_uploads(created_at) WHERE staging_deleted_at IS NULL`,
	`CREATE INDEX IF NOT EXISTS attachment_uploads_charged_client ON attachment_uploads(created_by) WHERE staging_deleted_at IS NULL`,
}
