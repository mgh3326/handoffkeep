package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const youtrackDocsMigrationID = "20261108-youtrack-docs"

func init() {
	registerMigration(migration{ID: youtrackDocsMigrationID, Apply: applyYouTrackDocsMigration})
}

// applyYouTrackDocsMigration adds the ledger that mirrors operator-read hk
// documents into YouTrack 기술 자료. It creates one new table and reads
// documents only; it takes no lock on an existing table, so the previous
// release keeps reading and writing as before.
//
// doc_key is the hk document key for an article, or "index:<prefix>:<hk
// project>" for the per-prefix parent article. source_sha is the document's
// sha256 at the last write, which is what makes an unchanged document a skip.
func applyYouTrackDocsMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, youtrackDocsMigration)
}

var youtrackDocsMigration = []string{
	`CREATE TABLE IF NOT EXISTS youtrack_doc_articles (
		doc_key TEXT PRIMARY KEY,
		hk_project TEXT NOT NULL,
		article_id TEXT NOT NULL,
		source_sha TEXT NOT NULL DEFAULT '',
		bytes BIGINT NOT NULL DEFAULT 0 CHECK(bytes >= 0),
		synced_at TIMESTAMPTZ NOT NULL)`,
}
