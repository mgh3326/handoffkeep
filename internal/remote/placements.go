package remote

import (
	"context"
	"net/url"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// PlacementList is GET /v1/placements.
type PlacementList struct {
	Placements []store.PlacementDecision `json:"placements"`
}

// PlacementFilter narrows GET /v1/placements. Zero fields are left to the
// server's defaults (the last 180 days, 100 rows); the server validates job
// and host.
type PlacementFilter struct {
	Job, Host string
	Since     time.Time
	Limit     int
}

// ListPlacements lists placement decisions, newest first.
func (c Client) ListPlacements(ctx context.Context, f PlacementFilter) (PlacementList, error) {
	q := url.Values{}
	for k, v := range map[string]string{"job": f.Job, "host": f.Host} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if !f.Since.IsZero() {
		q.Set("since", f.Since.UTC().Format(time.RFC3339Nano))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	path := "/v1/placements"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out PlacementList
	err := c.call(ctx, "GET", path, nil, &out)
	return out, err
}
