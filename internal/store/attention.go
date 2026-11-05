package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// What is waiting on the operator, as the console Home shows it: open
// decisions, tasks that need the operator, counts and a bounded top list for
// each. Every count and its items come from one read-only snapshot, so a count
// never disagrees with the list under it. Nothing here lists a source in full:
// each source is counted in SQL and cut at AttentionItems.

// AttentionItems is how many items each source returns.
const AttentionItems = 10

// Attention decision sources. A request is a decision_requests row; a
// lane event is an unanswered [decision-needed] lane event that has no open
// request of its own.
const (
	AttentionSourceRequest   = "request"
	AttentionSourceLaneEvent = DecisionSourceLaneEvent
)

// Why a task is waiting on the operator. No blocked task state exists, so
// these two reasons are what "blocked" means.
const (
	AttentionReasonAwaitingDecision = "awaiting_decision"
	AttentionReasonStalled          = "stalled"
)

// Attention is the whole read.
type Attention struct {
	Decisions AttentionDecisions
	Tasks     AttentionTasks
}

// AttentionDecisions counts open decisions and carries the first AttentionItems.
type AttentionDecisions struct {
	Count int
	Items []AttentionDecision
}

// AttentionDecision is one open decision. ID is the decision_requests id for a
// request and the relay event id for a lane event.
type AttentionDecision struct {
	Source    string
	ID        int64
	Lane      string
	Question  string
	CreatedAt time.Time
	DueAt     *time.Time
	Overdue   bool
}

// AttentionTasks counts the tasks that need the operator. Count is
// AwaitingDecision + Stalled; the two sets cannot overlap.
type AttentionTasks struct {
	Count            int
	AwaitingDecision int
	Stalled          int
	Items            []AttentionTask
}

// AttentionTask is one waiting task. Since is its newest activity.
type AttentionTask struct {
	ID     int64
	Title  string
	Lane   string
	State  string
	Reason string
	Since  time.Time
}

// attentionLaneSQL is the lane-event decisions that count here, with the read
// time as $1: the inbox's open [decision-needed] events, minus those whose own
// decision_requests row is still live (open and not expired). Recording such an
// event also opens that row, so decision_requests is the primary side and the
// event is skipped while the row is live, snoozed or not: a snoozed request
// must not come back through its event. Once the row expires it leaves the
// request list, so the event shows again rather than neither of them.
const attentionLaneSQL = `relay_events e WHERE ` + openLaneDecisionSQL + ` AND NOT EXISTS (
		SELECT 1 FROM decision_requests d WHERE d.source='lane_event' AND d.source_ref=e.id::text AND d.` + decisionLiveSQL + `)`

// attentionStalledSQL is the stalled rule of ListTasks, read as "tasks", with
// the stalled threshold as $1.
const attentionStalledSQL = `state IN (` + startedStatesSQL + `) AND NOT awaiting_decision AND ` + taskActivitySQL + ` < $1::timestamptz`

// Attention reads what is waiting on the operator at now.
func (s *Store) Attention(ctx context.Context, now time.Time) (Attention, error) {
	now = now.UTC()
	var out Attention
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Attention{}, err
	}
	defer tx.Rollback(ctx)

	if out.Decisions, err = attentionDecisionsTx(ctx, tx, now); err != nil {
		return Attention{}, fmt.Errorf("attention decisions: %w", err)
	}
	if out.Tasks, err = attentionTasksTx(ctx, tx, now.Add(-s.StartedStalledHours())); err != nil {
		return Attention{}, fmt.Errorf("attention tasks: %w", err)
	}
	return out, tx.Commit(ctx)
}

