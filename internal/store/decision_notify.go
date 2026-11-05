package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Every decision request gets one Discord message (docs/decision-discord.md).
// The transaction that opens a decision_requests row inserts its
// decision_notify row (openDecisionTx), so a post is owed exactly when the
// request exists. Closing a request writes nothing here: the answer, supersede
// or expiry is already committed on the request row, and a request whose
// status differs from the decision_notify row's last_status is an edit the
// worker owes. Expiry has no transaction of its own (DecisionStatusExpired is
// derived from expires_at), so this is also how an expiry reaches the channel.
//
// last_status is what the channel shows: "" before the first post, then the
// request's status. unsent and failed are final: unsent means the webhook was
// not configured when the message was due, failed that sending gave up.
//
// Requests from source linear are not queued: the Linear flow posts them
// (RecordLinearDecisionIssue). A request asked through Linear first opens a
// task_request row, which RecordLinearDecisionIssue supersedes moments later;
// the first post waits DecisionNoticePostDelay, and a request that closed
// before its first post is never posted, so such a request gets the Linear
// flow's message only.
//
// Lock order (lockorder.go): a writer only inserts a new decision_notify key.
// The worker locks one decision_notify row FOR UPDATE SKIP LOCKED and holds it
// while it sends, as ProcessNextLinearDecisionOp does; it reads the request
// and its task without locking them, and nothing else locks an existing
// decision_notify row. decision_notify_digest is written by the worker only.

const (
	DecisionNoticeUnsent = "unsent"
	DecisionNoticeFailed = "failed"

	// DecisionNoticePostDelay is how long a new request waits before its first
	// post. It lets the Linear ask path supersede its task_request row first.
	DecisionNoticePostDelay = time.Minute

	// DecisionDigestAge is how long a request has been open before the daily
	// digest lists it.
	DecisionDigestAge = 24 * time.Hour
	// DecisionDigestMaxItems bounds what the digest reads; the message fits
	// fewer and says how many it left out.
	DecisionDigestMaxItems = 50

	// About a day of retries with the shared backoff (30s doubling to an
	// hour), which outlasts a Discord outage.
	decisionNoticeMaxAttempts = 32
)

// ErrDecisionNoticeSkipped is what a sender returns when the webhook is not
// configured. The row settles as unsent, so configuring the webhook later
// does not replay old messages.
var ErrDecisionNoticeSkipped = errors.New("decision_notice_skipped")

// ErrDecisionNoticeRejected marks a failure that retrying cannot fix, such as
// Discord refusing the request or the message having been deleted. The row
// settles as failed at once.
var ErrDecisionNoticeRejected = errors.New("decision_notice_rejected")

// ErrDecisionNoticeGaveUp marks the failure that settled a row as failed, so
// the worker can log it apart from one that will be retried.
var ErrDecisionNoticeGaveUp = errors.New("decision_notice_gave_up")

// DecisionNotice is one request whose channel message is behind it. Record
// carries the status the message must show now (expired included).
type DecisionNotice struct {
	Record     DecisionRecord
	TaskTitle  string
	MessageID  string
	LastStatus string
	Attempts   int
}

// Post reports whether the notice is the first message rather than an edit.
func (n DecisionNotice) Post() bool { return n.LastStatus == "" }

// DecisionDigest is the day's list of requests open longer than
// DecisionDigestAge, oldest first. Total counts them all; Items holds at most
// DecisionDigestMaxItems.
type DecisionDigest struct {
	Day   time.Time
	Items []DecisionNotice
	Total int
}

// queueDecisionNoticeTx owes the channel a post for a request just opened.
func queueDecisionNoticeTx(ctx context.Context, tx pgx.Tx, requestID int64, source string, at time.Time) error {
	if source == DecisionSourceLinear {
		return nil
	}
	at = at.UTC()
	_, err := tx.Exec(ctx, `INSERT INTO decision_notify(request_id,next_attempt_at,created_at) VALUES($1,$2,$3) ON CONFLICT(request_id) DO NOTHING`,
		requestID, at.Add(DecisionNoticePostDelay), at)
	return err
}

// decisionEffectiveStatusSQL is scanDecisionRecord's status in SQL, for the
// request row aliased d and the clock in $1.
const decisionEffectiveStatusSQL = `CASE WHEN d.status='open' AND d.expires_at<=$1::timestamptz THEN 'expired' ELSE d.status END`

var decisionRecordColumnsD = "d." + strings.ReplaceAll(decisionRecordColumns, ",", ",d.")

