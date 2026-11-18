package testdb

import (
	"context"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

// AssertQueryBudget runs read under a statement counter and fails the test
// when it runs more statements than one request may (telemetry.QueryBudget),
// naming the route. The main read routes use it, so a per-row query shows up
// as a failing test instead of as a slow production trace (hk 5058).
func AssertQueryBudget(t testing.TB, route string, read func(ctx context.Context) error) {
	t.Helper()
	ctx, count := telemetry.WithQueryCount(context.Background())
	if err := read(ctx); err != nil {
		t.Fatalf("%s: %v", route, err)
	}
	if n := count.Count(); n > telemetry.QueryBudget {
		t.Fatalf("%s ran %d statements, over the budget of %d", route, n, telemetry.QueryBudget)
	}
}
