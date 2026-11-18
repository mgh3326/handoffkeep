package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const dotsInboxReadsMigrationID = "20261118-dots-inbox-reads"

func init() {
	registerMigration(migration{ID: dotsInboxReadsMigrationID, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, dotsInboxReadsStatements)
	}})
}

// 20261118-dots-inbox-reads adds dots_inbox_reads: one row per dots_inbox MCP
// call, with the server time, the authenticated principal, how many requests
// the call listed and its result (hk 4924). The operator asks whether dots
// checks its inbox on a schedule; until now nothing persisted a read.
//
// Expand-only: one new table no previous release reads or writes. Locks:
// ACCESS EXCLUSIVE on dots_inbox_reads for the catalog update only; the index
// builds on a new, empty table and touches no live data.
var dotsInboxReadsStatements = []string{
	`CREATE TABLE IF NOT EXISTS dots_inbox_reads (
		id BIGSERIAL PRIMARY KEY,
		read_at TIMESTAMPTZ NOT NULL,
		principal TEXT NOT NULL,
		listed INTEGER NOT NULL,
		result TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS dots_inbox_reads_read_at ON dots_inbox_reads (read_at)`,
}
