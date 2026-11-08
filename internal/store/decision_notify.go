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
// decision_deliveries row (openDecisionTx), one per destination, so a post is
// owed exactly when the request exists. Closing a request writes nothing here:
// the answer, supersede or expiry is already committed on the request row, and
// a request whose effective status differs from the row's applied_version is
// an edit the worker owes. Expiry has no transaction of its own
// (DecisionStatusExpired is derived from expires_at), so this is also how an
// expiry reaches the channel, and why a version is the effective status
// rather than a stored counter.
//
// applied_version is what the destination shows: "" before the first post,
// then the request's effective status. desired_version is the version the
// worker last tried to apply. state is pending while the worker owns the row;
// unsent and failed are final: unsent means the webhook was not configured
// when the message was due, failed that sending gave up.
//
// Requests from source linear are not queued: the Linear flow posts them
// (RecordLinearDecisionIssue). A request asked through Linear first opens a
// task_request row, which RecordLinearDecisionIssue supersedes moments later;
// the first post waits DecisionNoticePostDelay, and a request that closed
// before its first post is never posted, so such a request gets the Linear
// flow's message only.
//
// Lock order (lockorder.go): a writer only inserts a new decision_deliveries
// key. The worker locks one decision_deliveries row FOR UPDATE SKIP LOCKED and
// holds it while it sends, as ProcessNextLinearDecisionOp does; it reads the
// request and its task without locking them, and nothing else locks an
// existing decision_deliveries row. decision_notify_digest is written by the
// worker only. The v38 decision_notify table is no longer written.

const (
	DecisionNoticeUnsent = "unsent"
	DecisionNoticeFailed = "failed"
	// DecisionDeliveryPending is a row the worker still owns.
	DecisionDeliveryPending = "pending"
	// DecisionDestinationDiscord is the decisions channel webhook, the only
	// destination so far.
	DecisionDestinationDiscord = "discord"

	// DecisionNoticePostDelay is how long a new request waits before its first
	// post. It lets the Linear ask path supersede its task_request row first.
	DecisionNoticePostDelay = time.Minute

	// DecisionNoticeNearDue is how close to its due time a request's first
	// post mentions the operator.
	DecisionNoticeNearDue = 2 * time.Hour

	// DecisionDeliveryLagAge is how long a pending row may sit past its
	// next attempt behind its request before hygiene counts it as lagging.
	DecisionDeliveryLagAge = time.Hour

	// DecisionDigestAge is how long a request has been open before the daily
	// digest lists it.
	DecisionDigestAge = 24 * time.Hour
	// DecisionDigestMaxItems bounds what the digest reads; the message fits
	// fewer and says how many it left out.
	DecisionDigestMaxItems = 50

	// About a day of retries with the shared backoff (30s doubling to an
	// hour), which outlasts a Discord outage.
	decisionNoticeMaxAttempts = 32

	// decisionErrorCodeMaxBytes bounds last_error_code.
	decisionErrorCodeMaxBytes = 100
)

// decisionSettleTimeout bounds the write that records a send's outcome.
const decisionSettleTimeout = 5 * time.Second

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
	Record         DecisionRecord
	TaskTitle      string
	MessageID      string
	AppliedVersion string
	Attempts       int
	// Mention is set on a first post only, for a request that blocks its
	// task, is human_only, or is due within DecisionNoticeNearDue.
	Mention bool
}

// Post reports whether the notice is the first message rather than an edit.
func (n DecisionNotice) Post() bool { return n.AppliedVersion == "" }

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
	_, err := tx.Exec(ctx, `INSERT INTO decision_deliveries(request_id,destination,state,next_attempt_at,created_at) VALUES($1,$2,'pending',$3,$4) ON CONFLICT(request_id,destination) DO NOTHING`,
		requestID, DecisionDestinationDiscord, at.Add(DecisionNoticePostDelay), at)
	return err
}

// decisionEffectiveStatusSQL is scanDecisionRecord's status in SQL, for the
// request row aliased d and the clock in $1.
const decisionEffectiveStatusSQL = `CASE WHEN d.status='open' AND d.expires_at<=$1::timestamptz THEN 'expired' ELSE d.status END`

