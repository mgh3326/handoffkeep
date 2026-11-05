package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Declarative automation (docs/linear-internalize.md): started_stalled_hours
// flags started work at read time and moves nothing; backlog_icebox_days
// parks idle backlog and is off until the desk enables it (Q7).
const DefaultStartedStalledHours = 24

// Bounds on the two settings. Both become a time.Duration, which overflows
// past about 292 years: an unbounded backlog_icebox_days would wrap the
// sweep cutoff into the future and park every backlog task, and an unbounded
// started_stalled_hours would wrap negative and flag every started task.
const (
	MaxStartedStalledHours = 8760 // one year
	MaxBacklogIceboxDays   = 3650 // ten years
)

// CheckStartedStalledHours reports whether hours is a usable
// started_stalled_hours: 1..MaxStartedStalledHours.
func CheckStartedStalledHours(hours int) error {
	if hours < 1 || hours > MaxStartedStalledHours {
		return fmt.Errorf("started_stalled_hours must be 1..%d, got %d", MaxStartedStalledHours, hours)
	}
	return nil
}

// CheckBacklogIceboxDays reports whether days is a usable
// backlog_icebox_days: 0 (off) or 1..MaxBacklogIceboxDays.
func CheckBacklogIceboxDays(days int) error {
	if days < 0 || days > MaxBacklogIceboxDays {
		return fmt.Errorf("backlog_icebox_days must be 0 (off) or 1..%d, got %d", MaxBacklogIceboxDays, days)
	}
	return nil
}

// taskActivitySQL is a task's newest activity: a task_events row, a comment,
// or an update to the job bound to it in refs.job_id. A late update to an
// earlier job of the task no longer counts once another job is bound.
// created_at is the floor, so a task nobody touched is idle since it was
// filed. It reads the row as "tasks".
const taskActivitySQL = `GREATEST(tasks.created_at,
	(SELECT max(e.at) FROM task_events e WHERE e.task_id=tasks.id),
	(SELECT max(c.created_at) FROM task_comments c WHERE c.task_id=tasks.id),
	(SELECT max(j.updated_at) FROM jobs j WHERE j.job_id=tasks.refs->>'job_id' AND j.task_id=tasks.id))`

// SetStartedStalledHours sets started_stalled_hours; a value below 1 restores
// the default. A value above MaxStartedStalledHours is refused and the
// setting is left as it was.
func (s *Store) SetStartedStalledHours(hours int) error {
	if hours > MaxStartedStalledHours {
		return CheckStartedStalledHours(hours)
	}
	if hours < 1 {
		hours = 0
	}
	s.stalledHours.Store(int64(hours))
	return nil
}

// StartedStalledHours is the started_stalled_hours setting as a duration.
func (s *Store) StartedStalledHours() time.Duration {
	h := s.stalledHours.Load()
	if h < 1 {
		h = DefaultStartedStalledHours
	}
	return time.Duration(h) * time.Hour
}

// isStalled is the stalled rule: a started task, not waiting on a decision,
// whose newest activity is older than the setting.
func isStalled(state string, awaiting bool, activity, now time.Time, after time.Duration) bool {
	return IsStartedState(state) && !awaiting && now.Sub(activity) > after
}

type poolQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// markStalled sets Stalled on the started rows of xs, reading their activity
// in one query.
func (s *Store) markStalled(ctx context.Context, q poolQuerier, xs []Task, now time.Time) error {
	ids := []int64{}
	at := map[int64]int{}
	for i, x := range xs {
		if IsStartedState(x.State) && !x.AwaitingDecision {
			ids = append(ids, x.ID)
			at[x.ID] = i
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.Query(ctx, `SELECT id,`+taskActivitySQL+` FROM tasks WHERE id = ANY($1)`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	after := s.StartedStalledHours()
	for rows.Next() {
		var id int64
		var activity time.Time
		if err = rows.Scan(&id, &activity); err != nil {
			return err
		}
		x := &xs[at[id]]
		x.Stalled = isStalled(x.State, x.AwaitingDecision, activity, now, after)
	}
	return rows.Err()
}

// SweepCandidate is one backlog task the sweep parks.
type SweepCandidate struct {
	ID        int64     `json:"id"`
	Lane      string    `json:"lane"`
	Title     string    `json:"title"`
	IdleSince time.Time `json:"idle_since"`
}

// sweepSQL selects idle backlog tasks. It skips flagged tasks, tasks with an
// open decision request, and parents with open (started) children; unstarted
// tasks are never selected.
const sweepSQL = `SELECT id,lane,title,` + taskActivitySQL + ` FROM tasks
	WHERE state='backlog' AND NOT awaiting_decision
	AND COALESCE(refs->'decision_request'->>'status','') <> $2
	AND NOT EXISTS (SELECT 1 FROM tasks c WHERE c.parent_id=tasks.id AND c.state IN (` + startedStatesSQL + `))
	AND ` + taskActivitySQL + ` < $1`

// SweepBacklog parks backlog tasks idle for more than days in icebox,
// recording hk.sweep on each, and returns what it moved. With dryRun it
// returns what it would move and writes nothing. days 0 is the off setting
// and moves nothing; days outside CheckBacklogIceboxDays is refused before
// any read.
func (s *Store) SweepBacklog(ctx context.Context, days int, dryRun bool) ([]SweepCandidate, error) {
	if err := CheckBacklogIceboxDays(days); err != nil {
		return nil, err
	}
	out := []SweepCandidate{}
	if days == 0 {
		return out, nil
	}
	now := time.Now().UTC()
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := s.pool.Query(ctx, sweepSQL+` ORDER BY id LIMIT 1000`, cutoff, DecisionRequestOpen)
	if err != nil {
		return nil, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SweepCandidate, error) {
		var c SweepCandidate
		err := row.Scan(&c.ID, &c.Lane, &c.Title, &c.IdleSince)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	if dryRun {
		return candidates, nil
	}
	for _, c := range candidates {
		moved, err := s.sweepOne(ctx, c.ID, days, cutoff, now)
		if err != nil {
			return out, err
		}
		if moved {
			out = append(out, c)
		}
	}
	return out, nil
}

// sweepOne re-checks one candidate under its row lock and parks it.
func (s *Store) sweepOne(ctx context.Context, id int64, days int, cutoff, now time.Time) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(8241873001)`); err != nil {
		return false, err
	}
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	var idleSince time.Time
	err = tx.QueryRow(ctx, sweepSQL+` AND id=$3`, cutoff, DecisionRequestOpen, id).Scan(new(int64), new(string), new(string), &idleSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	ev := sourceEvent{
		Source: SourceSweep, SourceRef: "idle-since/" + idleSince.UTC().Format(time.RFC3339Nano), At: now,
		Note: fmt.Sprintf("backlog idle since %s; backlog_icebox_days=%d", idleSince.UTC().Format(time.RFC3339), days),
	}
	res, err := s.applySourceEventTx(ctx, tx, x, ev, now)
	if err != nil {
		return false, err
	}
	return res.Applied, tx.Commit(ctx)
}
