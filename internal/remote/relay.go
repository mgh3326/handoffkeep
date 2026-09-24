package remote

import (
	"context"
	"fmt"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ListRelayEventsPage reads one page of relay events with id > afterID in id
// order (GET /v1/relay/events). An empty kind lists every kind.
func (c Client) ListRelayEventsPage(ctx context.Context, kind string, afterID int64, limit int) ([]store.RelayEvent, error) {
	var out struct {
		Events []store.RelayEvent `json:"events"`
	}
	q := url.Values{"after_id": {fmt.Sprint(afterID)}, "limit": {fmt.Sprint(limit)}}
	if kind != "" {
		q.Set("kind", kind)
	}
	err := c.call(ctx, "GET", "/v1/relay/events?"+q.Encode(), nil, &out)
	return out.Events, err
}
