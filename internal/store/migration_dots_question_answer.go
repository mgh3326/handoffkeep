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
// catalog change, a short wait at startup. The backfill writes the rows an
// already-closed decision request answered: it takes ROW EXCLUSIVE on
// dots_results while it matches the reply body, and reads tasks,
// task_events and task_comments without locking them.
func applyDotsQuestionAnswerMigration(ctx context.Context, tx pgx.Tx) error {
	return execAll(ctx, tx, dotsQuestionAnswerMigration)
}

var dotsQuestionAnswerMigration = []string{
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answered_at TIMESTAMPTZ`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answered_by TEXT`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answer_comment_id BIGINT`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answer_text TEXT`,
	// Questions that an already-closed decision request answered before this
	// release carry no link to the request, so the reply body is the only
	// match. One closed request closes at most one row: the latest question
	// with that body created no later than the request, so a later repeat of
	// the same text stays open for the requester (operator decision 364,
	// hk 5000). Without this backfill the answered question would stay open
	// and a second answer would be accepted.
	`UPDATE dots_results AS dr
	SET answered_at = COALESCE(NULLIF(t.refs->'decision_request'->'resolution'->>'at','')::timestamptz, dr.created_at),
	    answered_by = COALESCE(t.refs->'decision_request'->'resolution'->>'by',''),
	    answer_text = COALESCE(t.refs->'decision_request'->'resolution'->>'text','')
	FROM tasks AS t
	WHERE t.id = dr.task_id
	  AND dr.kind = 'QUESTION'
	  AND dr.answered_at IS NULL
	  AND COALESCE(t.refs->'decision_request'->>'status','') IN ('answered','default_applied','withdrawn')
	  AND dr.id = (
		SELECT dr2.id
		FROM dots_results dr2
		JOIN task_events e ON e.task_id = dr2.task_id AND e.source = 'dots-hk-chatgpt' AND e.field = 'dots_reply' AND e.source_ref = dr2.comment_id AND e.from_value = 'QUESTION'
		JOIN task_comments c ON c.task_id = dr2.task_id AND c.id::text = e.to_value
		WHERE dr2.task_id = t.id
		  AND dr2.kind = 'QUESTION'
		  AND dr2.answered_at IS NULL
		  AND c.body = t.refs->'decision_request'->>'question'
		  AND c.created_at <= COALESCE(NULLIF(t.refs->'decision_request'->>'requested_at','')::timestamptz, dr2.created_at)
		ORDER BY c.created_at DESC, dr2.id DESC
		LIMIT 1)`,
}
