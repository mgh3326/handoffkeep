package store

import (
	"context"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestReadRoutesWithinQueryBudget runs the store reads behind the main read
// routes over seeded data and asserts each stays within one request's budget
// (hk 5058). The data holds a page of merged PRs, so a per-row lookup would
// show.
func TestReadRoutesWithinQueryBudget(t *testing.T) {
	s, pool := searchTestStore(t)
	var first Task
	for i := range 40 {
		x := flowTask(t, s, pool, "implement", StateMerged, false)
		setRefsPR(t, s, x.ID, testPR(8000+i))
		if i == 0 {
			first = x
		}
	}
	_ = flowTask(t, s, pool, "implement", StateDeveloping, false)
	now := time.Now().UTC().Add(72 * time.Hour)

	testdb.AssertQueryBudget(t, "/v1/tasks/drift", func(ctx context.Context) error {
		_, err := s.TaskDrift(ctx, now, DriftDefaultMergedDays)
		return err
	})
	testdb.AssertQueryBudget(t, "/v1/tasks/{id}", func(ctx context.Context) error {
		_, _, err := s.GetTask(ctx, first.ID)
		return err
	})
	testdb.AssertQueryBudget(t, "/ui/api/attention", func(ctx context.Context) error {
		_, err := s.Attention(ctx, now)
		return err
	})
	testdb.AssertQueryBudget(t, "/ui/api/work-status", func(ctx context.Context) error {
		_, err := s.ReadWorkStatus(ctx, now)
		return err
	})
}
