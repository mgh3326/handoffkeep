package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The dialect seams of the dots write path (hk 5088). The statements the
// PostgreSQL path ran inline in recordDotsResultTx live here with their MySQL
// text, because MySQL has no ON CONFLICT ... WHERE and no RETURNING.

// insertDotsDocumentTx inserts the result document and reports whether this
// call wrote it. PostgreSQL: ON CONFLICT(key) DO NOTHING. MySQL: a plain
// insert, since ON DUPLICATE KEY UPDATE cannot tell an existing row from a new
// one under ClientFoundRows; the key is the table's only unique key besides
// the id, so a unique violation is the existing document.
func insertDotsDocumentTx(ctx context.Context, tx transaction, key, body, sha string, createdBy string, now time.Time, taskID *int64) (bool, error) {
	if dialectFor(tx) == mysqlDialect {
		_, err := tx.Exec(ctx, "INSERT INTO documents(`key`,kind,session,job,body,sha256,created_by,created_at,updated_at,origin_class,task_id) VALUES(?,'note',?,'',?,?,?,?,?,'import',?)",
			key, DotsLane, body, sha, createdBy, now, now, taskID)
		if errors.Is(err, errUniqueViolation) {
			return false, nil
		}
		return err == nil, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO documents(key,kind,session,job,body,sha256,created_by,created_at,updated_at,origin_class,task_id) VALUES($1,'note',$2,'',$3,$4,$5,$6,$6,'import',$7) ON CONFLICT(key) DO NOTHING`, key, DotsLane, body, sha, createdBy, now, taskID)
	return tag == 1, documentBodyWriteError(err)
}

// upsertDotsResultTx records a dots result row. A row that exists for the
// comment only gains its task when it had none. MySQL reads the row first:
// dots_results has two unique keys (document_key, comment_id), so ON
// DUPLICATE KEY UPDATE could fire on the wrong one, and the PostgreSQL
// conflict target is the comment id alone. The caller holds the task-mutation
// lock, so the read and the write cannot interleave with another reply.
func upsertDotsResultTx(ctx context.Context, tx transaction, key, commentID string, issue DotsIssue, kind string, verdict *string, artefactSHA string, taskID *int64, lane string, askOperator *bool, now time.Time) error {
	if dialectFor(tx) != mysqlDialect {
		_, err := tx.Exec(ctx, `INSERT INTO dots_results(document_key,comment_id,issue_uuid,issue_identifier,kind,verdict,artefact_sha256,task_id,routed_lane,ask_operator,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)
		ON CONFLICT(comment_id) DO UPDATE SET task_id=EXCLUDED.task_id, updated_at=EXCLUDED.updated_at
		WHERE dots_results.task_id IS NULL AND EXCLUDED.task_id IS NOT NULL`,
			key, commentID, issue.IssueUUID, issue.Identifier, kind, verdict, artefactSHA, taskID, lane, askOperator, now)
		return err
	}
	var id int64
	var existing *int64
	err := tx.QueryRow(ctx, `SELECT id,task_id FROM dots_results WHERE comment_id=? FOR UPDATE`, commentID).Scan(&id, &existing)
	if errors.Is(err, ErrNoRows) {
		_, err = tx.Exec(ctx, `INSERT INTO dots_results(document_key,comment_id,issue_uuid,issue_identifier,kind,verdict,artefact_sha256,task_id,routed_lane,ask_operator,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
			key, commentID, issue.IssueUUID, issue.Identifier, kind, verdict, artefactSHA, taskID, lane, askOperator, now, now)
		return err
	}
	if err != nil {
		return err
	}
	if existing == nil && taskID != nil {
		_, err = tx.Exec(ctx, `UPDATE dots_results SET task_id=?,updated_at=? WHERE id=?`, *taskID, now, id)
	}
	return err
}

