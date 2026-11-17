package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A dots QUESTION reply is open until the request's requester lane answers it
// (hk 5000, operator decision 364). The answer is the task comment recorded by
// AnswerDotsQuestion; dots_results carries the mark so dots_inbox and dots
// results can tell an open question from an answered one. Callers hold the
// level-2 task lock and the task row; the row update adds no lock of its own.

// dotsQuestion is one unanswered QUESTION reply: its dots_results row, the
// task comment that holds the question, and that comment's body.
type dotsQuestion struct {
	ResultID  int64
	CommentID int64
	Body      string
}

// dotsOpenQuestionsTx returns the request's unanswered QUESTION replies,
// oldest first. The dots_results row is matched to its task comment through
// the reply event RecordDotsResultTx wrote: both carry the dots comment hash.
func dotsOpenQuestionsTx(ctx context.Context, tx pgx.Tx, taskID int64) ([]dotsQuestion, error) {
	rows, err := tx.Query(ctx, `SELECT dr.id,c.id,c.body
		FROM dots_results dr
		JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
		JOIN task_comments c ON c.task_id=dr.task_id AND c.id::text=e.to_value
		WHERE dr.task_id=$1 AND dr.kind='QUESTION' AND dr.answered_at IS NULL
		ORDER BY dr.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dotsQuestion
	for rows.Next() {
		var q dotsQuestion
		if err := rows.Scan(&q.ResultID, &q.CommentID, &q.Body); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// markDotsQuestionAnsweredTx closes one open QUESTION row. It refuses when the
// row is already answered, so two answers can never both land.
func markDotsQuestionAnsweredTx(ctx context.Context, tx pgx.Tx, resultID int64, answeredBy string, answerCommentID int64, at time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE dots_results SET answered_at=$2,answered_by=$3,answer_comment_id=NULLIF($4,0) WHERE id=$1 AND answered_at IS NULL`, resultID, at, answeredBy, answerCommentID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrNoOpenDotsQuestion
	}
	return nil
}

// markDotsQuestionFromDecisionTx closes the QUESTION a decision request came
// from. The comment id recorded at ask_operator time names the exact row; a
// request recorded before that link existed falls back to the reply body.
// answeredBy is the decision's decider and answerCommentID the answer comment
// when this is dots answer's own path (0 on the console/MCP path). It reports
// whether a row was closed.
func markDotsQuestionFromDecisionTx(ctx context.Context, tx pgx.Tx, taskID, questionCommentID int64, question, answeredBy, answerText string, answerCommentID int64, at time.Time) (bool, error) {
	var tag pgconn.CommandTag
	var err error
	if questionCommentID > 0 {
		tag, err = tx.Exec(ctx, `UPDATE dots_results SET answered_at=$3,answered_by=$4,answer_comment_id=NULLIF($5,0),answer_text=NULLIF($6,'')
			WHERE id=(
				SELECT dr.id
				FROM dots_results dr
				JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
				WHERE dr.task_id=$1 AND dr.kind='QUESTION' AND dr.answered_at IS NULL AND e.to_value=$2::text
				LIMIT 1)
			AND answered_at IS NULL`, taskID, questionCommentID, at, answeredBy, answerCommentID, answerText)
	} else {
		tag, err = tx.Exec(ctx, `UPDATE dots_results SET answered_at=$2,answered_by=$3,answer_comment_id=NULLIF($5,0),answer_text=NULLIF($6,'')
			WHERE id=(
				SELECT dr.id
				FROM dots_results dr
				JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
				JOIN task_comments c ON c.task_id=dr.task_id AND c.id::text=e.to_value
				WHERE dr.task_id=$1 AND dr.kind='QUESTION' AND dr.answered_at IS NULL AND c.body=$4
				ORDER BY dr.id DESC LIMIT 1)
			AND answered_at IS NULL`, taskID, at, answeredBy, question, answerCommentID, answerText)
	}
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
