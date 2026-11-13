package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// 20261113-dots-blocked-kind admits the outbox kind 'dots_blocked'. A dots
// BLOCKED reply is an interrupting lane event of type blocked, and the outbox
// kind is what names that type (hubforward), so BLOCKED gets its own kind
// instead of sharing dots_decision with QUESTION. Existing rows keep their
// kinds; only the set of admitted kinds grows.
//
// Expand-only: the CHECK is widened and added NOT VALID, so the new constraint
// is checked on new rows only and the old rows are not rescanned. Lock:
// ACCESS EXCLUSIVE on notification_outbox for the drop and the add, both
// catalog-only once NOT VALID.
func init() {
	registerMigration(migration{ID: "20261113-dots-blocked-kind", Apply: applyDotsBlockedKindMigration})
}

func applyDotsBlockedKindMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, []string{
		`ALTER TABLE notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_kind_check`,
		`ALTER TABLE notification_outbox ADD CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result')) NOT VALID`,
	})
}
