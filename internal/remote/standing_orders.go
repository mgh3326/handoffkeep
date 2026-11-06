package remote

import (
	"context"
	"github.com/mgh3326/handoffkeep/internal/store"
	"net/url"
	"time"
)

type StandingOrderResult struct {
	store.StandingOrder
	Duplicate bool `json:"duplicate,omitempty"`
}
type StandingOrderList struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Orders      []store.StandingOrder `json:"orders"`
	Truncated   bool                  `json:"truncated"`
}

func (c Client) PutStandingOrder(ctx context.Context, in store.StandingOrderInput) (StandingOrderResult, error) {
	var out StandingOrderResult
	err := c.call(ctx, "POST", "/v1/standing-orders", in, &out)
	return out, err
}
func (c Client) ListStandingOrders(ctx context.Context, project, lane string) (StandingOrderList, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if lane != "" {
		q.Set("lane", lane)
	}
	var out StandingOrderList
	err := c.call(ctx, "GET", "/v1/standing-orders?"+q.Encode(), nil, &out)
	return out, err
}
