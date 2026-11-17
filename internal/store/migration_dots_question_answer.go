package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const dotsQuestionAnswerMigrationID = "20261117-dots-question-answer"

func init() {
	registerMigration(migration{ID: dotsQuestionAnswerMigrationID, Apply: applyDotsQuestionAnswerMigration})
}

// applyDotsQuestionAnswerMigration records when a dots QUESTION reply was
// answered, by whom, and through which task comment (hk 5000, operator
// decision 364). hk has no other place to keep that: a QUESTION is a reply
// comment plus a dots_results row, and answering it must close the row so
// dots_inbox and dots results stop reporting the question as open.
//
// The columns are nullable with no default, so existing rows read as empty
// and the previous release, which never reads them, keeps working. ADD COLUMN
// without a default takes ACCESS EXCLUSIVE on dots_results only for the
// catalog change, a short wait at startup.
func applyDotsQuestionAnswerMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, dotsQuestionAnswerMigration)
}

var dotsQuestionAnswerMigration = []string{
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answered_at TIMESTAMPTZ`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answered_by TEXT`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answer_comment_id BIGINT`,
}
