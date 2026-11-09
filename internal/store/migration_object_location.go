package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Attachment objects need a durable bucket identity so a verified copy can be
// recorded before operators switch the configured endpoint. This nullable
// column takes ACCESS EXCLUSIVE briefly on attachments and rewrites no rows.
func init() {
	registerMigration(migration{ID: "20261109-attachment-object-location", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{`ALTER TABLE attachments ADD COLUMN IF NOT EXISTS object_location TEXT`})
	}})
}
