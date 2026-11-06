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
