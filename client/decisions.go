package client

import (
	"context"
	"fmt"
	"time"
)

// DecisionAskInput is a structured decision request put to the operator.
type DecisionAskInput struct {
	Question       string          `json:"question"`
	Options        DecisionOptions `json:"options"`
	Reason         string          `json:"reason,omitempty"`
	DefaultAction  string          `json:"default_action"`
	DefaultOption  string          `json:"default_option,omitempty"`
	DefaultTrigger string          `json:"default_trigger,omitempty"`
	DueAt          *time.Time      `json:"due_at,omitempty"`
	Doc            string          `json:"doc,omitempty"`
	Refs           []string        `json:"refs,omitempty"`
	// Supersedes is the ID of the open request this one replaces.
	Supersedes string `json:"supersedes,omitempty"`
	// Block also sets the task's awaiting_decision flag.
	Block bool `json:"block,omitempty"`
	// HumanOnly reserves the request for the human operator's own paths.
	HumanOnly bool `json:"human_only,omitempty"`
	// AnswerPolicy is operator_direct or operator_relay_allowed (empty).
	AnswerPolicy string `json:"answer_policy,omitempty"`
}

// DecisionOptions are a request's choices. AllowFree permits a free-form
// answer; the hk CLI sets it unless --no-free-answer is given.
type DecisionOptions struct {
	Options   []DecisionOption `json:"options"`
	AllowFree bool             `json:"allow_free"`
}

// DecisionOption is one choice, keyed A, B, ...
type DecisionOption struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Recommended bool   `json:"recommended,omitempty"`
}

// DecisionAskResult is the answer to AskDecision. Request.ID is the request's
// id and DecisionRef its structured reference: look the decision up by them,
// not by question text.
type DecisionAskResult struct {
	Task        Task            `json:"task"`
	Request     DecisionRequest `json:"request"`
	DecisionRef string          `json:"decision_ref"`
	Ver         string          `json:"ver"`
	// Duplicate is true when an identical open request already existed.
	Duplicate    bool `json:"duplicate"`
	IssueCreated bool `json:"issue_created"`
}

// AskDecision records a decision request on task taskID and gives it its
// Linear issue.
func (c Client) AskDecision(ctx context.Context, taskID int64, in DecisionAskInput) (DecisionAskResult, error) {
	var out DecisionAskResult
	err := c.Call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/decision-request/ask", taskID), in, &out)
	return out, err
}