// decisionMentionSQL is DecisionNotice.Mention for the delivery aliased n,
// request d and task t, with the near-due cutoff in $2: a first post of a
// request whose task awaits a decision, whose task's current structured
// request is this one and human_only, or that is due by the cutoff. A request
// without a task or due time reads NULL in those terms, so the whole is
// coalesced to false.
const decisionMentionSQL = `coalesce(n.applied_version='' AND (coalesce(t.awaiting_decision,false)
		OR (d.source='` + DecisionSourceTaskRequest + `' AND t.refs->'decision_request'->>'id'=d.source_ref AND t.refs->'decision_request'->'human_only'='true'::jsonb)
		OR d.due_at<=$2::timestamptz),false)`

var decisionRecordColumnsD = "d." + strings.ReplaceAll(decisionRecordColumns, ",", ",d.")

func scanDecisionNoticeRow(row pgx.Row, x *DecisionNotice, now time.Time, extra ...any) error {
	var options, refsRaw []byte
	r := &x.Record
	dest := []any{&r.ID, &r.Source, &r.SourceRef, &r.TaskID, &r.JobID, &r.Lane, &r.Question, &options, &r.Recommended, &r.DefaultAction,
		&r.DueAt, &r.SnoozedUntil, &r.ExpiresAt, &r.Status, &r.Answer, &r.AnsweredBy, &r.AnsweredAt, &r.SupersededBy, &r.CreatedAt, &r.UpdatedAt, &x.TaskTitle, &refsRaw}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return err
	}
	var refs TaskRefs
	if _, err := scanJSONB(refsRaw, &refs); err != nil {
		return err
	}
	if refs.DecisionRequest != nil && refs.DecisionRequest.ID == r.SourceRef && refs.DecisionRequest.Status == DecisionRequestOpen {
		if rendered, ok := RenderDecisionTemplate(*refs.DecisionRequest, refs.DecisionOptions); ok {
			r.Question = DecisionTemplateText(rendered)
		}
	}
	// Options are not part of a channel message; status, overdue and snooze
	// are derived as scanDecisionRecord derives them.
	r.deriveStatus(now)
	return nil
}

// decisionErrorCode is the stored form of a send error: its text before the
// first colon, such as "decision_notice_rejected" or "discord webhook status
// 500". The full error goes to the worker's log.
func decisionErrorCode(err error) string {
	code, _, _ := strings.Cut(err.Error(), ":")
	return firstBytes(strings.TrimSpace(code), decisionErrorCodeMaxBytes)
}

