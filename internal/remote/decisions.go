package remote

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// DecisionRecordList is GET /v1/decision-requests.
type DecisionRecordList struct {
	ServerTime       time.Time              `json:"server_time"`
	DecisionRequests []store.DecisionRecord `json:"decision_requests"`
}

// ListDecisionRecords lists decision records. Empty filter fields are left
// to the server's defaults (open, not snoozed).
func (c Client) ListDecisionRecords(ctx context.Context, f store.DecisionRecordFilter) (DecisionRecordList, error) {
	q := url.Values{}
	for k, v := range map[string]string{"status": f.Status, "lane": f.Lane, "source": f.Source} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.IncludeSnoozed {
		q.Set("include_snoozed", "true")
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	path := "/v1/decision-requests"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out DecisionRecordList
	err := c.call(ctx, "GET", path, nil, &out)
	return out, err
}

// SnoozeDecisionRecord hides an open record from the default list until
// until. The question's source is not touched.
func (c Client) SnoozeDecisionRecord(ctx context.Context, id int64, until time.Time) (store.DecisionRecord, error) {
	var out store.DecisionRecord
	err := c.call(ctx, "POST", fmt.Sprintf("/v1/decision-requests/%d/snooze", id), map[string]time.Time{"until": until.UTC()}, &out)
	return out, err
}

// OpenDecisionSources reads every question each source counts as open. It is
// a GET: the dry-run backfill is built on it so it can run with a normal
// client credential and writes nothing.
func (c Client) OpenDecisionSources(ctx context.Context) (store.OpenDecisionSources, error) {
	var out store.OpenDecisionSources
	err := c.call(ctx, "GET", "/v1/decision-requests/sources", nil, &out)
	return out, err
}

// DecisionDeliveryHygiene reads the decision delivery counts per destination.
func (c Client) DecisionDeliveryHygiene(ctx context.Context) (store.DecisionDeliveryHygiene, error) {
	var out store.DecisionDeliveryHygiene
	err := c.call(ctx, "GET", "/v1/decisions/deliveries/hygiene", nil, &out)
	return out, err
}
