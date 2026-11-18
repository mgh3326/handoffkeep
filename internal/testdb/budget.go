package testdb

import (
	"context"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

// AssertStatementsFlat is the N+1 guard for one read route (hk 5058). It runs
// read over a fixture of one row, then over a fixture of rows rows. grow(n)
// adds rows until the fixture holds n of what read covers. It fails when the
// larger run runs more than tolerance statements above the smaller one: a
// per-row loop adds a statement for each row, whatever the fixed count is.
// The per-request alarm (telemetry.QueryBudget) applies to the larger run too.
func AssertStatementsFlat(t testing.TB, route string, rows, tolerance int, grow func(n int), read func(ctx context.Context) error) {
	t.Helper()
	grow(1)
	small := countStatements(t, route, read)
	grow(rows)
	large := countStatements(t, route, read)
	if large-small > tolerance {
		t.Fatalf("%s runs %d statements for 1 row and %d for %d rows (tolerance %d): the count grows with its rows", route, small, large, rows, tolerance)
	}
	if large > telemetry.QueryBudget {
		t.Fatalf("%s runs %d statements for %d rows, over the per-request alarm of %d", route, large, rows, telemetry.QueryBudget)
	}
}

func countStatements(t testing.TB, route string, read func(ctx context.Context) error) int {
	t.Helper()
	ctx, count := telemetry.WithQueryCount(context.Background())
	if err := read(ctx); err != nil {
		t.Fatalf("%s: %v", route, err)
	}
	return count.Count()
}
