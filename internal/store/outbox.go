package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
}

const notificationOutboxColumns = `id,kind,target_lane,event_id,text,created_at,sent_at,hub_row_id,applied_at,applied_by`

func scanNotificationOutbox(row interface{ Scan(...any) error }, x *NotificationOutbox) error {
	return row.Scan(&x.ID, &x.Kind, &x.TargetLane, &x.EventID, &x.Text, &x.CreatedAt, &x.SentAt, &x.HubRowID, &x.AppliedAt, &x.AppliedBy)
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
func insertNotificationOutbox(ctx context.Context, tx pgx.Tx, kind, lane, eventID, text string, at time.Time) error {
	_, err := insertNotificationOutboxOnce(ctx, tx, kind, lane, eventID, text, at)
	return err
}

// insertNotificationOutboxOnce is insertNotificationOutbox reporting whether
// this call wrote the row.
func insertNotificationOutboxOnce(ctx context.Context, tx pgx.Tx, kind, lane, eventID, text string, at time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT (event_id) DO NOTHING`, kind, lane, eventID, text, at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ListUnsentNotifications returns outbox rows the drainer still owes the
// hub, in durable insertion order.
func (s *Store) ListUnsentNotifications(ctx context.Context, limit int) ([]NotificationOutbox, error) {
	if limit < 1 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE sent_at IS NULL ORDER BY id ASC LIMIT $1`, limit)
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

// MarkNotificationSent records the hub's receipt for one outbox row. It is
// idempotent: a repeated mark with the same hub_row_id returns the stored
// row; a different hub_row_id on an already-sent row is a conflict, never a
// silent overwrite. hubRowID must be the real hub row id — a duplicate
// answer carrying id 0 is not a receipt and cannot mark a row sent.
func (s *Store) MarkNotificationSent(ctx context.Context, eventID string, hubRowID int64) (NotificationOutbox, error) {
	if eventID == "" || len(eventID) > 256 || !validText(eventID, 256) || hubRowID < 1 {
		return NotificationOutbox{}, errors.New("invalid notification mark")
	}
	var x NotificationOutbox
	err := scanNotificationOutbox(s.pool.QueryRow(ctx, `UPDATE notification_outbox SET sent_at=now(),hub_row_id=$2 WHERE event_id=$1 AND sent_at IS NULL RETURNING `+notificationOutboxColumns, eventID, hubRowID), &x)
	if err == nil {
		return x, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return NotificationOutbox{}, err
	}
	if err = scanNotificationOutbox(s.pool.QueryRow(ctx, `SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE event_id=$1`, eventID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
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
	if eventID == "" || !validText(eventID, 256) || by == "" || !validText(by, 128) {
		return NotificationOutbox{}, errors.New("invalid notification ack")
	}
	var x NotificationOutbox
	err := scanNotificationOutbox(s.pool.QueryRow(ctx, `UPDATE notification_outbox SET applied_at=now(),applied_by=$2 WHERE event_id=$1 AND sent_at IS NOT NULL AND applied_at IS NULL RETURNING `+notificationOutboxColumns, eventID, by), &x)
	if err == nil {
		return x, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return NotificationOutbox{}, err
	}
	if err = scanNotificationOutbox(s.pool.QueryRow(ctx, `SELECT `+notificationOutboxColumns+` FROM notification_outbox WHERE event_id=$1`, eventID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
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
