package remote

import (
	"context"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// YTLinksStatus reads GET /v1/yt/links/status: the issue-link projection's
// state. task 0 is every task; limit 0 takes the server default.
func (c Client) YTLinksStatus(ctx context.Context, task int64, limit int) (store.YTLinksStatus, error) {
	q := url.Values{}
	if task > 0 {
		q.Set("task", strconv.FormatInt(task, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/yt/links/status"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out store.YTLinksStatus
	if err := c.call(ctx, "GET", path, nil, &out); err != nil {
		return store.YTLinksStatus{}, err
	}
	return out, nil
}
