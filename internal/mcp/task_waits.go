package mcp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	gmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTaskWaitTools adds the waits tools (hk 5128): task_wait and
// task_unwait record and end a session's hand wait, and waits_list reads the
// four sections and the backlog. A repeated task_wait for an open wait of the
// same kind and ref updates its reason, so the writes need no idempotency key.
func registerTaskWaitTools(s *gmcp.Server, o operatorTools) {
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "waits_list",
		Description: "Read what hk tasks wait on, in four sections: needs_operator (open decision requests and operator waits, with a recorded answer awaiting confirmation shown as answered), failed_start (a spawn that stopped, recorded as an external:spawn wait), waiting_on (another task, lane, PR, CI or external dependency) and idle_started (researching or developing with no recent activity). section filters to one section. Each row has the task, whom and what it waits on, since and age_seconds, next_action and grounds_link. The ordinary backlog is listed apart. Counts cover every section. Read-only.",
		Annotations: readOnlyAnnotations(),
	}, o.waitsList)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_wait",
		Description: "Record that one hk task waits on something the store cannot derive: on_kind operator (an operator step, with an empty on_ref), lane, task (on_ref is another task id), pr, ci or external (on_ref names it; external with spawn records a failed start). reason says what is wanted, in one line. Repeating an open wait of the same kind and ref updates its reason. Returns the wait.",
		Annotations: writeAnnotations(),
	}, o.taskWait)
	gmcp.AddTool(s, &gmcp.Tool{
		Name:        "task_unwait",
		Description: "End one hand wait of an hk task by its wait id, with an optional note on how it ended. Ending a wait that already ended changes nothing. Returns changed and the wait.",
		Annotations: writeAnnotations(),
	}, o.taskUnwait)
}

type taskWaitInput struct {
	ID     int64  `json:"id"`
	OnKind string `json:"on_kind"`
	OnRef  string `json:"on_ref,omitempty"`
	Reason string `json:"reason"`
}

type taskUnwaitInput struct {
	ID     int64  `json:"id"`
	WaitID int64  `json:"wait_id"`
	Note   string `json:"note,omitempty"`
}

type waitsListInput struct {
	Section string `json:"section,omitempty"`
}

func (o operatorTools) taskWait(ctx context.Context, _ *gmcp.CallToolRequest, in taskWaitInput) (*gmcp.CallToolResult, any, error) {
	if in.ID < 1 {
		return nil, nil, errors.New("id must be positive")
	}
	wait, err := o.service.AddTaskWait(ctx, in.ID, store.TaskWaitInput{OnKind: strings.TrimSpace(in.OnKind), OnRef: strings.TrimSpace(in.OnRef), Reason: strings.TrimSpace(in.Reason)}, writeAuthor(o.principal))
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{"wait": wait})
}

func (o operatorTools) taskUnwait(ctx context.Context, _ *gmcp.CallToolRequest, in taskUnwaitInput) (*gmcp.CallToolResult, any, error) {
	if in.ID < 1 || in.WaitID < 1 {
		return nil, nil, errors.New("id and wait_id must be positive")
	}
	wait, changed, err := o.service.ClearTaskWait(ctx, in.ID, in.WaitID, writeAuthor(o.principal), strings.TrimSpace(in.Note))
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(map[string]any{"changed": changed, "wait": wait})
}

func (o operatorTools) waitsList(ctx context.Context, _ *gmcp.CallToolRequest, in waitsListInput) (*gmcp.CallToolResult, any, error) {
	view, err := o.service.ListWaits(ctx, time.Now().UTC(), strings.TrimSpace(in.Section))
	if err != nil {
		return nil, nil, err
	}
	return jsonResult(view)
}
