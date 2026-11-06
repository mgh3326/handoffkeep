package remote

import (
	"context"
	"github.com/mgh3326/handoffkeep/internal/store"
	"net/url"
	"time"
)

func (c Client) ProjectFrontier(ctx context.Context, project string, since time.Time) (store.Frontier, error) {
	path := "/v1/projects/" + url.PathEscape(project) + "/frontier"
	if !since.IsZero() {
		path += "?" + url.Values{"since": {since.Format(time.RFC3339)}}.Encode()
	}
	var out store.Frontier
	err := c.call(ctx, "GET", path, nil, &out)
	return out, err
}
