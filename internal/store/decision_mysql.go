package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQL support of the decisions files (hk 5088): decision_records,
// decision_request, decision_resolve and decision_view. Each *Flow helper runs
// the PostgreSQL helper it names on PostgreSQL and its own MySQL text on MySQL,
// so the callers hold one transaction function for both dialects.

// recordDecisionRequestMySQL is RecordDecisionRequest's transaction on MySQL.
func (s *Store) recordDecisionRequestMySQL(ctx context.Context, taskID int64, by string, in DecisionRequestInput) (DecisionRequestResult, error) {
	var result DecisionRequestResult
	err := s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if result, err = s.recordDecisionRequestFlowTx(ctx, tx, taskID, by, in); err != nil {
			return err
		}
		if result.Duplicate {
			return nil
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return DecisionRequestResult{}, err
	}
	return result, nil
}

// recordMCPDecisionRequestMySQL is RecordMCPDecisionRequest on MySQL: the key
// lock, the replay check, the request and the audit row share one transaction,
// as on PostgreSQL. The caller has validated the write and the input.
func (s *Store) recordMCPDecisionRequestMySQL(ctx context.Context, taskID int64, in DecisionRequestInput, w MCPWrite) (DecisionRequestResult, bool, error) {
	var result DecisionRequestResult
	var replayed bool
	err := s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		// The key lock is taken before any task row, as on PostgreSQL.
		if _, err = mysqlTransactionLock(ctx, tx, mysqlLockKey("mcp_write", w.Tool+"\x00"+w.Actor+"\x00"+w.Key), false); err != nil {
			return err
		}
		var recorded DecisionRequestResult
		if replayed, err = replayMCPWrite(ctx, tx, w, &recorded); err != nil {
			return err
		}
		if replayed {
			if recorded.Task.ID != taskID {
				return ErrIdempotencyKeyReused
			}
			recorded.Duplicate = true
			result = recorded
			return tx.Commit(ctx)
		}
		if result, err = s.recordDecisionRequestFlowTx(ctx, tx, taskID, w.Author, in); err != nil {
			return err
		}
		if err = insertMCPWriteAudit(ctx, tx, w, taskID, result, time.Now().UTC()); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return DecisionRequestResult{}, false, err
	}
	return result, replayed, nil
}

