package remote

import (
	"context"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// RecordVerification posts the verdict for one head of a change and returns
// the stored row.
func (c Client) RecordVerification(ctx context.Context, in store.VerificationInput) (store.Verification, error) {
	var out store.Verification
	err := c.call(ctx, "POST", "/v1/changes/verifications", in, &out)
	return out, err
}

// ListChanges reads the changes for a PR URL (pr set) or a task (taskID set).
func (c Client) ListChanges(ctx context.Context, pr string, taskID int64) ([]store.Change, error) {
	q := url.Values{}
	if pr != "" {
		q.Set("pr", pr)
	} else {
		q.Set("task", strconv.FormatInt(taskID, 10))
	}
	var out struct {
		Changes []store.Change `json:"changes"`
	}
	err := c.call(ctx, "GET", "/v1/changes?"+q.Encode(), nil, &out)
	return out.Changes, err
}

// GetChange reads one PR's state (GET /v1/changes/one). include is the
// comma list of sections to return ("" for all); a PR hk has never seen is a
// not_found error (ErrorCode).
func (c Client) GetChange(ctx context.Context, repo string, pr int, allHeads bool, include string) (store.ChangeView, error) {
	q := url.Values{"repo": {repo}, "pr": {strconv.Itoa(pr)}}
	if allHeads {
		q.Set("all_heads", "1")
	}
	if include != "" {
		q.Set("include", include)
	}
	var out store.ChangeView
	err := c.call(ctx, "GET", "/v1/changes/one?"+q.Encode(), nil, &out)
	return out, err
}
