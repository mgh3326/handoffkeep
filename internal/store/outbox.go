package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The notification outbox is hk's durable record of lane notifications owed
// for assistant-path writes (MGH-36): a decision request answered or a chat
// question answered records exactly one row in the writer's transaction, and
// the stateless drainer posts it to the hub and marks it sent here. The
// event_id is deterministic — a repeated resolution writes nothing new, and
// a drained retry re-uses the same id so hub-side dedupe applies.
const (
	OutboxKindDecisionAnswered = "decision_answered"
	OutboxKindChatAnswer       = "chat_answer"
	// The dots kinds name the lane-event type a drainer sends them with:
	// dots_report is ready (RESULT), dots_decision is decision (QUESTION),
	// dots_blocked is blocked (BLOCKED), dots_followup is info.
	OutboxKindDotsReport   = "dots_report"
	OutboxKindDotsDecision = "dots_decision"
	OutboxKindDotsBlocked  = "dots_blocked"
	OutboxKindDotsFollowup = "dots_followup"
	OutboxKindYTWriteOp    = "yt_write_op"
	OutboxKindDeployLag    = "deploy_lag"
	// The resolution kinds of hk 3295: an operator deny of a pending decision
	// attempt, a disposition (ESC) answer, and a deploy result.
	OutboxKindDecisionDenied      = "decision_denied"
	OutboxKindDispositionAnswered = "disposition_answered"
	OutboxKindDeployResult        = "deploy_result"
	// OutboxKindTaskOrder is the operator's order to a lane (hk 5125): a new
	// order, or its withdrawal. It goes out as hub type "order".
	OutboxKindTaskOrder = "task_order"
	// OutboxKindTaskDrift is the daily drift and hygiene delivery (hk 5112
	// track B-3): one row per lane per day, plus one operator-desk summary.
	// It goes out as hub type "info".
	OutboxKindTaskDrift = "task_drift"
	// OutboxKindLaneCheck is the per-lane wake check (operator decision 418,
	// hk 5201): one row per lane whose findings need a look, deduplicated by
	// a 6-hour bucket in its event id. It goes out as hub type "findings", a
	// wake-asking type, so the lane is woken at once (hk 6341).
	OutboxKindLaneCheck = "lane_check"
	// OutboxKindDotsNote is the design lane's note of a dots comment or intake
	// (hk 6307): dots' opinion for the design session to sort, never an order.
	// It goes out as hub type "info": it waits for the lane's idle re-wake.
	OutboxKindDotsNote = "dots_note"
	// OutboxSkipBeforeForwarder is the skip_reason of a row queued before the
	// forwarder's cutoff: it was settled visibly instead of delivered, because
	// turning forwarding on must not flush the backlog (hk 4986).
	OutboxSkipBeforeForwarder = "before_forwarder_cutoff"
)

var (
	// ErrOutboxEventNotFound is a mark-sent for an event_id never written.
	ErrOutboxEventNotFound = errors.New("notification_outbox_not_found")
	// ErrOutboxConflict is a mark-sent naming a hub row id different from the
	// one already recorded — the receipt chain disagrees, so nothing changes.
	ErrOutboxConflict = errors.New("notification_outbox_conflict")
	// ErrOutboxNotReceived is an applied ack for a row the hub never
	// accepted: applied follows received.
	ErrOutboxNotReceived = errors.New("notification_outbox_not_received")
)

// NotificationOutbox is one lane notification owed by a server-side write.
// SentAt and HubRowID stay NULL until the drainer records the hub's
// receipt (201 or 409 with the existing row id) here: that is the received
// receipt. AppliedAt and AppliedBy stay NULL until the receiving lane acks
// the event: the applied receipt, which only follows a received one.
// SkippedAt and SkipReason are set when forwarding starts with the row older
// than the forwarder's cutoff: a skipped row is settled, never delivered, and
// can never gain a receipt.
type NotificationOutbox struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	TargetLane string     `json:"target_lane"`
	EventID    string     `json:"event_id"`
	Text       string     `json:"text"`
	CreatedAt  time.Time  `json:"created_at"`
	SentAt     *time.Time `json:"sent_at"`
	HubRowID   *int64     `json:"hub_row_id"`
	AppliedAt  *time.Time `json:"applied_at"`
	AppliedBy  *string    `json:"applied_by"`
	SkippedAt  *time.Time `json:"skipped_at"`
	SkipReason *string    `json:"skip_reason"`
}

