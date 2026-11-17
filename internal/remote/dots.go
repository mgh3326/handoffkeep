package remote

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/capabilities"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// CreateDotsRequest files a request in the dots lane.
func (c Client) CreateDotsRequest(ctx context.Context, in store.DotsRequestInput) (store.Task, error) {
	var out struct {
		Task store.Task `json:"task"`
	}
	err := c.call(ctx, "POST", "/v1/dots/requests", in, &out)
	if err != nil {
		return store.Task{}, unsupportedRoute(err, "dots_requests_unsupported", "the dots requests route")
	}
	return out.Task, nil
}

// CancelDotsRequest settles the caller lane's own dots request as canceled
// with a reason (hk 4987).
func (c Client) CancelDotsRequest(ctx context.Context, taskID int64, in store.DotsCancelInput) (store.Task, error) {
	var out struct {
		Task store.Task `json:"task"`
	}
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/dots/requests/%d/cancel", taskID), in, &out)
	if err != nil {
		return store.Task{}, unsupportedRoute(err, "dots_cancel_unsupported", "the dots cancel route")
	}
	return out.Task, nil
}

// DotsAdoptionResult is the answer to SetDotsAdoption: the stored result and
// whether this call changed it (false: the same change was already stored).
type DotsAdoptionResult struct {
	Result  store.DotsResult `json:"result"`
	Changed bool             `json:"changed"`
}

// ListDotsResults lists the dots results of one task (taskID > 0) or of one
// Linear issue identifier, newest first.
func (c Client) ListDotsResults(ctx context.Context, taskID int64, issue string) ([]store.DotsResult, error) {
	var out struct {
		Results []store.DotsResult `json:"results"`
	}
	p := url.Values{}
	if taskID > 0 {
		p.Set("task", strconv.FormatInt(taskID, 10))
	}
	if issue != "" {
		p.Set("issue", issue)
	}
	err := c.call(ctx, "GET", "/v1/dots/results?"+p.Encode(), nil, &out)
	if err != nil {
		return nil, unsupportedRoute(err, "dots_results_unsupported", "the dots results route")
	}
	return out.Results, nil
}

// SetDotsAdoption records what became of a dots result.
func (c Client) SetDotsAdoption(ctx context.Context, id int64, in store.DotsAdoptionInput) (DotsAdoptionResult, error) {
	var out DotsAdoptionResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/dots/results/%d/adoption", id), in, &out)
	if err != nil {
		return out, unsupportedRoute(err, "dots_results_unsupported", "the dots results route")
	}
	return out, nil
}

// AnswerDots answers a task's open dots question and records the comment.
// RequesterLane is required when the answer closes a plain QUESTION, and
// QuestionID names the QUESTION reply to close when more than one is open.
// Both fields ride only a server whose /v1/capabilities lists
// FeatureDotsAnswerQuestion; the lane is omitted on an older server so a
// decision-path answer still works there, and a target is refused locally
// because an older server cannot honour it.
func (c Client) AnswerDots(ctx context.Context, taskID int64, text, requesterLane string, questionID int64) (store.DotsAnswerResult, error) {
	var out store.DotsAnswerResult
	payload := map[string]any{"task": taskID, "text": text}
	listed := false
	if questionID > 0 {
		if err := c.requireListedFeature(ctx, capabilities.FeatureDotsAnswerQuestion); err != nil {
			return out, err
		}
		listed = true
		payload["question_id"] = questionID
	}
	if requesterLane != "" {
		if !listed {
			// A failed capabilities read must not fail an answer the decision
			// path can still take: the lane is optional there, and omitting it
			// leaves a plain answer to the server's own refusal.
			if ok, err := c.supportsFeature(ctx, capabilities.FeatureDotsAnswerQuestion); err == nil {
				listed = ok
			}
		}
		if listed {
			payload["requester_lane"] = requesterLane
		}
	}
	err := c.call(ctx, "POST", "/v1/dots/answer", payload, &out)
	if err != nil {
		return out, unsupportedRoute(err, "dots_answer_unsupported", "the dots answer route")
	}
	return out, nil
}
