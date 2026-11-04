package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type TaskDeadlines struct {
	DueAt           *time.Time `json:"due_at"`
	SLAStartedAt    *time.Time `json:"sla_started_at"`
	SLAMediumRiskAt *time.Time `json:"sla_medium_risk_at"`
	SLAHighRiskAt   *time.Time `json:"sla_high_risk_at"`
	SLABreachesAt   *time.Time `json:"sla_breaches_at"`
}

func deadlineText(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (s *Store) SetTaskDeadlines(ctx context.Context, id int64, d TaskDeadlines, by string) (Task, bool, error) {
	return s.setTaskDeadlines(ctx, id, d, by, false)
}

func (s *Store) SetTaskDue(ctx context.Context, id int64, due *time.Time, by string) (Task, bool, error) {
	return s.setTaskDeadlines(ctx, id, TaskDeadlines{DueAt: due}, by, true)
}

func (s *Store) setTaskDeadlines(ctx context.Context, id int64, d TaskDeadlines, by string, dueOnly bool) (Task, bool, error) {
	if id < 1 || by == "" || !validText(by, 128) {
		return Task{}, false, errors.New("invalid task deadlines")
	}
	// Postgres timestamps have microsecond precision. Normalize before
	// comparing and recording history so repeated writes converge.
	for _, p := range []**time.Time{&d.DueAt, &d.SLAStartedAt, &d.SLAMediumRiskAt, &d.SLAHighRiskAt, &d.SLABreachesAt} {
		if *p != nil {
			t := (*p).UTC().Truncate(time.Microsecond)
			*p = &t
		}
	}
	// Supplied SLA thresholds must be chronological; each may be cleared.
	times := []*time.Time{d.SLAStartedAt, d.SLAMediumRiskAt, d.SLAHighRiskAt, d.SLABreachesAt}
	var previous *time.Time
	for _, t := range times {
		if t == nil {
			continue
		}
		if previous != nil && t.Before(*previous) {
			return Task{}, false, errors.New("invalid SLA times")
		}
		previous = t
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Task{}, false, err
	}
	defer tx.Rollback(ctx)
	var x Task
	if err = scanTask(tx.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, id), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Task{}, false, ErrTaskNotFound
		}
		return Task{}, false, err
	}
	if err = s.wrkTaskFenceTx(ctx, tx, x, "", true); err != nil {
		return Task{}, false, err
	}
	if dueOnly {
		d.SLAStartedAt, d.SLAMediumRiskAt, d.SLAHighRiskAt, d.SLABreachesAt = x.SLAStartedAt, x.SLAMediumRiskAt, x.SLAHighRiskAt, x.SLABreachesAt
	}
	names := []string{"due_at", "sla_started_at", "sla_medium_risk_at", "sla_high_risk_at", "sla_breaches_at"}
	old := []*time.Time{x.DueAt, x.SLAStartedAt, x.SLAMediumRiskAt, x.SLAHighRiskAt, x.SLABreachesAt}
	next := []*time.Time{d.DueAt, d.SLAStartedAt, d.SLAMediumRiskAt, d.SLAHighRiskAt, d.SLABreachesAt}
	changed := false
	for i := range names {
		if deadlineText(old[i]) != deadlineText(next[i]) {
			changed = true
		}
	}
	if !changed {
		return x, false, tx.Commit(ctx)
	}
	now := time.Now().UTC()
	if err = scanTask(tx.QueryRow(ctx, `UPDATE tasks SET due_at=$2,sla_started_at=$3,sla_medium_risk_at=$4,sla_high_risk_at=$5,sla_breaches_at=$6,updated_at=$7 WHERE id=$1 RETURNING `+taskColumns, id, d.DueAt, d.SLAStartedAt, d.SLAMediumRiskAt, d.SLAHighRiskAt, d.SLABreachesAt, now), &x); err != nil {
		return Task{}, false, err
	}
	for i, name := range names {
		from, to := deadlineText(old[i]), deadlineText(next[i])
		if from == to {
			continue
		}
		if _, _, err = insertEvent(ctx, tx, taskEventInput{TaskID: id, Kind: "metadata", From: x.State, To: x.State, By: by, Refs: x.Refs, At: now, Field: name, FromValue: from, ToValue: to}); err != nil {
			return Task{}, false, err
		}
	}
	return x, true, tx.Commit(ctx)
}
