package remote

import (
	"context"
	"errors"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// ListYTOps reads GET /v1/yt/ops: the YouTrack write ops the operator watches.
// next is the after_id of the next page, 0 at the end.
func (c Client) ListYTOps(ctx context.Context, state string, taskID, afterID int64, limit int) ([]ytwrite.OpView, int64, error) {
	return nil, 0, errors.New("not implemented")
}
