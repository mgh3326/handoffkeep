package remote

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

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