// ProcessNextDecisionNotice runs the next due delivery through send and
// records the result. A row is due when it is pending, its next attempt has
// come and applied_version differs from the request's effective status.
// send returns the message id of a first post. A request that closed before
// its first post is settled without calling send, and so is a later change to
// a request that never got a message. A failure retries with backoff until
// the attempts run out, then the row stays failed. found is false when
// nothing is due.
func (s *Store) ProcessNextDecisionNotice(ctx context.Context, now time.Time, send func(context.Context, DecisionNotice) (string, error)) (found bool, sendErr error, err error) {
	now = now.UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, nil, err
	}
	defer tx.Rollback(ctx)
	var x DecisionNotice
	err = scanDecisionNoticeRow(tx.QueryRow(ctx, `SELECT `+decisionRecordColumnsD+`,coalesce(t.title,''),t.refs,n.message_id,n.applied_version,n.attempts,`+decisionMentionSQL+`
		FROM decision_deliveries n JOIN decision_requests d ON d.id=n.request_id LEFT JOIN tasks t ON t.id=d.task_id
		WHERE n.destination='`+DecisionDestinationDiscord+`' AND n.state='pending' AND n.next_attempt_at<=$1::timestamptz
		AND n.applied_version<>`+decisionEffectiveStatusSQL+`
		ORDER BY n.next_attempt_at ASC,n.request_id ASC LIMIT 1 FOR UPDATE OF n SKIP LOCKED`, now, now.Add(DecisionNoticeNearDue)),
		&x, now, &x.MessageID, &x.AppliedVersion, &x.Attempts, &x.Mention)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	want := x.Record.Status
	const key = `WHERE request_id=$1 AND destination='` + DecisionDestinationDiscord + `'`
	// A request that closed before its first post is never posted, and one
	// that was never posted has no message to edit.
	if (x.Post() && want != DecisionStatusOpen) || (!x.Post() && x.MessageID == "") {
		if _, err = tx.Exec(ctx, `UPDATE decision_deliveries SET desired_version=$2,applied_version=$2 `+key, x.Record.ID, want); err != nil {
			return true, nil, err
		}
		return true, nil, tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `UPDATE decision_deliveries SET desired_version=$2 `+key, x.Record.ID, want); err != nil {
		return true, nil, err
	}
	messageID, sendErr := send(ctx, x)
	// send may have created the message: record the outcome even if ctx is
	// canceled now (shutdown), or the next start would post it again.
	settle, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), decisionSettleTimeout)
	defer cancelSettle()
	switch {
	case errors.Is(sendErr, ErrDecisionNoticeSkipped):
		sendErr = nil
		_, err = tx.Exec(settle, `UPDATE decision_deliveries SET state='unsent',last_error_code='' `+key, x.Record.ID)
	case sendErr == nil && x.Post():
		_, err = tx.Exec(settle, `UPDATE decision_deliveries SET applied_version=desired_version,message_id=$2,posted_at=$3,attempts=0,last_error_code='',next_attempt_at=$3 `+key,
			x.Record.ID, messageID, now)
	case sendErr == nil:
		_, err = tx.Exec(settle, `UPDATE decision_deliveries SET applied_version=desired_version,edited_at=$2,attempts=0,last_error_code='',next_attempt_at=$2 `+key,
			x.Record.ID, now)
	default:
		attempts := x.Attempts + 1
		code := decisionErrorCode(sendErr)
		if attempts >= decisionNoticeMaxAttempts || errors.Is(sendErr, ErrDecisionNoticeRejected) {
			sendErr = fmt.Errorf("%w: request %d: %w", ErrDecisionNoticeGaveUp, x.Record.ID, sendErr)
			_, err = tx.Exec(settle, `UPDATE decision_deliveries SET state='failed',attempts=$2,last_error_code=$3 `+key, x.Record.ID, attempts, code)
		} else {
			_, err = tx.Exec(settle, `UPDATE decision_deliveries SET attempts=$2,last_error_code=$3,next_attempt_at=$4 `+key,
				x.Record.ID, attempts, code, now.Add(linearDecisionOpBackoff(x.Attempts)))
		}
	}
	if err != nil {
		return true, sendErr, err
	}
	return true, sendErr, tx.Commit(settle)
}

// DecisionNoticeState is a decision_deliveries row as stored.
type DecisionNoticeState struct {
	RequestID      int64      `json:"request_id"`
	Destination    string     `json:"destination"`
	MessageID      string     `json:"message_id"`
	DesiredVersion string     `json:"desired_version"`
	AppliedVersion string     `json:"applied_version"`
	State          string     `json:"state"`
	Attempts       int        `json:"attempts"`
	LastErrorCode  string     `json:"last_error_code"`
	NextAttemptAt  time.Time  `json:"next_attempt_at"`
	PostedAt       *time.Time `json:"posted_at"`
	EditedAt       *time.Time `json:"edited_at"`
}

// GetDecisionNotice reads the Discord delivery of one request.
func (s *Store) GetDecisionNotice(ctx context.Context, requestID int64) (DecisionNoticeState, bool, error) {
	var x DecisionNoticeState
	err := s.pool.QueryRow(ctx, `SELECT request_id,destination,message_id,desired_version,applied_version,state,attempts,last_error_code,next_attempt_at,posted_at,edited_at
		FROM decision_deliveries WHERE request_id=$1 AND destination=$2`, requestID, DecisionDestinationDiscord).
		Scan(&x.RequestID, &x.Destination, &x.MessageID, &x.DesiredVersion, &x.AppliedVersion, &x.State, &x.Attempts, &x.LastErrorCode, &x.NextAttemptAt, &x.PostedAt, &x.EditedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionNoticeState{}, false, nil
	}
	return x, err == nil, err
}

