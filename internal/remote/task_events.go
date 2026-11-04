package remote

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ApplyTaskEvent posts one integration event (POST /v1/task-events). The
// result says whether it applied; a redelivered event reports redelivered.
func (c Client) ApplyTaskEvent(ctx context.Context, in store.TaskSourceEvent) (store.TaskEventResult, error) {
	var out store.TaskEventResult
	if err := c.call(ctx, "POST", "/v1/task-events", in, &out); err != nil {
		return out, unsupportedRoute(err, "task_events_unsupported", "the task events route")
	}
	if out.State == "" {
		return out, errors.New("task_event_not_applied: server response has no state — it likely predates task events")
	}
	return out, nil
}

// ListTasksFiltered lists tasks with the full filter set. A stalled filter
// the server does not echo is refused rather than shown as every task.
func (c Client) ListTasksFiltered(ctx context.Context, tq store.TaskQuery) ([]store.Task, error) {
	var out struct {
		Tasks   []store.Task `json:"tasks"`
		Project *string      `json:"project"`
		Area    *string      `json:"area"`
		Stalled bool         `json:"stalled"`
	}
	q := url.Values{"lane": {tq.Lane}, "state": {tq.State}, "parent_lane": {tq.ParentLane}, "limit": {fmt.Sprint(tq.Limit)}}
	if tq.Project != nil {
		q.Set("project", *tq.Project)
	}
	if tq.Area != nil {
		q.Set("area", *tq.Area)
	}
	if tq.AfterID != nil {
		q.Set("after_id", fmt.Sprint(*tq.AfterID))
	}
	if tq.Stalled {
		q.Set("stalled", "true")
	}
	if err := c.call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	if tq.Stalled && !out.Stalled {
		return nil, errors.New("list_stalled_filter_ignored: the server did not echo stalled=true — it likely predates the stalled flag")
	}
	if err := taskProjectEchoOK(out.Project, tq.Project); err != nil {
		return nil, err
	}
	if err := taskProjectFilterOK(out.Tasks, tq.Project); err != nil {
		return nil, err
	}
	if err := taskAreaListOK(out.Tasks, out.Area, tq.Area); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// SweepPreview returns the backlog tasks the sweep would park at days
// (GET /v1/tasks/sweep). It writes nothing.
func (c Client) SweepPreview(ctx context.Context, days int) ([]store.SweepCandidate, error) {
	var out struct {
		DryRun bool                   `json:"dry_run"`
		Tasks  []store.SweepCandidate `json:"tasks"`
	}
	q := url.Values{"days": {fmt.Sprint(days)}}
	if err := c.call(ctx, "GET", "/v1/tasks/sweep?"+q.Encode(), nil, &out); err != nil {
		return nil, unsupportedRoute(err, "task_sweep_unsupported", "the backlog sweep preview")
	}
	if !out.DryRun {
		return nil, errors.New("task_sweep_unsupported: the server did not confirm a dry run")
	}
	return out.Tasks, nil
}