func attentionDecisionsTx(ctx context.Context, tx pgx.Tx, now time.Time) (AttentionDecisions, error) {
	out := AttentionDecisions{Items: []AttentionDecision{}}
	var requests, lanes int
	if err := tx.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM decision_requests WHERE `+decisionOpenSQL+`),
		(SELECT COUNT(*) FROM `+attentionLaneSQL+`)`, now).Scan(&requests, &lanes); err != nil {
		return out, err
	}
	out.Count = requests + lanes

	items, err := attentionRequestItems(ctx, tx, now)
	if err != nil {
		return out, err
	}
	laneItems, err := attentionLaneItems(ctx, tx, now)
	if err != nil {
		return out, err
	}
	items = append(items, laneItems...)
	sort.SliceStable(items, func(i, j int) bool { return attentionDecisionLess(items[i], items[j]) })
	if len(items) > AttentionItems {
		items = items[:AttentionItems]
	}
	out.Items = items
	return out, nil
}

// attentionDecisionLess orders overdue first, then the nearest due date (none
// last), then the oldest, then source and id so equal rows keep a fixed order.
func attentionDecisionLess(a, b AttentionDecision) bool {
	if a.Overdue != b.Overdue {
		return a.Overdue
	}
	if (a.DueAt == nil) != (b.DueAt == nil) {
		return a.DueAt != nil
	}
	if a.DueAt != nil && !a.DueAt.Equal(*b.DueAt) {
		return a.DueAt.Before(*b.DueAt)
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	if a.Source != b.Source {
		return a.Source < b.Source
	}
	return a.ID < b.ID
}

func attentionRequestItems(ctx context.Context, tx pgx.Tx, now time.Time) ([]AttentionDecision, error) {
	rows, err := tx.Query(ctx, `SELECT id,lane,question,created_at,due_at FROM decision_requests WHERE `+decisionOpenSQL+`
		ORDER BY (due_at IS NOT NULL AND due_at<=$1::timestamptz) DESC, due_at ASC NULLS LAST, created_at ASC, id ASC LIMIT $2`, now, AttentionItems)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttentionDecision{}
	for rows.Next() {
		x := AttentionDecision{Source: AttentionSourceRequest}
		if err := rows.Scan(&x.ID, &x.Lane, &x.Question, &x.CreatedAt, &x.DueAt); err != nil {
			return nil, err
		}
		x.CreatedAt = x.CreatedAt.UTC()
		if x.DueAt != nil {
			due := x.DueAt.UTC()
			x.DueAt = &due
			x.Overdue = !due.After(now)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func attentionLaneItems(ctx context.Context, tx pgx.Tx, now time.Time) ([]AttentionDecision, error) {
	rows, err := tx.Query(ctx, `SELECT e.id,e.owner_lane,e.text,e.received_at FROM `+attentionLaneSQL+`
		ORDER BY e.received_at ASC, e.id ASC LIMIT $2`, now, AttentionItems)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AttentionDecision{}
	for rows.Next() {
		x := AttentionDecision{Source: AttentionSourceLaneEvent}
		var text string
		if err := rows.Scan(&x.ID, &x.Lane, &text, &x.CreatedAt); err != nil {
			return nil, err
		}
		x.Question = strings.TrimSpace(strings.TrimPrefix(text, "[decision-needed]"))
		x.CreatedAt = x.CreatedAt.UTC()
		out = append(out, x)
	}
	return out, rows.Err()
}

// attentionTasksTx reads the waiting tasks. stalledBefore is the activity time
// a started task must be older than to count as stalled.
func attentionTasksTx(ctx context.Context, tx pgx.Tx, stalledBefore time.Time) (AttentionTasks, error) {
	out := AttentionTasks{Items: []AttentionTask{}}
	// Only awaiting and started rows can qualify, so the activity subqueries
	// run for those alone.
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE `+awaitingTaskDecisionSQL+`),
		COUNT(*) FILTER (WHERE `+attentionStalledSQL+`)
		FROM tasks WHERE awaiting_decision OR state IN (`+startedStatesSQL+`)`, stalledBefore).Scan(&out.AwaitingDecision, &out.Stalled); err != nil {
		return out, err
	}
	out.Count = out.AwaitingDecision + out.Stalled

	rows, err := tx.Query(ctx, `SELECT tasks.id,title,lane,state,(`+awaitingTaskDecisionSQL+`) AS awaiting,`+taskActivitySQL+` AS since
		FROM tasks WHERE (`+awaitingTaskDecisionSQL+`) OR (`+attentionStalledSQL+`)
		ORDER BY awaiting DESC, since ASC, tasks.id ASC LIMIT $2`, stalledBefore, AttentionItems)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var x AttentionTask
		var awaiting bool
		if err := rows.Scan(&x.ID, &x.Title, &x.Lane, &x.State, &awaiting, &x.Since); err != nil {
			return out, err
		}
		x.Since = x.Since.UTC()
		x.Reason = AttentionReasonStalled
		if awaiting {
			x.Reason = AttentionReasonAwaitingDecision
		}
		out.Items = append(out.Items, x)
	}
	return out, rows.Err()
}
