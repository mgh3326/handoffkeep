package remote

import (
	"context"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// ListYTOps reads GET /v1/yt/ops: the YouTrack write ops the operator watches.
// next is the after_id of the next page, 0 at the end.
func (c Client) ListYTOps(ctx context.Context, state string, taskID, afterID int64, limit int) ([]ytwrite.OpView, int64, error) {
	q := url.Values{}
	if state != "" {
		q.Set("state", state)
	}
	if taskID > 0 {
		q.Set("task", strconv.FormatInt(taskID, 10))
	}
	if afterID > 0 {
		q.Set("after_id", strconv.FormatInt(afterID, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out struct {
		Ops  []ytwrite.OpView `json:"ops"`
		Next *int64           `json:"next_after_id"`
	}
	if err := c.call(ctx, "GET", "/v1/yt/ops?"+q.Encode(), nil, &out); err != nil {
		return nil, 0, err
	}
	var next int64
	if out.Next != nil {
		next = *out.Next
	}
	return out.Ops, next, nil
}
