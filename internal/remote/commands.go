package remote

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ListOperatorCommands reads one executor lane's commands in id order
// (GET /v1/commands). An empty state lists every state.
func (c Client) ListOperatorCommands(ctx context.Context, lane, state string, afterID int64, limit int) ([]store.OperatorCommand, error) {
	var out struct {
		Commands []store.OperatorCommand `json:"commands"`
	}
	q := url.Values{"executor_lane": {lane}, "after_id": {strconv.FormatInt(afterID, 10)}, "limit": {strconv.Itoa(limit)}}
	if state != "" {
		q.Set("state", state)
	}
	err := c.call(ctx, "GET", "/v1/commands?"+q.Encode(), nil, &out)
	return out.Commands, err
}

// OperatorCommandReceipt reports an executor's ack, done or failure for one
// command (POST /v1/commands/{id}/receipt).
func (c Client) OperatorCommandReceipt(ctx context.Context, id int64, in store.OperatorCommandReceiptInput) (store.OperatorCommand, error) {
	var out store.OperatorCommand
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/commands/%d/receipt", id), in, &out)
	return out, err
}