// checkDecisionAttachmentsExistMySQL is checkDecisionAttachmentsExist for a
// MySQL transaction: it refuses the first content address no attachments row
// holds.
func checkDecisionAttachmentsExistMySQL(ctx context.Context, tx transaction, options DecisionOptions) error {
	shas := []string{}
	seen := map[string]bool{}
	for _, option := range options.Options {
		for _, attachment := range option.Attachments {
			if !seen[attachment.SHA256] {
				seen[attachment.SHA256] = true
				shas = append(shas, attachment.SHA256)
			}
		}
	}
	if len(shas) == 0 {
		return nil
	}
	args := make([]any, len(shas))
	for i, sha := range shas {
		args[i] = sha
	}
	rs, err := tx.Query(ctx, `SELECT sha256 FROM attachments WHERE sha256 IN (`+placeholders(len(shas))+`)`, args...)
	if err != nil {
		return err
	}
	defer rs.Close()
	found := map[string]bool{}
	for rs.Next() {
		var sha string
		if err := rs.Scan(&sha); err != nil {
			return err
		}
		found[sha] = true
	}
	if err := rs.Err(); err != nil {
		return err
	}
	for _, sha := range shas {
		if !found[sha] {
			return fmt.Errorf("%w: %s", ErrDecisionAttachmentNotFound, sha)
		}
	}
	return nil
}

// insertDotsAnswerCommentTx inserts the answer comment and reads it back.
// PostgreSQL returns the row from the insert; MySQL reads it by the new id in
// the same transaction.
func insertDotsAnswerCommentTx(ctx context.Context, tx transaction, comment *TaskComment, taskID int64, body, author string, now time.Time) error {
	if dialectFor(tx) != mysqlDialect {
		return scanTaskComment(tx.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,$3,$4) RETURNING `+taskCommentColumns, taskID, body, author, now), comment)
	}
	id, err := tx.InsertID(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES(?,?,?,?)`, taskID, body, author, now)
	if err != nil {
		return err
	}
	return scanTaskComment(tx.QueryRow(ctx, `SELECT `+taskCommentColumns+` FROM task_comments WHERE id=?`, id), comment)
}

// markDotsChoiceResultFlowTx is markDotsChoiceResultTx on either dialect.
func markDotsChoiceResultFlowTx(ctx context.Context, tx transaction, taskID int64, docKey string) error {
	if dialectFor(tx) != mysqlDialect {
		return markDotsChoiceResultTx(ctx, pgxTx(tx), taskID, docKey)
	}
	_, err := tx.Exec(ctx, `UPDATE tasks SET refs=JSON_SET(refs,'$.dots_choice.result_doc',?) WHERE id=?`, docKey, taskID)
	return err
}

// The helpers below are the dots_questions.go and classify helpers on either
// dialect: PostgreSQL runs the original, MySQL its own text. The question
// comment is joined through the reply event, whose to_value is the comment id
// as text; the cast is pinned to the table collation so the comparison never
// mixes collations.

const dotsCommentIDText = `CAST(c.id AS CHAR) COLLATE utf8mb4_0900_bin`

