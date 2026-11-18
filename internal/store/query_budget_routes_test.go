package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// The N+1 guard for the store reads behind the main read routes (hk 5058):
// each runs over one row and over thirty, and its statement count must not
// grow with the rows. A per-row loop fails here whatever its fixed count is.
const guardRows = 30

// growTo returns a grow function that adds fixtures from build until the
// fixture holds n of them.
func growTo(build func(i int)) func(n int) {
	have := 0
	return func(n int) {
		for ; have < n; have++ {
			build(have)
		}
	}
}

func TestStatementsFlatDriftMergedWithoutDeploy(t *testing.T) {
	s, pool := searchTestStore(t)
	// Merged tasks with no PR are listed by merged_without_deploy and read no
	// per-task hold: the drift endpoint's batched class.
	grow := growTo(func(int) { flowTask(t, s, pool, "implement", StateMerged, false) })
	now := time.Now().UTC().Add(72 * time.Hour)
	testdb.AssertStatementsFlat(t, "/v1/tasks/drift", guardRows, 0, grow, func(ctx context.Context) error {
		_, err := s.TaskDrift(ctx, now, DriftDefaultMergedDays)
		return err
	})
}

// The deploy-record lookup of drift is batched per page: a page of PRs runs
// one statement, however many PRs it holds.
func TestStatementsFlatDriftDeployLookup(t *testing.T) {
	s, pool := searchTestStore(t)
	grow := growTo(func(i int) {
		x := flowTask(t, s, pool, "implement", StateMerged, false)
		setRefsPR(t, s, x.ID, testPR(9000+i))
	})
	now := time.Now().UTC().Add(72 * time.Hour)
	testdb.AssertStatementsFlat(t, "/v1/tasks/drift (deploy lookup)", guardRows, 0, grow, func(ctx context.Context) error {
		tx, err := s.beginReportRead(ctx, true)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		_, err = driftMergedWithoutDeploy(ctx, tx, now, DriftDefaultMergedDays)
		return err
	})
}

func TestStatementsFlatTaskGet(t *testing.T) {
	s, pool := searchTestStore(t)
	parent := flowTask(t, s, pool, "implement", StateDeveloping, false)
	grow := growTo(func(i int) {
		if _, err := s.CreateTask(context.Background(), Task{
			Lane: relaneLane(t, "child"), Title: fmt.Sprintf("child %d", i), Kind: "fix",
			CreatedBy: "tester", Project: projectPtr(testProjectName), ParentID: &parent.ID,
		}); err != nil {
			t.Fatal(err)
		}
	})
	testdb.AssertStatementsFlat(t, "/v1/tasks/{id}", guardRows, 0, grow, func(ctx context.Context) error {
		_, _, err := s.GetTask(ctx, parent.ID)
		return err
	})
}

func TestStatementsFlatConsoleAttention(t *testing.T) {
	s, pool := searchTestStore(t)
	options := DecisionOptions{Options: []DecisionOption{{Key: "A", Label: "A", Recommended: true}}}
	grow := growTo(func(i int) {
		task := flowTask(t, s, pool, "implement", StateDeveloping, false)
		if _, err := s.RecordDecisionRequest(context.Background(), task.ID, "tester", DecisionRequestInput{
			Question: fmt.Sprintf("Choose %d?", i), Options: options, DefaultAction: "hold", Block: true,
		}); err != nil {
			t.Fatal(err)
		}
	})
	now := time.Now().UTC()
	testdb.AssertStatementsFlat(t, "/ui/api/attention", guardRows, 0, grow, func(ctx context.Context) error {
		_, err := s.Attention(ctx, now)
		return err
	})
}

func TestStatementsFlatConsoleWorkStatus(t *testing.T) {
	s, pool := searchTestStore(t)
	grow := growTo(func(int) { flowTask(t, s, pool, "implement", StateDeveloping, true) })
	now := time.Now().UTC()
	testdb.AssertStatementsFlat(t, "/ui/api/work-status", guardRows, 0, grow, func(ctx context.Context) error {
		_, err := s.ReadWorkStatus(ctx, now)
		return err
	})
}
