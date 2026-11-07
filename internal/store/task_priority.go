package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/guard"
)

var ErrInvalidTaskPriority = errors.New("invalid_task_priority")

// PriorityResult is the result recorded for an idempotent priority write.
type PriorityResult struct {
	Previous int `json:"previous"`
	Priority int `json:"priority"`
	// Replayed is true when the event key was already recorded and this call
	// wrote nothing, so the recorded values may not be the row's current ones.
	Replayed bool `json:"replayed,omitempty"`
}

// SetTaskPriority records an operator's priority change and replays its first result.
func (s *Store) SetTaskPriority(ctx context.Context, id int64, priority int, by, reason, key string) (PriorityResult, error) {
	var out PriorityResult
	if id < 1 || priority < 0 || priority > math.MaxInt32 || by == "" || !validText(by, 128) || !validText(reason, MaxBytes) || strings.TrimSpace(key) == "" || !validText(key, 128) {
		return out, ErrInvalidTaskPriority
	}
	if err := guard.Reject(reason); err != nil {
		return out, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return out, ErrTaskNotFound
		}
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT from_value::int,to_value::int FROM task_events WHERE task_id=$1 AND source='ui-priority' AND source_ref=$2`, id, key).Scan(&out.Previous, &out.Priority)
	if err == nil {
		out.Replayed = true
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if IsSettledState(x.State) {
		return out, ErrTaskTerminal
	}
	out = PriorityResult{Previous: x.Priority, Priority: priority}
	now := time.Now().UTC()
	if _, err = tx.Exec(ctx, `UPDATE tasks SET priority=$2,updated_at=$3 WHERE id=$1`, id, priority, now); err != nil {
		return PriorityResult{}, err
	}
	note := fmt.Sprintf("priority %d → %d", x.Priority, priority)
	if reason != "" {
		note += ": " + reason
	}
	if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "metadata", From: x.State, To: x.State, By: by, Note: note, Refs: x.Refs, At: now, Source: "ui-priority", SourceRef: key, Field: "priority", FromValue: fmt.Sprint(x.Priority), ToValue: fmt.Sprint(priority)}); err != nil {
		return PriorityResult{}, err
	}
	return out, tx.Commit(ctx)
}
