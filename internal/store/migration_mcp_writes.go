package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const mcpWritesMigrationID = "20261108-mcp-dots-writes"

func init() {
	registerMigration(migration{ID: mcpWritesMigrationID, Apply: func(ctx context.Context, tx pgx.Tx) error {
		return execAll(ctx, tx, mcpWritesStatements)
	}})
}

// The MCP write contract (hk 3465) needs two additive changes. A keyed comment
// records its idempotency key in source_ref, so a replayed call finds the
// comment it made. mcp_write_audit records every MCP write with its actor,
// channel and author label, and holds the first result of a keyed write so a
// replay returns that result.
//
// The ALTER takes ACCESS EXCLUSIVE on task_comments for the length of the
// catalog change only: a constant default on a new column does not rewrite
// rows. The previous release never reads source_ref and keeps inserting
// comments, which the default fills. The partial unique index on the audit
// table is created on a new, empty table, so it takes no lock on live data.
var mcpWritesStatements = []string{
	`ALTER TABLE task_comments ADD COLUMN IF NOT EXISTS source_ref TEXT NOT NULL DEFAULT '' CHECK (length(source_ref) <= 128)`,
	`CREATE TABLE IF NOT EXISTS mcp_write_audit (
 id BIGSERIAL PRIMARY KEY,
 tool TEXT NOT NULL CHECK (length(tool) <= 64),
 actor TEXT NOT NULL CHECK (length(actor) <= 320),
 channel TEXT NOT NULL CHECK (length(channel) <= 32),
 author TEXT NOT NULL CHECK (length(author) <= 128),
 task_id BIGINT,
 idempotency_key TEXT NOT NULL DEFAULT '' CHECK (length(idempotency_key) <= 128),
 result JSONB,
 at TIMESTAMPTZ NOT NULL)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS mcp_write_audit_key ON mcp_write_audit(tool, actor, idempotency_key) WHERE idempotency_key <> ''`,
}
