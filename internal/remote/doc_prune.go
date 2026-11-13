package remote

import (
	"context"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// PruneDocuments runs POST /v1/documents/prune: a dry run unless apply is
// true. It deletes review/mwr- documents last updated more than olderThanDays
// days ago, at most limit of them.
func (c Client) PruneDocuments(ctx context.Context, prefix string, olderThanDays, limit int, apply bool) (store.DocPruneResult, error) {
	var out store.DocPruneResult
	body := map[string]any{"prefix": prefix, "older_than_days": olderThanDays, "limit": limit, "apply": apply}
	err := c.call(ctx, "POST", "/v1/documents/prune", body, &out)
	return out, err
}
