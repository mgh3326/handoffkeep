package remote

import (
	"context"
	"net/url"
	"strconv"
	"time"

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

// ResolveYTOp records the operator's resolution through POST /v1/yt/ops/{id}/resolve.
func (c Client) ResolveYTOp(ctx context.Context, id int64, resolution ytwrite.Resolution) (ytwrite.Result, error) {
	var wire struct {
		OpID       int64          `json:"op_id"`
		Status     ytwrite.Status `json:"status"`
		State      ytwrite.State  `json:"state"`
		Reason     string         `json:"reason"`
		RetryAfter string         `json:"retry_after"`
		YTID       string         `json:"yt_id"`
	}
	// adopted is sent only when set, so a server that predates it still
	// takes an ordinary resolution.
	body := struct {
		Applied bool   `json:"applied"`
		YTID    string `json:"yt_id"`
		Adopted bool   `json:"adopted,omitempty"`
		By      string `json:"by"`
		Note    string `json:"note"`
	}{resolution.Applied, resolution.YTID, resolution.Adopted, resolution.By, resolution.Note}
	if err := c.call(ctx, "POST", "/v1/yt/ops/"+strconv.FormatInt(id, 10)+"/resolve", body, &wire); err != nil {
		return ytwrite.Result{}, err
	}
	retryAfter, err := time.ParseDuration(wire.RetryAfter)
	if err != nil {
		return ytwrite.Result{}, err
	}
	return ytwrite.Result{OpID: wire.OpID, Status: wire.Status, State: wire.State, Reason: wire.Reason, RetryAfter: retryAfter, YTID: wire.YTID}, nil
}