func dotsPlainQuestionsFlowTx(ctx context.Context, tx transaction, taskID int64) ([]dotsQuestion, error) {
	if dialectFor(tx) != mysqlDialect {
		return dotsPlainQuestionsTx(ctx, pgxTx(tx), taskID)
	}
	rs, err := tx.Query(ctx, `SELECT dr.id,c.id,c.body,c.created_at
		FROM dots_results dr
		JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
		JOIN task_comments c ON c.task_id=dr.task_id AND `+dotsCommentIDText+`=e.to_value
		WHERE dr.task_id=? AND dr.kind='QUESTION' AND dr.ask_operator=false AND dr.answered_at IS NULL
		ORDER BY dr.id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []dotsQuestion
	for rs.Next() {
		var q dotsQuestion
		if err := rs.Scan(&q.ResultID, &q.CommentID, &q.Body, &q.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rs.Err()
}

func dotsQuestionTargetFlowTx(ctx context.Context, tx transaction, taskID, commentID int64) (dotsQuestion, bool, error) {
	if dialectFor(tx) != mysqlDialect {
		return dotsQuestionTargetTx(ctx, pgxTx(tx), taskID, commentID)
	}
	var q dotsQuestion
	err := tx.QueryRow(ctx, `SELECT dr.id,c.id,c.body,c.created_at,COALESCE(dr.ask_operator,true)
		FROM dots_results dr
		JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
		JOIN task_comments c ON c.task_id=dr.task_id AND `+dotsCommentIDText+`=e.to_value
		WHERE dr.task_id=? AND dr.kind='QUESTION' AND dr.answered_at IS NULL AND c.id=?
		ORDER BY dr.id LIMIT 1`, taskID, commentID).Scan(&q.ResultID, &q.CommentID, &q.Body, &q.CreatedAt, &q.AskOperator)
	if errors.Is(err, ErrNoRows) {
		return dotsQuestion{}, false, nil
	}
	if err != nil {
		return dotsQuestion{}, false, err
	}
	return q, true, nil
}

func dotsLatestQuestionAskOperatorFlowTx(ctx context.Context, tx transaction, taskID int64) (ask bool, found bool, err error) {
	if dialectFor(tx) != mysqlDialect {
		return dotsLatestQuestionAskOperatorTx(ctx, pgxTx(tx), taskID)
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(ask_operator,true) FROM dots_results WHERE task_id=? AND kind='QUESTION' ORDER BY id DESC LIMIT 1`, taskID).Scan(&ask)
	if errors.Is(err, ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return ask, true, nil
}

func markDotsQuestionAnsweredFlowTx(ctx context.Context, tx transaction, resultID int64, answeredBy string, answerCommentID int64, at time.Time) error {
	if dialectFor(tx) != mysqlDialect {
		return markDotsQuestionAnsweredTx(ctx, pgxTx(tx), resultID, answeredBy, answerCommentID, at)
	}
	n, err := tx.Exec(ctx, `UPDATE dots_results SET answered_at=?,answered_by=?,answer_comment_id=NULLIF(?,0) WHERE id=? AND answered_at IS NULL`, at, answeredBy, answerCommentID, resultID)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNoOpenDotsQuestion
	}
	return nil
}

func classifyDotsQuestionsFlowTx(ctx context.Context, tx transaction, taskID int64) error {
	if dialectFor(tx) != mysqlDialect {
		return classifyDotsQuestionsTx(ctx, pgxTx(tx), taskID)
	}
	rs, err := tx.Query(ctx, `SELECT dr.id,COALESCE(c.body,''),COALESCE(c.author,''),COALESCE(e.note,'')
		FROM dots_results dr
		LEFT JOIN task_events e ON e.task_id=dr.task_id AND e.source='dots-hk-chatgpt' AND e.field='dots_reply' AND e.source_ref=dr.comment_id AND e.from_value='QUESTION'
		LEFT JOIN task_comments c ON c.task_id=dr.task_id AND `+dotsCommentIDText+`=e.to_value
		WHERE dr.kind='QUESTION' AND dr.ask_operator IS NULL AND (?=0 OR dr.task_id=?)`, taskID, taskID)
	if err != nil {
		return err
	}
	type classified struct {
		id  int64
		ask bool
	}
	var updates []classified
	for rs.Next() {
		var id int64
		var body, author, note string
		if err := rs.Scan(&id, &body, &author, &note); err != nil {
			rs.Close()
			return err
		}
		ask := true
		if hash, _, ok := strings.Cut(note, " "); ok && body != "" && author != "" {
			ask = !dotsReplyPayloadMatches(hash, body, false, author)
		}
		updates = append(updates, classified{id: id, ask: ask})
	}
	if err := rs.Err(); err != nil {
		rs.Close()
		return err
	}
	rs.Close()
	for _, u := range updates {
		if _, err := tx.Exec(ctx, `UPDATE dots_results SET ask_operator=? WHERE id=? AND ask_operator IS NULL`, u.ask, u.id); err != nil {
			return err
		}
	}
	return nil
}
