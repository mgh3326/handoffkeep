package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const translationsMigrationID = "20261107-translations"

func init() {
	registerMigration(migration{ID: translationsMigrationID, Apply: applyTranslationsMigration})
}

// The table keeps translations alongside their original records. Creating the
// table and its index locks only the new table, with no lock on existing data.
func applyTranslationsMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`CREATE TABLE translations (entity TEXT NOT NULL CHECK(entity IN ('task','decision_request','task_comment')), entity_id TEXT NOT NULL, field TEXT NOT NULL CHECK(field IN ('title','body','question','options')), lang TEXT NOT NULL CHECK(lang = 'ko'), source_sha256 TEXT NOT NULL, text TEXT NOT NULL, translator TEXT NOT NULL, updated_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(entity, entity_id, field, lang))`,
		`CREATE INDEX translations_text_trgm ON translations USING GIN (text gin_trgm_ops)`,
	})
}
