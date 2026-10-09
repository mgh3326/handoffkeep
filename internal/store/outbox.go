package store

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

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
)

var (
	// ErrOutboxEventNotFound is a mark-sent for an event_id never written.
	ErrOutboxEventNotFound = errors.New("notification_outbox_not_found")
	// ErrOutboxConflict is a mark-sent naming a hub row id different from the
	// one already recorded — the receipt chain disagrees, so nothing changes.
	ErrOutboxConflict = errors.New("notification_outbox_conflict")
)

// NotificationOutbox is one lane notification owed by an assistant-path
// write. SentAt and HubRowID stay NULL until the drainer records the hub's
// receipt (201 or 409 with the existing row id) here.
type NotificationOutbox struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	TargetLane string     `json:"target_lane"`
	EventID    string     `json:"event_id"`
	Text       string     `json:"text"`
	CreatedAt  time.Time  `json:"created_at"`
	SentAt     *time.Time `json:"sent_at"`
	HubRowID   *int64     `json:"hub_row_id"`
}

const notificationOutboxColumns = `id,kind,target_lane,event_id,text,created_at,sent_at,hub_row_id`

func scanNotificationOutbox(row interface{ Scan(...any) error }, x *NotificationOutbox) error {
	return row.Scan(&x.ID, &x.Kind, &x.TargetLane, &x.EventID, &x.Text, &x.CreatedAt, &x.SentAt, &x.HubRowID)
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

// assistantLaneText bounds one notification to the lane-event byte cap the
// hub enforces, so a written row can never be undrainable. The full answer
// lives in the message body or the decision record; the lane event is a
// pointer with a quote.
func assistantLaneText(prefix, body string) string {
	text := prefix + body
	if len(text) <= RelayLaneEventMaxBytes {
		return text
	}
	keep := RelayLaneEventMaxBytes - len(prefix) - len("…")
	if keep < 0 {
		keep = 0
	}
	if len(body) > keep {
		body = body[:keep]
	}
	for len(body) > 0 && !utf8.ValidString(body) {
		body = body[:len(body)-1]
	}
	return prefix + body + "…"
}

// insertNotificationOutbox records one owed lane notification inside the
// writer's transaction. ON CONFLICT DO NOTHING keeps a replayed resolution
// from queueing a second notification for the same event.
func insertNotificationOutbox(ctx context.Context, tx pgx.Tx, kind, lane, eventID, text string, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT (event_id) DO NOTHING`, kind, lane, eventID, text, at)
	return err
}

// AssistantPendingDecision is one open decision request with the task it
// blocks on — what an assistant poller needs to read, answer or skip it.
type AssistantPendingDecision struct {
	TaskID  int64            `json:"task_id"`
	Lane    string           `json:"lane"`
	State   string           `json:"state"`
	Title   string           `json:"title"`
	Request DecisionRequest  `json:"request"`
	Options *DecisionOptions `json:"options,omitempty"`
}

// AssistantPending is the assistant surface's pending work list: open
// decision requests on live tasks (merged and dropped tasks are terminal —
// an uncleaned open request left on them is not answerable work) plus every
// pending chat question carrying its revision. ServerTime is the hk clock so
// a poller judges deadlines against the server's time, never its own.
type AssistantPending struct {
	ServerTime       time.Time                  `json:"server_time"`
	DecisionRequests []AssistantPendingDecision `json:"decision_requests"`
	ChatQuestions    []ChatQuestion             `json:"chat_questions"`
}

// ListAssistantPending reads the two pending sets with the database clock.
func (s *Store) ListAssistantPending(ctx context.Context) (AssistantPending, error) {
	out := AssistantPending{DecisionRequests: []AssistantPendingDecision{}, ChatQuestions: []ChatQuestion{}}
	if err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&out.ServerTime); err != nil {
		return out, err
	}
	tasks, err := s.pool.Query(ctx, `SELECT `+taskColumns+` FROM tasks WHERE refs->'decision_request'->>'status'='open' AND state NOT IN ('merged','dropped') ORDER BY id ASC`)
	if err != nil {
		return out, err
	}
	defer tasks.Close()
	for tasks.Next() {
		var x Task
		if err := scanTask(tasks, &x); err != nil {
			return out, err
		}
		if x.Refs.DecisionRequest == nil {
			continue
		}
		out.DecisionRequests = append(out.DecisionRequests, AssistantPendingDecision{TaskID: x.ID, Lane: x.Lane, State: x.State, Title: x.Title, Request: *x.Refs.DecisionRequest, Options: x.Refs.DecisionOptions})
	}
	if err := tasks.Err(); err != nil {
		return out, err
	}
	questions, err := s.ListChatQuestions(ctx, "", "pending", "", 1000)
	if err != nil {
		return out, err
	}
	out.ChatQuestions = questions
	return out, nil
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
