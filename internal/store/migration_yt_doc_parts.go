package store

import (
	"context"
	"github.com/jackc/pgx/v5"
)

func init() {
	registerMigration(migration{ID: "20261109-yt-doc-parts", Apply: applyYouTrackDocPartsMigration})
}

// Add part identity to existing article rows; the ALTERs take ACCESS EXCLUSIVE on the new sync ledger only, not application tables.
func applyYouTrackDocPartsMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE youtrack_doc_articles ADD COLUMN IF NOT EXISTS document_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE youtrack_doc_articles ADD COLUMN IF NOT EXISTS part_index INTEGER NOT NULL DEFAULT 1 CHECK (part_index > 0)`,
		`ALTER TABLE youtrack_doc_articles ADD COLUMN IF NOT EXISTS part_count INTEGER NOT NULL DEFAULT 1 CHECK (part_count > 0)`,
		`ALTER TABLE youtrack_doc_articles ADD COLUMN IF NOT EXISTS part_sha TEXT NOT NULL DEFAULT ''`,
		`UPDATE youtrack_doc_articles SET document_key=doc_key WHERE document_key=''`,
		`CREATE INDEX IF NOT EXISTS youtrack_doc_articles_document_part_idx ON youtrack_doc_articles(document_key,part_index)`,
	})
}