func scanDecisionNoticeRow(row pgx.Row, x *DecisionNotice, now time.Time, extra ...any) error {
	var options []byte
	r := &x.Record
	dest := []any{&r.ID, &r.Source, &r.SourceRef, &r.TaskID, &r.JobID, &r.Lane, &r.Question, &options, &r.Recommended, &r.DefaultAction,
		&r.DueAt, &r.SnoozedUntil, &r.ExpiresAt, &r.Status, &r.Answer, &r.AnsweredBy, &r.AnsweredAt, &r.SupersededBy, &r.CreatedAt, &r.UpdatedAt, &x.TaskTitle}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	// Options are not part of a channel message; status, overdue and snooze
	// are derived as scanDecisionRecord derives them.
	if r.Status == DecisionStatusOpen {
		if r.ExpiresAt != nil && !now.Before(*r.ExpiresAt) {
			r.Status = DecisionStatusExpired
		}
		r.Overdue = r.DueAt != nil && !now.Before(*r.DueAt)
		r.Snoozed = r.SnoozedUntil != nil && now.Before(*r.SnoozedUntil)
	}
	return nil
}

// ProcessNextDecisionNotice runs the next due notice through send and records
// the result. send returns the message id of a first post. A request that
// closed before its first post is settled without calling send, and so is a
// later change to a request that never got a message. A failure
// retries with backoff until the attempts run out, then the row stays failed.
// found is false when nothing is due.
func (s *Store) ProcessNextDecisionNotice(ctx context.Context, now time.Time, send func(context.Context, DecisionNotice) (string, error)) (found bool, sendErr error, err error) {
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback(ctx)
	var x DecisionNotice
	err = scanDecisionNoticeRow(tx.QueryRow(ctx, `SELECT `+decisionRecordColumnsD+`,coalesce(t.title,''),n.message_id,n.last_status,n.attempts
		FROM decision_notify n JOIN decision_requests d ON d.id=n.request_id LEFT JOIN tasks t ON t.id=d.task_id
		WHERE n.last_status IN ('','open','expired') AND n.next_attempt_at<=$1::timestamptz
		AND (n.last_status='' OR n.last_status<>`+decisionEffectiveStatusSQL+`)
		ORDER BY n.next_attempt_at ASC,n.request_id ASC LIMIT 1 FOR UPDATE OF n SKIP LOCKED`, now), &x, now, &x.MessageID, &x.LastStatus, &x.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	want := x.Record.Status
	// A request that closed before its first post is never posted, and one
	// that was never posted has no message to edit.
	if (x.Post() && want != DecisionStatusOpen) || (!x.Post() && x.MessageID == "") {
		if _, err = tx.Exec(ctx, `UPDATE decision_notify SET last_status=$2 WHERE request_id=$1`, x.Record.ID, want); err != nil {
			return true, nil, err
		}
		return true, nil, tx.Commit(ctx)
	}
	messageID, sendErr := send(ctx, x)
	switch {
	case errors.Is(sendErr, ErrDecisionNoticeSkipped):
		sendErr = nil
		_, err = tx.Exec(ctx, `UPDATE decision_notify SET last_status='unsent',last_error='' WHERE request_id=$1`, x.Record.ID)
	case sendErr == nil && x.Post():
		_, err = tx.Exec(ctx, `UPDATE decision_notify SET last_status=$2,message_id=$3,posted_at=$4,attempts=0,last_error='',next_attempt_at=$4 WHERE request_id=$1`,
			x.Record.ID, want, messageID, now)
	case sendErr == nil:
		_, err = tx.Exec(ctx, `UPDATE decision_notify SET last_status=$2,edited_at=$3,attempts=0,last_error='',next_attempt_at=$3 WHERE request_id=$1`,
			x.Record.ID, want, now)
	default:
		attempts := x.Attempts + 1
		lastError := firstBytes(sendErr.Error(), linearDecisionErrorMaxBytes)
		if attempts >= decisionNoticeMaxAttempts || errors.Is(sendErr, ErrDecisionNoticeRejected) {
			sendErr = fmt.Errorf("%w: request %d: %w", ErrDecisionNoticeGaveUp, x.Record.ID, sendErr)
			_, err = tx.Exec(ctx, `UPDATE decision_notify SET last_status='failed',attempts=$2,last_error=$3 WHERE request_id=$1`, x.Record.ID, attempts, lastError)
		} else {
			_, err = tx.Exec(ctx, `UPDATE decision_notify SET attempts=$2,last_error=$3,next_attempt_at=$4 WHERE request_id=$1`,
				x.Record.ID, attempts, lastError, now.Add(linearDecisionOpBackoff(x.Attempts)))
		}
	}
	if err != nil {
		return true, sendErr, err
	}
	return true, sendErr, tx.Commit(ctx)
}

// DecisionNoticeState is a decision_notify row as stored.
type DecisionNoticeState struct {
	RequestID  int64      `json:"request_id"`
	MessageID  string     `json:"message_id"`
	PostedAt   *time.Time `json:"posted_at"`
	LastStatus string     `json:"last_status"`
	EditedAt   *time.Time `json:"edited_at"`
	Attempts   int        `json:"attempts"`
	LastError  string     `json:"last_error"`
}

// GetDecisionNotice reads the channel state of one request.
func (s *Store) GetDecisionNotice(ctx context.Context, requestID int64) (DecisionNoticeState, bool, error) {
	var x DecisionNoticeState
	err := s.pool.QueryRow(ctx, `SELECT request_id,message_id,posted_at,last_status,edited_at,attempts,last_error FROM decision_notify WHERE request_id=$1`, requestID).
		Scan(&x.RequestID, &x.MessageID, &x.PostedAt, &x.LastStatus, &x.EditedAt, &x.Attempts, &x.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionNoticeState{}, false, nil
	}
	return x, err == nil, err
}

// ProcessDecisionDigest sends the digest of now's UTC day through send, once:
// the first call of the day claims the day's row, and a call after it was
// sent, found empty or given up finds nothing to do. A day with no request
// open longer than DecisionDigestAge sends nothing. found is false when there
// was nothing to do.
func (s *Store) ProcessDecisionDigest(ctx context.Context, now time.Time, send func(context.Context, DecisionDigest) (string, error)) (found bool, sendErr error, err error) {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `INSERT INTO decision_notify_digest(day,state,next_attempt_at,created_at) VALUES($1::date,'pending',$2,$2) ON CONFLICT(day) DO NOTHING`, day, now); err != nil {
		return false, nil, err
	}
	var state string
	var attempts int
	var next time.Time
	err = tx.QueryRow(ctx, `SELECT state,attempts,next_attempt_at FROM decision_notify_digest WHERE day=$1::date FOR UPDATE SKIP LOCKED`, day).Scan(&state, &attempts, &next)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if state != "pending" || next.After(now) {
		return false, nil, nil
	}
	digest := DecisionDigest{Day: day}
	const stale = `d.status='open' AND (d.expires_at IS NULL OR d.expires_at>$1::timestamptz)
		AND (d.snoozed_until IS NULL OR d.snoozed_until<=$1::timestamptz) AND d.created_at<=$2::timestamptz`
	cutoff := now.Add(-DecisionDigestAge)
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM decision_requests d WHERE `+stale, now, cutoff).Scan(&digest.Total); err != nil {
		return false, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+decisionRecordColumnsD+`,coalesce(t.title,'') FROM decision_requests d LEFT JOIN tasks t ON t.id=d.task_id
		WHERE `+stale+` ORDER BY d.created_at ASC,d.id ASC LIMIT $3`, now, cutoff, DecisionDigestMaxItems)
	if err != nil {
		return false, nil, err
	}
	for rows.Next() {
		var x DecisionNotice
		if err := scanDecisionNoticeRow(rows, &x, now); err != nil {
			rows.Close()
			return false, nil, err
		}
		digest.Items = append(digest.Items, x)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, nil, err
	}
	if digest.Total == 0 {
		if _, err = tx.Exec(ctx, `UPDATE decision_notify_digest SET state='empty',sent_at=$2 WHERE day=$1::date`, day, now); err != nil {
			return true, nil, err
		}
		return true, nil, tx.Commit(ctx)
	}
	messageID, sendErr := send(ctx, digest)
	switch {
	case errors.Is(sendErr, ErrDecisionNoticeSkipped):
		sendErr = nil
		_, err = tx.Exec(ctx, `UPDATE decision_notify_digest SET state='unsent',listed=$2 WHERE day=$1::date`, day, digest.Total)
	case sendErr == nil:
		_, err = tx.Exec(ctx, `UPDATE decision_notify_digest SET state='sent',listed=$2,message_id=$3,sent_at=$4,attempts=$5,last_error='' WHERE day=$1::date`,
			day, digest.Total, messageID, now, attempts+1)
	default:
		lastError := firstBytes(sendErr.Error(), linearDecisionErrorMaxBytes)
		if attempts+1 >= decisionNoticeMaxAttempts || errors.Is(sendErr, ErrDecisionNoticeRejected) {
			sendErr = fmt.Errorf("%w: digest %s: %w", ErrDecisionNoticeGaveUp, day.Format(time.DateOnly), sendErr)
			_, err = tx.Exec(ctx, `UPDATE decision_notify_digest SET state='failed',attempts=$2,last_error=$3 WHERE day=$1::date`, day, attempts+1, lastError)
		} else {
			_, err = tx.Exec(ctx, `UPDATE decision_notify_digest SET attempts=$2,last_error=$3,next_attempt_at=$4 WHERE day=$1::date`,
				day, attempts+1, lastError, now.Add(linearDecisionOpBackoff(attempts)))
		}
	}
	if err != nil {
		return true, sendErr, err
	}
	return true, sendErr, tx.Commit(ctx)
}

// DecisionDigestState is the stored state of one day's digest.
type DecisionDigestState struct {
	State     string `json:"state"`
	Listed    int    `json:"listed"`
	MessageID string `json:"message_id"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error"`
}

// GetDecisionDigest reads the digest row of one UTC day.
func (s *Store) GetDecisionDigest(ctx context.Context, day time.Time) (DecisionDigestState, bool, error) {
	var x DecisionDigestState
	day = day.UTC()
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	err := s.pool.QueryRow(ctx, `SELECT state,listed,message_id,attempts,last_error FROM decision_notify_digest WHERE day=$1::date`, day).
		Scan(&x.State, &x.Listed, &x.MessageID, &x.Attempts, &x.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionDigestState{}, false, nil
	}
	return x, err == nil, err
}