// recordSessionDecisionMySQL is RecordSessionDecision's transaction on MySQL;
// the caller has validated the input. decidedAt is already truncated to the
// column's microseconds and grounds is the JSON of in.Grounds.
func (s *Store) recordSessionDecisionMySQL(ctx context.Context, in SessionDecisionInput, now time.Time, audit *MCPWrite, ref string, grounds []byte) (SessionDecisionResult, error) {
	var result SessionDecisionResult
	err := s.retryTx(ctx, func() error {
		tx, release, err := s.begin(ctx)
		if err != nil {
			return err
		}
		defer release()
		defer tx.Rollback(ctx)
		if audit != nil {
			if _, err = mysqlTransactionLock(ctx, tx, mysqlLockKey("mcp_write", audit.Tool+"\x00"+audit.Actor+"\x00"+audit.Key), false); err != nil {
				return err
			}
			var recorded SessionDecisionResult
			replayed, err := replayMCPWrite(ctx, tx, *audit, &recorded)
			if err != nil {
				return err
			}
			if replayed {
				if !sessionDecisionMatches(recorded.Record, in) {
					return ErrIdempotencyKeyReused
				}
				recorded.Duplicate = true
				result = recorded
				return tx.Commit(ctx)
			}
		}
		id, err := tx.InsertID(ctx, `INSERT INTO decision_requests(source,source_ref,lane,question,reading,grounds,status,answer,answered_by,answered_at,created_at,updated_at)
			VALUES(?,?,?,?,?,?,'answered',?,?,?,?,?)`,
			DecisionSourceSessionChat, ref, in.Lane, in.Reading, in.Reading, string(grounds), in.Quote, in.By, in.DecidedAt, now, now)
		if errors.Is(err, errUniqueViolation) {
			var existing DecisionRecord
			if err := scanDecisionRecord(tx.QueryRow(ctx, `SELECT `+decisionRecordColumns+` FROM decision_requests WHERE source=? AND source_ref=?`, DecisionSourceSessionChat, ref), &existing, now); err != nil {
				return err
			}
			if !sessionDecisionMatches(existing, in) {
				return ErrSessionDecisionConflict
			}
			result = SessionDecisionResult{Record: existing, Duplicate: true}
			if audit != nil {
				if err := insertMCPWriteAudit(ctx, tx, *audit, 0, result, now); err != nil {
					return err
				}
			}
			return tx.Commit(ctx)
		}
		if err != nil {
			return err
		}
		var record DecisionRecord
		if err := scanDecisionRecord(tx.QueryRow(ctx, `SELECT `+decisionRecordColumns+` FROM decision_requests WHERE id=?`, id), &record, now); err != nil {
			return err
		}
		result = SessionDecisionResult{Record: record}
		if audit != nil {
			if err := insertMCPWriteAudit(ctx, tx, *audit, 0, result, now); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	})
	if err != nil {
		return SessionDecisionResult{}, err
	}
	return result, nil
}

// mysqlDenyViolation maps a duplicate on the one-deny key to
// ErrDecisionAttemptDenied, as mapDriverError does for PostgreSQL's constraint
// of the same name; any other error is returned as it is.
func mysqlDenyViolation(err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == 1062 && strings.Contains(me.Message, "decision_answer_attempts_one_deny") {
		return errors.Join(err, ErrDecisionAttemptDenied)
	}
	return err
}

// otherBlockingDecisionsFlowTx is otherBlockingDecisionsTx on either
// dialect's transaction.
func otherBlockingDecisionsFlowTx(ctx context.Context, tx transaction, taskID int64, requestID string) (int, error) {
	if dialectFor(tx) != mysqlDialect {
		return otherBlockingDecisionsTx(ctx, pgxTx(tx), taskID, requestID)
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM decision_requests
		WHERE task_id=? AND status='open' AND source IN (?,?,?)
		AND NOT (source IN (?,?) AND source_ref=?)`,
		taskID, DecisionSourceTaskFlag, DecisionSourceTaskRequest, DecisionSourceLinear, DecisionSourceTaskRequest, DecisionSourceLinear, requestID).Scan(&n)
	return n, err
}

// closeDotsAskOperatorQuestionsFlowTx is closeDotsAskOperatorQuestionsTx on
// either dialect's transaction.
func (s *Store) closeDotsAskOperatorQuestionsFlowTx(ctx context.Context, tx transaction, taskID int64, answeredBy, answerText string, answerCommentID int64, at time.Time) error {
	if s.dialect != mysqlDialect {
		return closeDotsAskOperatorQuestionsTx(ctx, pgxTx(tx), taskID, answeredBy, answerText, answerCommentID, at)
	}
	_, err := tx.Exec(ctx, `UPDATE dots_results SET answered_at=?,answered_by=?,answer_comment_id=NULLIF(?,0),answer_text=NULLIF(?,'')
		WHERE task_id=? AND kind='QUESTION' AND ask_operator AND answered_at IS NULL`, at, answeredBy, answerCommentID, answerText, taskID)
	return err
}

// queueDecisionAnswerNoticeFlow is queueDecisionAnswerNotice on either
// dialect's transaction; the MySQL text is the same notice. A session answer
// recorded by the task lane's own session writes no task-lane notice, as on
// PostgreSQL (hk 6481); the decision event and audit rows are still written.
func (s *Store) queueDecisionAnswerNoticeFlow(ctx context.Context, tx transaction, x Task, resolved DecisionRequest, in ResolveDecisionInput, decider string, resolutionID int64, at time.Time) error {
	if s.dialect != mysqlDialect {
		return s.queueDecisionAnswerNotice(ctx, pgxTx(tx), x, resolved, in, decider, resolutionID, at)
	}
	taskID := strconv.FormatInt(x.ID, 10)
	prefix := "[hk] 결정 답변 " + resolved.ID + " task " + taskID + ": "
	choice := decisionAnswerNoticeChoice(x.Refs.DecisionOptions, in, resolved.Status)
	if resolved.Status != DecisionRequestAnswered && choice != resolved.Status {
		choice = resolved.Status + ": " + choice
	}
	body := choice + " by " + decider + " via " + in.Channel
	if base := s.decisionConsoleURL(); base != "" {
		body += " | " + base + "/ui/tasks/" + taskID
	}
	text := assistantLaneText(prefix, body)
	if !decisionSelfRecordedAnswer(x.Lane, in) {
		if _, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDecisionAnswered, x.Lane, decisionAnswerNoticeEventID(resolved.ID, resolutionID), text, at); err != nil {
			return err
		}
	}
	// The asker's copy, as on PostgreSQL (hk 5129).
	asker, ok := decisionAskerNoticeLane(x.Lane, resolved, in)
	if !ok {
		return nil
	}
	_, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDecisionAnswered, asker, decisionAskerAnswerNoticeEventID(resolved.ID, resolutionID), text, at)
	return err
}

// queueDecisionDenyNoticeFlow is queueDecisionDenyNotice on either dialect's
// transaction; the MySQL text is the same notice.
func (s *Store) queueDecisionDenyNoticeFlow(ctx context.Context, tx transaction, x Task, request DecisionRequest, in ResolveDecisionInput, denyAttemptID int64, at time.Time) error {
	if s.dialect != mysqlDialect {
		return s.queueDecisionDenyNotice(ctx, pgxTx(tx), x, request, in, denyAttemptID, at)
	}
	taskID := strconv.FormatInt(x.ID, 10)
	prefix := "[hk] 결정 거부 " + request.ID + " task " + taskID + ": "
	body := "attempt " + strconv.FormatInt(in.DeniesAttemptID, 10) + " denied by " + in.Actor + " via " + in.Channel + "; request stays " + request.Status
	if in.Receipt != "" {
		body += " | " + in.Receipt
	}
	if base := s.decisionConsoleURL(); base != "" {
		body += " | " + base + "/ui/tasks/" + taskID
	}
	_, err := insertNotificationOutboxOnce(ctx, tx, OutboxKindDecisionDenied, x.Lane, decisionDenyNoticeEventID(request.ID, denyAttemptID), assistantLaneText(prefix, body), at)
	return err
}
