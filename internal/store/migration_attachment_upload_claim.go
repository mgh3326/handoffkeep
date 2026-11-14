package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261114-attachment-upload-claim adds attachment_uploads.claim_nonce, the
// per-claim token a completion presents on every transition out of
// 'completing'. A streamed completion can run far longer than the five-minute
// claim lease, so a second CompleteUpload may reclaim the row while the first
// still runs; without the token the stale completion's transitions could move
// a row another completion owns. The nonce is set when a claim wins
// pending -> completing and checked on every completing -> * transition and
// lease heartbeat; it is meaningful only while status='completing'.
//
// Expand-only: one nullable column with no default and no constraint; the
// previous release never reads or writes it. Locks: ACCESS EXCLUSIVE on
// attachment_uploads for the catalog update only, no table rewrite.
func init() {
	registerMigration(migration{ID: "20261114-attachment-upload-claim", Apply: applyAttachmentUploadClaimMigration})
}

func applyAttachmentUploadClaimMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE attachment_uploads ADD COLUMN IF NOT EXISTS claim_nonce text`,
	})
}
