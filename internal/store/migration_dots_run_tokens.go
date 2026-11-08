package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const dotsRunTokensMigrationID = "20261108-dots-run-tokens"

func init() {
	registerMigration(migration{ID: dotsRunTokensMigrationID, Apply: applyDotsRunTokensMigration})
}

// applyDotsRunTokensMigration creates dots_run_tokens, one row per START (or
// takeover) that issued a run token. A request has at most one active row
// (superseded_at IS NULL); a takeover supersedes it and inserts the next, so
// the rows are the token history. The table is new and locks no existing row.
// Its foreign key takes SHARE ROW EXCLUSIVE on tasks only while the table is
// created, the same short wait dots_results took. Expand-only: the previous
// release never reads or writes it.
func applyDotsRunTokensMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, dotsRunTokensMigration)
}

var dotsRunTokensMigration = []string{
	`CREATE TABLE IF NOT EXISTS dots_run_tokens (
		id BIGSERIAL PRIMARY KEY,
		task_id BIGINT NOT NULL REFERENCES tasks(id),
		token TEXT NOT NULL CHECK(token ~ '^[0-9a-f]{64}$'),
		issue_key TEXT NOT NULL CHECK(issue_key ~ '^[0-9a-f]{64}$'),
		takeover BOOLEAN NOT NULL DEFAULT FALSE,
		issued_by TEXT NOT NULL,
		issued_at TIMESTAMPTZ NOT NULL,
		superseded_at TIMESTAMPTZ,
		UNIQUE(task_id, issue_key)
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS dots_run_tokens_active ON dots_run_tokens(task_id) WHERE superseded_at IS NULL`,
}
