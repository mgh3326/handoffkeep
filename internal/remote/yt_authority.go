package remote

import (
	"context"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// YTAuthorities reads GET /v1/yt/authority: every kind's canonical side.
func (c Client) YTAuthorities(ctx context.Context) ([]store.YTAuthority, error) {
	var out struct {
		Authority []store.YTAuthority `json:"authority"`
	}
	if err := c.call(ctx, "GET", "/v1/yt/authority", nil, &out); err != nil {
		return nil, err
	}
	return out.Authority, nil
}

// SetYTAuthority switches kinds through POST /v1/yt/authority and returns
// every kind's row before and after.
func (c Client) SetYTAuthority(ctx context.Context, kinds []store.YTAuthorityKind, side store.YTAuthoritySide, by, note string) (before, after []store.YTAuthority, err error) {
	body := map[string]any{"kinds": kinds, "authority": side, "by": by, "note": note}
	var out struct {
		Before []store.YTAuthority `json:"before"`
		After  []store.YTAuthority `json:"after"`
	}
	if err := c.call(ctx, "POST", "/v1/yt/authority", body, &out); err != nil {
		return nil, nil, err
	}
	return out.Before, out.After, nil
}
