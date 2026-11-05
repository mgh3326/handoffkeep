package remote

import (
	"context"
	"fmt"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// ListBenchReps reads the newest bench reps, newest first (GET /v1/bench/reps).
func (c Client) ListBenchReps(ctx context.Context, limit int) ([]store.BenchRep, error) {
	var out struct {
		Reps []store.BenchRep `json:"reps"`
	}
	err := c.call(ctx, "GET", fmt.Sprintf("/v1/bench/reps?limit=%d", limit), nil, &out)
	return out.Reps, err
}
