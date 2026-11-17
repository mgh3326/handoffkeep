package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// A dots QUESTION reply is open until the request's requester lane answers it
// (hk 5000, operator decision 364). The answer is the task comment recorded by
// AnswerDotsQuestion; dots_results carries the mark so dots_inbox and dots
// results can tell an open question from an answered one. Callers hold the
// level-2 task lock and the task row; the row update adds no lock of its own.

// dotsQuestion is one unanswered QUESTION reply: its dots_results row and the
// task comment that holds the question.
type dotsQuestion struct {
	ResultID  int64
	CommentID int64
}

// dotsOpenQuestionTx returns the request's latest QUESTION reply that has no
// answer. The dots_results row is matched to its task comment through the
// reply event RecordDotsResultTx wrote: both carry the dots comment hash.
func dotsOpenQuestionTx(ctx context.Context, tx pgx.Tx, taskID int64) (dotsQuestion, bool, error) {
	var q dotsQuestion
	err := tx.QueryRow(ctx, `SELECT dr.id,c.id
		FROM dots_results dr
		JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
		JOIN task_comments c ON c.task_id=dr.task_id AND c.id::text=e.to_value
		WHERE dr.task_id=$1 AND dr.kind='QUESTION' AND dr.answered_at IS NULL
		ORDER BY dr.id DESC LIMIT 1`, taskID).Scan(&q.ResultID, &q.CommentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dotsQuestion{}, false, nil
	}
	if err != nil {
		return dotsQuestion{}, false, err
	}
	return q, true, nil
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

// markDotsQuestionAnsweredByBodyTx closes the open QUESTION row whose reply
// body is question. A decision request may be answered outside dots answer
// (the operator's console or MCP path), and the QUESTION it came from must not
// stay open; matching the body keeps an unrelated question untouched. It
// reports whether a row was closed.
func markDotsQuestionAnsweredByBodyTx(ctx context.Context, tx pgx.Tx, taskID int64, question, answeredBy string, answerCommentID int64, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE dots_results SET answered_at=$2,answered_by=$3,answer_comment_id=NULLIF($5,0)
		WHERE id=(
			SELECT dr.id
			FROM dots_results dr
			JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
			JOIN task_comments c ON c.task_id=dr.task_id AND c.id::text=e.to_value
			WHERE dr.task_id=$1 AND dr.kind='QUESTION' AND dr.answered_at IS NULL AND c.body=$4
			ORDER BY dr.id DESC LIMIT 1)
		AND answered_at IS NULL`, taskID, at, answeredBy, question, answerCommentID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
