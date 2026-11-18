package remote

import (
	"context"
	"net/url"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// LaneLookResult is the answer to a look: whether this call wrote the row,
// and the look as it reads afterwards.
type LaneLookResult struct {
	Changed bool           `json:"changed"`
	Look    store.LaneLook `json:"look"`
}

// RecordLaneLook records a lane's look at one lane_check notice
// (POST /v1/lanes/{lane}/looks; operator decision 418, hk 5201). A repeat of
// the same notice changes nothing.
func (c Client) RecordLaneLook(ctx context.Context, lane, notice, note string) (LaneLookResult, error) {
	var out LaneLookResult
	err := c.call(ctx, "POST", "/v1/lanes/"+url.PathEscape(lane)+"/looks", struct {
		Notice string `json:"notice"`
		Note   string `json:"note,omitempty"`
	}{notice, note}, &out)
	if err != nil {
		return out, unsupportedRoute(err, "lane_look_unsupported", "the lane look route")
	}
	return out, nil
}
