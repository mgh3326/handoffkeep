package remote

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TaskWaitClearResult is the answer to a clear: whether this call ended the
// wait, and the wait as it reads afterwards.
type TaskWaitClearResult struct {
	Changed bool           `json:"changed"`
	Wait    store.TaskWait `json:"wait"`
}

// AddTaskWait records a hand wait of task id (POST /v1/tasks/{id}/waits).
// Repeating an open wait of the same kind and ref updates its reason.
func (c Client) AddTaskWait(ctx context.Context, id int64, in store.TaskWaitInput) (store.TaskWait, error) {
	var out store.TaskWait
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/waits", id), in, &out)
	if err != nil {
		return out, unsupportedRoute(err, "task_wait_unsupported", "the task waits route")
	}
	return out, nil
}

// ClearTaskWait ends hand wait waitID of task id with an optional note.
func (c Client) ClearTaskWait(ctx context.Context, id, waitID int64, note string) (TaskWaitClearResult, error) {
	var out TaskWaitClearResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/waits/%d/clear", id, waitID), struct {
		Note string `json:"note"`
	}{note}, &out)
	if err != nil {
		return out, unsupportedRoute(err, "task_wait_unsupported", "the task waits route")
	}
	return out, nil
}

// ListWaits reads the waits sections (GET /v1/waits). section is one section key
// or empty for every section.
func (c Client) ListWaits(ctx context.Context, section string) (store.WaitsView, error) {
	var out store.WaitsView
	q := url.Values{"open": {"true"}}
	if section != "" {
		q.Set("section", section)
	}
	err := c.call(ctx, "GET", "/v1/waits?"+q.Encode(), nil, &out)
	if err != nil {
		return out, unsupportedRoute(err, "waits_unsupported", "the waits route")
	}
	return out, nil
}
