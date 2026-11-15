package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const attachmentArchiveMigrationID = "20261116-attachment-archive"

func init() {
	registerMigration(migration{ID: attachmentArchiveMigrationID, Apply: applyAttachmentArchiveMigration})
}

// applyAttachmentArchiveMigration records the archive of an attachment's hot
// object: when the copy was confirmed in Drive (archived_at), where it is
// (archive_location), and when the object store copy was deleted
// (hot_deleted_at). hk 4849, design/2026-11-16/hk-attachment-archive.
//
// It is expand-only: three nullable-or-defaulted columns, so the previous
// release keeps inserting attachments without naming them and keeps reading
// every row. No column is dropped, renamed or narrowed.
//
// Lock: the three ADD COLUMN statements take ACCESS EXCLUSIVE on attachments
// one after another. Registered migrations run once, inside the migrating
// transaction, so the lock is taken at the deploy that introduces these
// columns, never on a later start. Each add is metadata-only (no default that
// rewrites rows; the one default is constant), so the pause is a catalog
// update.
func applyAttachmentArchiveMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, attachmentArchiveMigration)
}

var attachmentArchiveMigration = []string{
	`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ`,
	`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS archive_location TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS hot_deleted_at TIMESTAMPTZ`,
}
