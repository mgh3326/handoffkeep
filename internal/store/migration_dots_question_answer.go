package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const dotsQuestionAnswerMigrationID = "20261117-dots-question-answer"

func init() {
	registerMigration(migration{ID: dotsQuestionAnswerMigrationID, Apply: applyDotsQuestionAnswerMigration})
}

// applyDotsQuestionAnswerMigration records who owns each dots QUESTION reply:
// a question sent with ask_operator is answered through its decision request,
// and a plain one by the request's requester lane (hk 5000, operator decision
// 364). The owner is written when the question is asked; the columns here also
// carry the answer for display.
//
// The columns are nullable with no default, so existing rows read as empty and
// the previous release, which never reads them, keeps working. ADD COLUMN
// without a default takes ACCESS EXCLUSIVE on dots_results only for the catalog
// change, a short wait at startup.
//
// Legacy rows are classified from the reply's own recorded payload, then the
// display invariant is restored: at most the current open request's question
// stays open, and any question whose decision request is already closed is
// closed. The backfill writes only dots_results rows it matched by event and
// the task row it reads; it takes ROW EXCLUSIVE on dots_results.
func applyDotsQuestionAnswerMigration(ctx context.Context, tx pgx.Tx) error {
	if err := execAll(ctx, tx, dotsQuestionAnswerMigration); err != nil {
		return err
	}
	if err := classifyLegacyDotsQuestionsTx(ctx, tx); err != nil {
		return err
	}
	return execAll(ctx, tx, dotsQuestionAnswerInvariant)
}

var dotsQuestionAnswerMigration = []string{
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answered_at TIMESTAMPTZ`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answered_by TEXT`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answer_comment_id BIGINT`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS answer_text TEXT`,
	`ALTER TABLE dots_results ADD COLUMN IF NOT EXISTS ask_operator BOOLEAN`,
}

// classifyLegacyDotsQuestionsTx sets ask_operator on QUESTION rows recorded
// before the column existed. The dots_reply event's first note field is the
// payload hash ReplyDots writes: sha256 of
// "QUESTION\x00<body>\x00<ask_operator>\x00<author>\x00chatgpt\x00". A row that
// matches neither variant (no event, an older format, an unknown channel) is
// asked by the operator: the plain path refuses it, which is the safe side.
func classifyLegacyDotsQuestionsTx(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT dr.id,COALESCE(c.body,''),COALESCE(c.author,''),COALESCE(e.note,'')
		FROM dots_results dr
		LEFT JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
		LEFT JOIN task_comments c ON c.task_id=dr.task_id AND c.id::text=e.to_value
		WHERE dr.kind='QUESTION' AND dr.ask_operator IS NULL`)
	if err != nil {
		return err
	}
	type classified struct {
		id  int64
		ask bool
	}
	var updates []classified
	for rows.Next() {
		var id int64
		var body, author, note string
		if err := rows.Scan(&id, &body, &author, &note); err != nil {
			rows.Close()
			return err
		}
		ask := true
		if hash, _, ok := strings.Cut(note, " "); ok && body != "" && author != "" {
			ask = !dotsReplyPayloadMatches(hash, body, false, author)
		}
		updates = append(updates, classified{id: id, ask: ask})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, u := range updates {
		if _, err := tx.Exec(ctx, `UPDATE dots_results SET ask_operator=$2 WHERE id=$1 AND ask_operator IS NULL`, u.id, u.ask); err != nil {
			return err
		}
	}
	return nil
}

// dotsReplyPayloadMatches reports whether hash is the payload of a QUESTION
// reply with this body and author. The channel is fixed: ReplyDots accepts only
// chatgpt.
func dotsReplyPayloadMatches(hash, body string, askOperator bool, author string) bool {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%t\x00%s\x00%s\x00", "QUESTION", body, askOperator, author, "chatgpt")))
	return hex.EncodeToString(sum[:]) == hash
}

// dotsQuestionAnswerInvariant closes legacy ask_operator rows that cannot be
// open under the request invariant: a task with no open decision request has
// every question closed, and an open request keeps only its own latest
// matching question (a duplicate re-send closes). Plain questions are never
// touched; their answered_at is authoritative.
var dotsQuestionAnswerInvariant = []string{
	`UPDATE dots_results AS dr
	SET answered_at = COALESCE(NULLIF(t.refs->'decision_request'->'resolution'->>'at','')::timestamptz, t.updated_at),
	    answered_by = COALESCE(t.refs->'decision_request'->'resolution'->>'by',''),
	    answer_text = COALESCE(t.refs->'decision_request'->'resolution'->>'text','')
	FROM tasks AS t
	WHERE t.id = dr.task_id
	  AND dr.kind = 'QUESTION'
	  AND dr.ask_operator
	  AND dr.answered_at IS NULL
	  AND COALESCE(t.refs->'decision_request'->>'status','') <> 'open'`,
	`UPDATE dots_results AS dr
	SET answered_at = t.updated_at
	FROM tasks AS t
	WHERE t.id = dr.task_id
	  AND dr.kind = 'QUESTION'
	  AND dr.ask_operator
	  AND dr.answered_at IS NULL
	  AND COALESCE(t.refs->'decision_request'->>'status','') = 'open'
	  AND dr.id IS DISTINCT FROM (
		SELECT dr2.id
		FROM dots_results dr2
		JOIN task_events e ON e.task_id = dr2.task_id AND e.source = 'dots-hk-chatgpt' AND e.field = 'dots_reply' AND e.source_ref = dr2.comment_id AND e.from_value = 'QUESTION'
		JOIN task_comments c ON c.task_id = dr2.task_id AND c.id::text = e.to_value
		WHERE dr2.task_id = t.id
		  AND dr2.kind = 'QUESTION'
		  AND dr2.ask_operator
		  AND dr2.answered_at IS NULL
		  AND c.body = t.refs->'decision_request'->>'question'
		  AND c.created_at <= COALESCE(NULLIF(t.refs->'decision_request'->>'requested_at','')::timestamptz, c.created_at)
		ORDER BY c.created_at DESC, dr2.id DESC
		LIMIT 1)`,
}
