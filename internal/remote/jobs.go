package remote

import (
	"context"
	"github.com/mgh3326/handoffkeep/internal/store"
	"net/url"
	"strconv"
	"time"
)

func (c Client) JobSkillsReport(ctx context.Context, since time.Time, limit int) (store.JobSkillsReport, error) {
	var out store.JobSkillsReport
	q := url.Values{"since": {since.Format(time.RFC3339)}, "limit": {strconv.Itoa(limit)}}
	err := c.call(ctx, "GET", "/v1/job-skills?"+q.Encode(), nil, &out)
	return out, err
}
