package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const attachmentTaskRefMigrationID = "20261109-attachment-task-ref"

func init() {
	registerMigration(migration{ID: attachmentTaskRefMigrationID, Apply: applyAttachmentTaskRefMigration})
}

// applyAttachmentTaskRefMigration widens attachment_refs.ref_kind to admit
// 'task', so an attachment can be filed against an hk task. The new CHECK is a
// superset of the old one, so every row the previous release wrote still
// passes it, and the previous release never writes 'task'.
//
// Lock: DROP CONSTRAINT and ADD CONSTRAINT take ACCESS EXCLUSIVE on
// attachment_refs until this transaction commits, and ADD validates the
// existing rows under that lock. The table has one row per attachment
// reference and only attachment puts write it, so the wait is short. The
// drop and the add run in one transaction, so no committed state lacks the
// constraint.
func applyAttachmentTaskRefMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, attachmentTaskRefMigration)
}

var attachmentTaskRefMigration = []string{
	`ALTER TABLE attachment_refs DROP CONSTRAINT IF EXISTS attachment_refs_ref_kind_check`,
	`ALTER TABLE attachment_refs ADD CONSTRAINT attachment_refs_ref_kind_check CHECK(ref_kind IN ('checkpoint','document','memory','task','none'))`,
}
