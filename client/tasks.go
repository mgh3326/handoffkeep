package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Task is a task as GET /v1/tasks/{id} answers it, with the fields the fleet
// reads. Unknown fields in the answer are ignored.
type Task struct {
	ID        int64     `json:"id"`
	Lane      string    `json:"lane"`
	Title     string    `json:"title"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	Priority  int       `json:"priority"`
	Refs      TaskRefs  `json:"refs"`
	ClaimedBy string    `json:"claimed_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Project is nil on a task with no project.
	Project *string `json:"project"`
	// Expected is the task's expected duration as a Go duration string
	// ("24h0m0s"); nil when unset.
	Expected *string `json:"expected"`
	// Category is derived from State by the server (started, completed, ...).
	Category string `json:"category"`
	// AwaitingDecision is set while the task waits on the operator.
	AwaitingDecision bool `json:"awaiting_decision"`
}

// TaskRefs are a task's typed references.
type TaskRefs struct {
	Closure *TaskClosure `json:"closure,omitempty"`
	PR      string       `json:"pr,omitempty"`
	JobID   string       `json:"job_id,omitempty"`
	// ReportPath is where the task's worker writes its report.
	ReportPath string `json:"report_path,omitempty"`
	// DecisionRequest is the task's current structured decision request.
	DecisionRequest *DecisionRequest `json:"decision_request,omitempty"`
}

// TaskClosure records how a closed task was closed.
type TaskClosure struct {
	Type     string `json:"type"`
	Evidence string `json:"evidence"`
}

// DecisionRequest is a task's current structured decision request. Look a
// request up by ID: a superseding request gets a new ID, and Root names the
// first revision of the same logical request.
type DecisionRequest struct {
	ID          string              `json:"id"`
	Revision    int                 `json:"revision"`
	Supersedes  string              `json:"supersedes,omitempty"`
	Root        string              `json:"root,omitempty"`
	Status      string              `json:"status"`
	Question    string              `json:"question"`
	DueAt       *time.Time          `json:"due_at,omitempty"`
	RequestedBy string              `json:"requested_by"`
	RequestedAt time.Time           `json:"requested_at"`
	HumanOnly   bool                `json:"human_only,omitempty"`
	Resolution  *DecisionResolution `json:"resolution,omitempty"`
}

// DecisionResolution is the answer that closed a decision request.
type DecisionResolution struct {
	Kind   string    `json:"kind"`
	Option string    `json:"option,omitempty"`
	Text   string    `json:"text,omitempty"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
}

// TaskComment is one comment on a task.
type TaskComment struct {
	ID        int64     `json:"id"`
	TaskID    int64     `json:"task_id"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// GetTask reads one task; found is false when the server answers not_found.
func (c Client) GetTask(ctx context.Context, id int64) (Task, bool, error) {
	var out Task
	err := c.transport().Call(ctx, "GET", fmt.Sprintf("/v1/tasks/%d", id), nil, &out)
	if ErrorCode(err) == "not_found" {
		return out, false, nil
	}
	return out, err == nil, err
}

// ListTasksInput filters ListTasks. An empty Lane, State or ParentLane
// matches every task. Limit must be 1 to 1000; the server refuses any other
// value. The answer stops at Limit tasks without saying whether more exist, so
// a full page may be a cut one.
type ListTasksInput struct {
	Lane, State, ParentLane string
	Limit                   int
}

// ListTasks lists tasks through GET /v1/tasks, as the CLI's tasks list does.
func (c Client) ListTasks(ctx context.Context, in ListTasksInput) ([]Task, error) {
	var out struct {
		Tasks []Task `json:"tasks"`
	}
	q := url.Values{"lane": {in.Lane}, "state": {in.State}, "parent_lane": {in.ParentLane}, "limit": {fmt.Sprint(in.Limit)}}
	if err := c.transport().Call(ctx, "GET", "/v1/tasks?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// ClaimTask claims a task for claimedBy, linking jobID, or recording noJob as
// the reason there is none. An answer that does not show the task started,
// claimed by claimedBy and linked to jobID is an error, as in the hk CLI.
func (c Client) ClaimTask(ctx context.Context, id int64, claimedBy, jobID, noJob string) (Task, error) {
	var out Task
	body := map[string]string{"claimed_by": claimedBy}
	if jobID != "" {
		body["job_id"] = jobID
	}
	if noJob != "" {
		body["no_job"] = noJob
	}
	if err := c.transport().Call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/claim", id), body, &out); err != nil {
		return out, err
	}
	if !activeTaskState(out.State) {
		return out, fmt.Errorf("claim_not_applied: server response has state=%q, want a started state (developing or researching; old servers: claimed)", out.State)
	}
	if out.ClaimedBy != claimedBy {
		return out, fmt.Errorf("claim_claimant_not_recorded: server response has claimed_by=%q, want %q", out.ClaimedBy, claimedBy)
	}
	if jobID != "" && out.Refs.JobID != jobID {
		return out, fmt.Errorf("claim_job_id_not_recorded: server response has refs.job_id=%q, want %q (server predates job_id claims)", out.Refs.JobID, jobID)
	}
	return out, nil
}

// RebindTaskJob moves an active task's job link from fromJob to toJob during
// a builder takeover by by.
func (c Client) RebindTaskJob(ctx context.Context, id int64, fromJob, toJob, by, reason string) (Task, error) {
	var out Task
	body := map[string]string{"from_job": fromJob, "to_job": toJob, "by": by, "reason": reason}
	err := c.transport().Call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/rebind", id), body, &out)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Code == "" && (he.Status == http.StatusNotFound || he.Status == http.StatusMethodNotAllowed) {
			return out, fmt.Errorf("task_rebind_route_missing: server does not provide POST /v1/tasks/{id}/rebind; minimum server version is a handoffkeep build from PR 1751 or newer")
		}
		return out, err
	}
	if !activeTaskState(out.State) || out.ClaimedBy != by || out.Refs.JobID != toJob {
		return out, fmt.Errorf("task_rebind_not_recorded: server response has state=%q claimed_by=%q refs.job_id=%q; want an active task claimed by %q with job_id=%q", out.State, out.ClaimedBy, out.Refs.JobID, by, toJob)
	}
	return out, nil
}

// ReopenTask reopens a closed task with reason.
func (c Client) ReopenTask(ctx context.Context, id int64, reason string) (Task, error) {
	var out Task
	err := c.transport().Call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/reopen", id), struct {
		Reason string `json:"reason"`
	}{reason}, &out)
	return out, err
}

// CommentTask appends a comment. Only the body is sent: the server derives
// the author from the bearer token. The route takes no idempotency key, so a
// retry after an unknown outcome can add a second comment.
func (c Client) CommentTask(ctx context.Context, id int64, body string) (TaskComment, error) {
	var out TaskComment
	err := c.transport().Call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/comments", id), map[string]string{"body": body}, &out)
	return out, err
}

// activeTaskState reports whether state is a started task state in either
// vocabulary: claimed and in_progress from a server that predates the
// eleven-state model, researching, developing and reviewing from a current one.
func activeTaskState(state string) bool {
	switch state {
	case "claimed", "in_progress", "researching", "developing", "reviewing":
		return true
	}
	return false
}
