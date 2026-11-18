package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261118-doc-retention adds the audit table for document retention
// (hk 5057, hk 4899 A3). Retention deletes review/mwr-* and screens/*
// bodies whose change merged or whose job ended over 30 days ago; each
// deleted document leaves one row here with who deleted it, when, the key
// and the body's sha256, so a deletion can be audited after the body is
// gone. Dry runs write no row.
//
// Expand-only: one new table no previous release reads or writes. Locks:
// ACCESS EXCLUSIVE on doc_prune_log for the catalog update only; no
// existing table is touched.
func init() {
	registerMigration(migration{ID: "20261118-doc-retention", Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, []string{
			`CREATE TABLE IF NOT EXISTS doc_prune_log (
				id BIGSERIAL PRIMARY KEY,
				document_key TEXT NOT NULL,
				sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
				pruned_at TIMESTAMPTZ NOT NULL,
				pruned_by TEXT NOT NULL
			)`,
			`CREATE INDEX IF NOT EXISTS doc_prune_log_pruned_at ON doc_prune_log (pruned_at)`,
		})
	}})
}
