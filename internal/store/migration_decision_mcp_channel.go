package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-decision-mcp-channel admits the MCP answer channels 'mcp' (a bearer
// MCP caller) and 'dots' (the chatgpt channel) in decision_answer_attempts.
// Until now an MCP decision_answer was recorded as 'console', so the answer
// showed the console as its route. The CHECK is widened, never narrowed: every
// value the previous release writes is still admitted, so the previous release
// keeps writing and reading the table.
//
// Expand-only. Lock: ACCESS EXCLUSIVE on decision_answer_attempts while the
// CHECK is replaced, one scan of a small append-only table; the table's
// append-only trigger blocks UPDATE and DELETE, not this DDL.
func init() {
	registerMigration(migration{ID: "20261113-decision-mcp-channel", Apply: applyDecisionMCPChannelMigration})
}

func applyDecisionMCPChannelMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE decision_answer_attempts
			DROP CONSTRAINT decision_answer_attempts_channel_check,
			ADD CONSTRAINT decision_answer_attempts_channel_check
				CHECK(channel IN ('cli','assistant','console','session','linear','lane','default','mcp','dots'))`,
	})
}
