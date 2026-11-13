package youtrack

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// StateChange is one change of an issue's state field, read from YouTrack's
// activity log: the time YouTrack recorded it, not the time hk noticed it.
// The console reads state entry and close times from these (hk 4439).
type StateChange struct {
	IssueID string
	At      time.Time
	// State is the hk state the issue moved into; StateKnown is false when
	// the new value maps to no hk state, and State then holds the raw value.
	State      string
	StateKnown bool
}

const (
	// stateChangePageSize is the $top of one /api/activities page.
	stateChangePageSize = 500
	// MaxStateChanges bounds one StateChanges read; past it the read fails
	// rather than showing a partial history as complete.
	MaxStateChanges = 50000
	// maxActivitiesScanned bounds the activities one read pages through, state
	// changes or not: the log also holds every other field change.
	maxActivitiesScanned = 200000
)

// stateChangeFields asks only for what a state change carries.
const stateChangeFields = "id,timestamp,field(id,name),added(name,$type),removed(name,$type),target(id,$type)"

type stateActivity struct {
	Timestamp int64          `json:"timestamp"`
	Field     *ActivityField `json:"field"`
	Added     []struct {
		Name string `json:"name"`
	} `json:"added"`
	Target *Issue `json:"target"`
}

// StateChanges returns every change of the state field (schema.StateField)
// recorded at or after since, in the order YouTrack lists them (oldest first). A change that clears the field,
// or that is on another field, is left out. A GET is idempotent, so a 5xx is
// retried like every other read.
func (c *Client) StateChanges(ctx context.Context, schema Schema, since time.Time) ([]StateChange, error) {
	var out []StateChange
	for skip := 0; ; skip += stateChangePageSize {
		q := url.Values{}
		q.Set("categories", "CustomFieldCategory")
		q.Set("fields", stateChangeFields)
		q.Set("start", strconv.FormatInt(since.UnixMilli(), 10))
		q.Set("$skip", strconv.Itoa(skip))
		q.Set("$top", strconv.Itoa(stateChangePageSize))
		var page []stateActivity
		if err := c.get(ctx, "/api/activities", q, &page); err != nil {
			return nil, err
		}
		for _, a := range page {
			if a.Field == nil || a.Field.Name != schema.StateField || a.Target == nil || len(a.Added) == 0 {
				continue
			}
			state, known := StateFromValue(a.Added[0].Name)
			if !known {
				state = a.Added[0].Name
			}
			out = append(out, StateChange{IssueID: a.Target.ID, At: msTime(a.Timestamp), State: state, StateKnown: known})
		}
		if len(out) > MaxStateChanges || skip+len(page) > maxActivitiesScanned {
			return nil, fmt.Errorf("more than %d state changes or %d activities since %s", MaxStateChanges, maxActivitiesScanned, since.Format(time.RFC3339))
		}
		if len(page) < stateChangePageSize {
			return out, nil
		}
	}
}
