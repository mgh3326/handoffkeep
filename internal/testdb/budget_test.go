package testdb

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

// recorder stands in for the test: it keeps the first failure instead of
// stopping the test, so one call can be checked for both outcomes.
type recorder struct {
	testing.TB
	failed string
}

func (r *recorder) Helper() {}
func (r *recorder) Fatalf(format string, args ...any) {
	if r.failed == "" {
		r.failed = fmt.Sprintf(format, args...)
	}
}

// statements issues n statements through the tracer, which counts them under
// a WithQueryCount context whether or not a span is on.
func statements(ctx context.Context, n int) {
	for range n {
		telemetry.QueryTracer{}.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT 1 FROM tasks"})
	}
}

// The guard passes a read whose statements do not grow with its rows, fails
// one that issues a statement per row (a deliberate N+1), and reports the
// per-request alarm for a fixed count over telemetry.QueryBudget.
func TestAssertStatementsFlatDetectsPerRowLoops(t *testing.T) {
	rows := 0
	grow := func(n int) { rows = n }
	for _, tc := range []struct {
		name string
		read func(ctx context.Context) error
		want string
	}{
		{"batched", func(ctx context.Context) error { statements(ctx, 2); return nil }, ""},
		{"per-row loop", func(ctx context.Context) error { statements(ctx, 1+rows); return nil }, "grows with its rows"},
		{"over the alarm", func(ctx context.Context) error { statements(ctx, telemetry.QueryBudget+1); return nil }, "per-request alarm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{TB: t}
			AssertStatementsFlat(r, "/v1/test", 30, 0, grow, tc.read)
			switch {
			case tc.want == "" && r.failed != "":
				t.Fatalf("unexpected failure: %s", r.failed)
			case tc.want != "" && !strings.Contains(r.failed, tc.want):
				t.Fatalf("failure %q, want it to mention %q", r.failed, tc.want)
			}
		})
	}
}
