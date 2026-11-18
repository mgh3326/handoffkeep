package remote

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// CreateTaskOrder records the operator's "set going" order for a task. Only
// the operator token may call it; the server derives the actor and the channel
// from the token. The idempotency key is required: the same key with the same
// words replays the first order.
func (c Client) CreateTaskOrder(ctx context.Context, taskID int64, words, key string) (store.TaskOrderResult, error) {
	var out store.TaskOrderResult
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/tasks/%d/orders", taskID), map[string]string{"words": words, "idempotency_key": key}, &out)
	return out, err
}

// ListTaskOrders lists a task's orders, only the ones still waiting on their
// lane when openOnly is set.
func (c Client) ListTaskOrders(ctx context.Context, taskID int64, openOnly bool) ([]store.TaskOrder, error) {
	var out struct {
		Orders []store.TaskOrder `json:"orders"`
	}
	q := url.Values{}
	if openOnly {
		q.Set("open", "1")
	}
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/tasks/%d/orders?", taskID)+q.Encode(), nil, &out)
	return out.Orders, err
}

// AckTaskOrder acknowledges an order as the calling lane.
func (c Client) AckTaskOrder(ctx context.Context, orderID, comment string) (store.TaskOrderResult, error) {
	var out store.TaskOrderResult
	err := c.call(ctx, "POST", "/v1/task-orders/"+esc(orderID)+"/ack", map[string]string{"comment": comment}, &out)
	return out, err
}

// WithdrawTaskOrder withdraws an open order. Only the operator token may call it.
func (c Client) WithdrawTaskOrder(ctx context.Context, orderID, reason string) (store.TaskOrderResult, error) {
	var out store.TaskOrderResult
	err := c.call(ctx, "POST", "/v1/task-orders/"+esc(orderID)+"/withdraw", map[string]string{"reason": reason}, &out)
	return out, err
}