const notificationOutboxColumns = `id,kind,target_lane,event_id,text,created_at,sent_at,hub_row_id,applied_at,applied_by,skipped_at,skip_reason`

func scanNotificationOutbox(row interface{ Scan(...any) error }, x *NotificationOutbox) error {
	return row.Scan(&x.ID, &x.Kind, &x.TargetLane, &x.EventID, &x.Text, &x.CreatedAt, &x.SentAt, &x.HubRowID, &x.AppliedAt, &x.AppliedBy, &x.SkippedAt, &x.SkipReason)
}

// decisionAnsweredEventID is the deterministic outbox id for a resolution:
// dr-<task>-<revision>-answered names one request's answer exactly once.
func decisionAnsweredEventID(requestID string) string {
	return requestID + "-answered"
}

// chatAnsweredEventID is the deterministic outbox id for one assistant
// answer to a question at a revision: Q-…-rev<n>-answered.
func chatAnsweredEventID(questionID string, revision int) string {
	return fmt.Sprintf("%s-rev%d-answered", questionID, revision)
}

// assistantLaneText keeps the full notification text for the lane inbox.
func assistantLaneText(prefix, body string) string {
	return prefix + body
}

// insertNotificationOutbox records one owed lane notification inside the
// writer's transaction. ON CONFLICT DO NOTHING keeps a replayed resolution
// from queueing a second notification for the same event.
func insertNotificationOutbox(ctx context.Context, tx transaction, kind, lane, eventID, text string, at time.Time) error {
	_, err := insertNotificationOutboxOnce(ctx, tx, kind, lane, eventID, text, at)
	return err
}

// insertNotificationOutboxOnce is insertNotificationOutbox reporting whether
// this call wrote the row.
func insertNotificationOutboxOnce(ctx context.Context, tx transaction, kind, lane, eventID, text string, at time.Time) (bool, error) {
	return insertNotificationOutboxTx(ctx, tx, kind, lane, eventID, text, at)
}

func insertNotificationOutboxTx(ctx context.Context, tx transaction, kind, lane, eventID, text string, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, sqlFor(tx,
		`INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT (event_id) DO NOTHING`,
		`INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES(?,?,?,?,?)`), kind, lane, eventID, text, at)
	// A repeat of event_id (the only unique key besides the auto id) wrote
	// nothing: report false, as PostgreSQL's ON CONFLICT DO NOTHING does. An
	// ON DUPLICATE KEY no-op would report one affected row and read as a write.
	if errors.Is(err, errUniqueViolation) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return tag == 1, nil
}

