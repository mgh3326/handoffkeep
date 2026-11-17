package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// A dots QUESTION reply has one owner, recorded when it is asked (hk 5000,
// operator decision 364): ask_operator rows are answered through their decision
// request, plain rows by the request's requester lane. The plain path selects
// only ask_operator = false, so it can never close a question the decision path
// owns, whatever its answered_at says. dots_results also carries the answer for
// display. Callers hold the level-2 task lock and the task row.

// dotsQuestion is one open QUESTION reply: its dots_results row, the task
// comment that holds the question, that comment's body and creation time, and
// its owner.
type dotsQuestion struct {
	ResultID    int64
	CommentID   int64
	Body        string
	CreatedAt   time.Time
	AskOperator bool
}

// dotsPlainQuestionsTx returns the request's open plain QUESTION replies,
// oldest first. The dots_results row is matched to its task comment through the
// reply event RecordDotsResultTx wrote: both carry the dots comment hash.
func dotsPlainQuestionsTx(ctx context.Context, tx pgx.Tx, taskID int64) ([]dotsQuestion, error) {
	rows, err := tx.Query(ctx, `SELECT dr.id,c.id,c.body,c.created_at
		FROM dots_results dr
		JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
		JOIN task_comments c ON c.task_id=dr.task_id AND c.id::text=e.to_value
		WHERE dr.task_id=$1 AND dr.kind='QUESTION' AND dr.ask_operator=false AND dr.answered_at IS NULL
		ORDER BY dr.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []dotsQuestion
	for rows.Next() {
		var q dotsQuestion
		if err := rows.Scan(&q.ResultID, &q.CommentID, &q.Body, &q.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// dotsQuestionTargetTx returns one open QUESTION reply by its comment id (the
// reply id dots_inbox shows). Its owner decides which path answers it.
func dotsQuestionTargetTx(ctx context.Context, tx pgx.Tx, taskID, commentID int64) (dotsQuestion, bool, error) {
	var q dotsQuestion
	err := tx.QueryRow(ctx, `SELECT dr.id,c.id,c.body,c.created_at,COALESCE(dr.ask_operator,true)
		FROM dots_results dr
		JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
		JOIN task_comments c ON c.task_id=dr.task_id AND c.id::text=e.to_value
		WHERE dr.task_id=$1 AND dr.kind='QUESTION' AND dr.answered_at IS NULL AND c.id=$2
		LIMIT 1`, taskID, commentID).Scan(&q.ResultID, &q.CommentID, &q.Body, &q.CreatedAt, &q.AskOperator)
	if errors.Is(err, pgx.ErrNoRows) {
		return dotsQuestion{}, false, nil
	}
	if err != nil {
		return dotsQuestion{}, false, err
	}
	return q, true, nil
}

// dotsLatestQuestionAskOperatorTx reports the owner of the task's latest
// QUESTION reply (answered or not), which is what was last asked. It tells a
// zero-open refusal apart: a decision question already closed means
// decision_request_resolved, a plain question means nothing is open.
func dotsLatestQuestionAskOperatorTx(ctx context.Context, tx pgx.Tx, taskID int64) (ask bool, found bool, err error) {
	err = tx.QueryRow(ctx, `SELECT COALESCE(ask_operator,true) FROM dots_results WHERE task_id=$1 AND kind='QUESTION' ORDER BY id DESC LIMIT 1`, taskID).Scan(&ask)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return ask, true, nil
}

// markDotsQuestionAnsweredTx closes one open plain QUESTION row. It refuses
// when the row is already answered, so two answers can never both land.
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

// closeDotsAskOperatorQuestionsTx closes every open ask_operator QUESTION of
// the task with the decision's answer. recordDecisionRequestTx refuses a new
// request while one is open, and a superseding request closes the old request's
// rows in the same transaction, so all open ask_operator rows belong to the
// request being closed: the one that opened it and any duplicate re-send. The
// update is display only; the plain path never selects these rows.
func closeDotsAskOperatorQuestionsTx(ctx context.Context, tx pgx.Tx, taskID int64, answeredBy, answerText string, answerCommentID int64, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE dots_results SET answered_at=$2,answered_by=$3,answer_comment_id=NULLIF($4,0),answer_text=NULLIF($5,'')
		WHERE task_id=$1 AND kind='QUESTION' AND ask_operator AND answered_at IS NULL`, taskID, at, answeredBy, answerCommentID, answerText)
	return err
}