// DecisionDeliveryCounts is one destination's hygiene: rows by state, and
// Lagging, the pending rows behind their request for over
// DecisionDeliveryLagAge past their next attempt.
type DecisionDeliveryCounts struct {
	Pending int `json:"pending"`
	Lagging int `json:"lagging"`
	Unsent  int `json:"unsent"`
	Failed  int `json:"failed"`
}

// DecisionDeliveryHygiene is the delivery hygiene of every destination.
type DecisionDeliveryHygiene struct {
	Destinations map[string]DecisionDeliveryCounts `json:"destinations"`
}

// DecisionDeliveryHygiene counts decision_deliveries rows per destination.
// Every known destination is present, zero when it has no row.
func (s *Store) DecisionDeliveryHygiene(ctx context.Context, now time.Time) (DecisionDeliveryHygiene, error) {
	now = now.UTC()
	out := DecisionDeliveryHygiene{Destinations: map[string]DecisionDeliveryCounts{DecisionDestinationDiscord: {}}}
	rows, err := s.pool.Query(ctx, `SELECT n.destination,
		count(*) FILTER (WHERE n.state='pending'),
		count(*) FILTER (WHERE n.state='pending' AND n.applied_version<>`+decisionEffectiveStatusSQL+` AND n.next_attempt_at<$2::timestamptz),
		count(*) FILTER (WHERE n.state='unsent'),
		count(*) FILTER (WHERE n.state='failed')
		FROM decision_deliveries n LEFT JOIN decision_requests d ON d.id=n.request_id
		GROUP BY n.destination ORDER BY n.destination`, now, now.Add(-DecisionDeliveryLagAge))
	if err != nil {
		return DecisionDeliveryHygiene{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var dest string
		var c DecisionDeliveryCounts
		if err := rows.Scan(&dest, &c.Pending, &c.Lagging, &c.Unsent, &c.Failed); err != nil {
			return DecisionDeliveryHygiene{}, err
		}
		out.Destinations[dest] = c
	}
	return out, rows.Err()
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
		AND (d.snoozed_until IS NULL OR d.snoozed_until<=$1::timestamptz) AND d.created_at<$2::timestamptz`
	cutoff := now.Add(-DecisionDigestAge)
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM decision_requests d WHERE `+stale, now, cutoff).Scan(&digest.Total); err != nil {
		return false, nil, err
	}
	rows, err := tx.Query(ctx, `SELECT `+decisionRecordColumnsD+`,coalesce(t.title,''),t.refs FROM decision_requests d LEFT JOIN tasks t ON t.id=d.task_id
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
	// send may have created the message: record the outcome even if ctx is
	// canceled now (shutdown), or the next start would post it again.
	settle, cancelSettle := context.WithTimeout(context.WithoutCancel(ctx), decisionSettleTimeout)
	defer cancelSettle()
	switch {
	case errors.Is(sendErr, ErrDecisionNoticeSkipped):
		sendErr = nil
		_, err = tx.Exec(settle, `UPDATE decision_notify_digest SET state='unsent',listed=$2 WHERE day=$1::date`, day, digest.Total)
	case sendErr == nil:
		_, err = tx.Exec(settle, `UPDATE decision_notify_digest SET state='sent',listed=$2,message_id=$3,sent_at=$4,attempts=$5,last_error='' WHERE day=$1::date`,
			day, digest.Total, messageID, now, attempts+1)
	default:
		lastError := firstBytes(sendErr.Error(), linearDecisionErrorMaxBytes)
		if attempts+1 >= decisionNoticeMaxAttempts || errors.Is(sendErr, ErrDecisionNoticeRejected) {
			sendErr = fmt.Errorf("%w: digest %s: %w", ErrDecisionNoticeGaveUp, day.Format(time.DateOnly), sendErr)
			_, err = tx.Exec(settle, `UPDATE decision_notify_digest SET state='failed',attempts=$2,last_error=$3 WHERE day=$1::date`, day, attempts+1, lastError)
		} else {
			_, err = tx.Exec(settle, `UPDATE decision_notify_digest SET attempts=$2,last_error=$3,next_attempt_at=$4 WHERE day=$1::date`,
				day, attempts+1, lastError, now.Add(linearDecisionOpBackoff(attempts)))
		}
	}
	if err != nil {
		return true, sendErr, err
	}
	return true, sendErr, tx.Commit(settle)
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