// ListUnsentNotifications returns outbox rows the drainer still owes the
// hub, in durable insertion order, created at or after since. A skipped row
// is settled, not owed: it is never listed, so nothing that counts unsent
// rows counts the backlog that forwarding deliberately did not send (hk 4986).
// The cutoff is part of the read, not only of the settle, so a row that
// commits between the two can never be listed as live. The zero time means no
// lower bound; the forwarder always passes its required cutoff.
func (s *Store) ListUnsentNotifications(ctx context.Context, limit int, since time.Time) ([]NotificationOutbox, error) {
	if limit < 1 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.db().Query(ctx, s.q(`SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE sent_at IS NULL AND skipped_at IS NULL AND created_at >= $2 ORDER BY id ASC LIMIT $1`, `SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE sent_at IS NULL AND skipped_at IS NULL AND created_at >= ? ORDER BY id ASC LIMIT ?`), notificationSinceArgs(s.dialect, limit, since)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationOutbox{}
	for rows.Next() {
		var x NotificationOutbox
		if err := scanNotificationOutbox(rows, &x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func notificationSinceArgs(d storeDialect, limit int, since time.Time) []any {
	if d == mysqlDialect {
		return []any{since, limit}
	}
	return []any{limit, since}
}

// SkipNotificationsBefore settles every row created before the forwarder's
// cutoff as skipped: the first start of forwarding settles the backlog
// visibly instead of posting weeks of stale notices (hk 4986). It touches
// only rows still owed (sent_at and skipped_at NULL), so a restart re-skips
// nothing, and it never writes sent_at or hub_row_id. It returns how many
// rows this call settled.
func (s *Store) SkipNotificationsBefore(ctx context.Context, before time.Time, reason string) (int64, error) {
	if before.IsZero() || reason == "" || !validText(reason, 128) {
		return 0, errors.New("invalid notification skip")
	}
	tag, err := s.db().Exec(ctx, s.q(`UPDATE notification_outbox SET skipped_at=now(),skip_reason=$2 WHERE created_at < $1 AND sent_at IS NULL AND skipped_at IS NULL`, `UPDATE notification_outbox SET skipped_at=UTC_TIMESTAMP(6),skip_reason=? WHERE created_at < ? AND sent_at IS NULL AND skipped_at IS NULL`), skipNotificationArgs(s.dialect, before, reason)...)
	if err != nil {
		return 0, err
	}
	return tag, nil
}

func skipNotificationArgs(d storeDialect, before time.Time, reason string) []any {
	if d == mysqlDialect {
		return []any{reason, before}
	}
	return []any{before, reason}
}

// MarkNotificationSent records the hub's receipt for one outbox row. It is
// idempotent: a repeated mark with the same hub_row_id returns the stored
// row; a different hub_row_id on an already-sent row is a conflict, never a
// silent overwrite. hubRowID must be the real hub row id — a duplicate
// answer carrying id 0 is not a receipt and cannot mark a row sent. A
// skipped row can never gain a receipt: sending is what skipping ruled out,
// so it answers ErrOutboxConflict like any other disagreeing receipt.
func (s *Store) MarkNotificationSent(ctx context.Context, eventID string, hubRowID int64) (NotificationOutbox, error) {
	var out NotificationOutbox
	err := s.retryTx(ctx, func() error {
		var err error
		out, err = s.markNotificationSentOnce(ctx, eventID, hubRowID)
		return err
	})
	return out, err
}

// markNotificationSentOnce is one attempt of MarkNotificationSent's write.
func (s *Store) markNotificationSentOnce(ctx context.Context, eventID string, hubRowID int64) (NotificationOutbox, error) {
	if eventID == "" || len(eventID) > 256 || !validText(eventID, 256) || hubRowID < 1 {
		return NotificationOutbox{}, errors.New("invalid notification mark")
	}
	var x NotificationOutbox
	if s.dialect == mysqlDialect {
		return s.markNotificationReceiptMySQL(ctx, eventID, hubRowID, "", false)
	}
	err := scanNotificationOutbox(s.pool.QueryRow(ctx, `UPDATE notification_outbox SET sent_at=now(),hub_row_id=$2 WHERE event_id=$1 AND sent_at IS NULL AND skipped_at IS NULL RETURNING `+notificationOutboxColumns, eventID, hubRowID), &x)
	if err == nil {
		return x, nil
	}
	if !errors.Is(err, ErrNoRows) {
		return NotificationOutbox{}, err
	}
	if err = scanNotificationOutbox(s.pool.QueryRow(ctx, `SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE event_id=$1`, eventID), &x); err != nil {
		if errors.Is(err, ErrNoRows) {
			return NotificationOutbox{}, ErrOutboxEventNotFound
		}
		return NotificationOutbox{}, err
	}
	if x.HubRowID == nil || *x.HubRowID != hubRowID {
		return NotificationOutbox{}, ErrOutboxConflict
	}
	return x, nil
}

// MarkNotificationApplied records the receiving lane's ack for one outbox
// row, the second receipt after the hub's. It is idempotent per (event_id,
// by): a repeat returns the stored row, and an ack by another identity on an
// applied row is a conflict. An ack for a row the hub has not accepted yet is
// ErrOutboxNotReceived: applied never precedes received.
func (s *Store) MarkNotificationApplied(ctx context.Context, eventID, by string) (NotificationOutbox, error) {
	var out NotificationOutbox
	err := s.retryTx(ctx, func() error {
		var err error
		out, err = s.markNotificationAppliedOnce(ctx, eventID, by)
		return err
	})
	return out, err
}

// markNotificationAppliedOnce is one attempt of MarkNotificationApplied's write.
func (s *Store) markNotificationAppliedOnce(ctx context.Context, eventID, by string) (NotificationOutbox, error) {
	if eventID == "" || !validText(eventID, 256) || by == "" || !validText(by, 128) {
		return NotificationOutbox{}, errors.New("invalid notification ack")
	}
	var x NotificationOutbox
	if s.dialect == mysqlDialect {
		return s.markNotificationReceiptMySQL(ctx, eventID, 0, by, true)
	}
	err := scanNotificationOutbox(s.pool.QueryRow(ctx, `UPDATE notification_outbox SET applied_at=now(),applied_by=$2 WHERE event_id=$1 AND sent_at IS NOT NULL AND applied_at IS NULL RETURNING `+notificationOutboxColumns, eventID, by), &x)
	if err == nil {
		return x, nil
	}
	if !errors.Is(err, ErrNoRows) {
		return NotificationOutbox{}, err
	}
	if err = scanNotificationOutbox(s.pool.QueryRow(ctx, `SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE event_id=$1`, eventID), &x); err != nil {
		if errors.Is(err, ErrNoRows) {
			return NotificationOutbox{}, ErrOutboxEventNotFound
		}
		return NotificationOutbox{}, err
	}
	switch {
	case x.SentAt == nil:
		return NotificationOutbox{}, ErrOutboxNotReceived
	case x.AppliedBy == nil || *x.AppliedBy != by:
		return NotificationOutbox{}, ErrOutboxConflict
	}
	return x, nil
}

// markNotificationReceiptMySQL keeps the read, state check and write under one
// row lock because MySQL has no UPDATE ... RETURNING.
func (s *Store) markNotificationReceiptMySQL(ctx context.Context, eventID string, hubRowID int64, by string, applied bool) (NotificationOutbox, error) {
	tx, release, err := s.begin(ctx)
	if err != nil {
		return NotificationOutbox{}, err
	}
	defer release()
	defer tx.Rollback(ctx)
	var x NotificationOutbox
	if err := scanNotificationOutbox(tx.QueryRow(ctx, `SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE event_id=? FOR UPDATE`, eventID), &x); err != nil {
		if errors.Is(err, ErrNoRows) {
			return NotificationOutbox{}, ErrOutboxEventNotFound
		}
		return NotificationOutbox{}, err
	}
	if !applied {
		if x.SkippedAt != nil {
			return NotificationOutbox{}, ErrOutboxConflict
		}
		if x.SentAt != nil {
			if x.HubRowID == nil || *x.HubRowID != hubRowID {
				return NotificationOutbox{}, ErrOutboxConflict
			}
			return x, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE notification_outbox SET sent_at=UTC_TIMESTAMP(6),hub_row_id=? WHERE event_id=?`, hubRowID, eventID); err != nil {
			return NotificationOutbox{}, err
		}
	} else {
		if x.SentAt == nil {
			return NotificationOutbox{}, ErrOutboxNotReceived
		}
		if x.AppliedAt != nil {
			if x.AppliedBy == nil || *x.AppliedBy != by {
				return NotificationOutbox{}, ErrOutboxConflict
			}
			return x, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE notification_outbox SET applied_at=UTC_TIMESTAMP(6),applied_by=? WHERE event_id=?`, by, eventID); err != nil {
			return NotificationOutbox{}, err
		}
	}
	if err := scanNotificationOutbox(tx.QueryRow(ctx, `SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE event_id=?`, eventID), &x); err != nil {
		return NotificationOutbox{}, err
	}
	return x, tx.Commit(ctx)
}
