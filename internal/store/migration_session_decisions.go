package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-session-decisions lets sessions add closed decision records with
// their interpretation and ground links. The old source values remain
// accepted, and the new columns have defaults for the previous release.
// Expand-only. Lock: ACCESS EXCLUSIVE on decision_requests while adding two
// metadata columns and replacing its source CHECK; defaults avoid a table
// rewrite on supported PostgreSQL versions.
func init() {
	registerMigration(migration{ID: "20261113-session-decisions", Apply: applySessionDecisionsMigration})
}

func applySessionDecisionsMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE decision_requests
			ADD COLUMN reading TEXT NOT NULL DEFAULT '',
			ADD COLUMN grounds JSONB NOT NULL DEFAULT '[]'::jsonb,
			DROP CONSTRAINT decision_requests_source_check,
		ADD CONSTRAINT decision_requests_source_check
				CHECK(source IN ('task_flag','task_request','job_escalate','lane_event','chat','session_chat','linear')) NOT VALID`,
	})
}
