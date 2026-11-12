package client

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// QuotaObservation is one stored account quota reading.
type QuotaObservation struct {
	ID          int64            `json:"id"`
	Kind        string           `json:"kind"`
	Window      string           `json:"window,omitempty"`
	MeasuredAt  time.Time        `json:"measured_at"`
	ConfirmedAt *time.Time       `json:"confirmed_at,omitempty"`
	ExpiresAt   *time.Time       `json:"expires_at,omitempty"`
	Plan        string           `json:"plan,omitempty"`
	Buckets     []QuotaBucket    `json:"buckets"`
	FreeBucket  *QuotaFreeBucket `json:"free_bucket,omitempty"`
}

// QuotaBucket is one percentage measurement in a quota observation.
type QuotaBucket struct {
	Window   string     `json:"window"`
	UsedPct  float64    `json:"used_pct"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	Scope    string     `json:"scope"`
	Model    string     `json:"model,omitempty"`
}

// QuotaFreeBucket describes the free-profile state carried by Devin readings.
type QuotaFreeBucket struct {
	State    string     `json:"state"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
}

// ListQuotaObservations returns one account's observations, oldest first.
// Since is required by the API; zero Until omits it so the server uses now.
// Limit must be 1 to 5000; the server defaults it to 500 when zero.
func (c Client) ListQuotaObservations(ctx context.Context, key string, since, until time.Time, limit int) ([]QuotaObservation, error) {
	q := url.Values{"since": {since.Format(time.RFC3339)}}
	if !until.IsZero() {
		q.Set("until", until.Format(time.RFC3339))
	}
	if limit != 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	var out []QuotaObservation
	err := c.transport().Call(ctx, "GET", "/v1/quota/accounts/"+url.PathEscape(key)+"/observations?"+q.Encode(), nil, &out)
	return out, err
}
