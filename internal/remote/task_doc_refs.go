package remote

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TaskDocRefChange is the answer to a reference add or remove: whether this
// call changed the reference, and the task and document it names.
type TaskDocRefChange struct {
	Changed bool   `json:"changed"`
	TaskID  int64  `json:"task_id"`
	Doc     string `json:"doc"`
}

// ListTaskDocRefs reads the documents task id cites by reference (GET
// /v1/tasks/{id}/doc-refs).
func (c Client) ListTaskDocRefs(ctx context.Context, id int64) ([]store.TaskDocRef, error) {
	var out struct {
		DocRefs []store.TaskDocRef `json:"doc_refs"`
	}
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/tasks/%d/doc-refs", id), nil, &out)
	if err != nil {
		return nil, unsupportedRoute(err, "task_doc_refs_unsupported", "the task doc-refs route")
	}
	return out.DocRefs, nil
}

// AddTaskDocRef records a reference from task id to document key doc (POST
// /v1/tasks/{id}/doc-refs). Repeating it changes nothing.
func (c Client) AddTaskDocRef(ctx context.Context, id int64, doc string) (TaskDocRefChange, error) {
	var out TaskDocRefChange
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/doc-refs", id), struct {
		Doc string `json:"doc"`
	}{doc}, &out)
	if err != nil {
		return out, unsupportedRoute(err, "task_doc_refs_unsupported", "the task doc-refs route")
	}
	return out, nil
}

// RemoveTaskDocRef drops the reference from task id to document key doc (DELETE
// /v1/tasks/{id}/doc-refs?doc=). Removing an absent reference changes nothing.
func (c Client) RemoveTaskDocRef(ctx context.Context, id int64, doc string) (TaskDocRefChange, error) {
	var out TaskDocRefChange
	q := url.Values{"doc": {doc}}
	err := c.call(ctx, "DELETE", fmt.Sprintf("/v1/tasks/%d/doc-refs?%s", id, q.Encode()), nil, &out)
	if err != nil {
		return out, unsupportedRoute(err, "task_doc_refs_unsupported", "the task doc-refs route")
	}
	return out, nil
}
