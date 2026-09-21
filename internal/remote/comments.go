package remote

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// CreateTaskComment sends only the body; the server derives the author from
// the bearer token.
func (c Client) CreateTaskComment(ctx context.Context, taskID int64, body string) (store.TaskComment, error) {
	var out store.TaskComment
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/comments", taskID), map[string]string{"body": body}, &out)
	return out, err
}
func (c Client) ListTaskComments(ctx context.Context, taskID, afterID int64, limit int) ([]store.TaskComment, error) {
	var out struct {
		Comments []store.TaskComment `json:"comments"`
	}
	q := url.Values{"after_id": {fmt.Sprint(afterID)}, "limit": {fmt.Sprint(limit)}}
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/tasks/%d/comments?", taskID)+q.Encode(), nil, &out)
	return out.Comments, err
}
