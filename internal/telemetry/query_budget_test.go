package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// budgetMux serves one route that runs n statements through the tracer, with
// no database: TraceQueryStart counts a statement whether or not a span is on.
func budgetMux(n int) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tasks/{id}/many", func(w http.ResponseWriter, r *http.Request) {
		for range n {
			QueryTracer{}.TraceQueryStart(r.Context(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1 FROM tasks"})
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	return mux
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf
}

// A request over QueryBudget logs one db.query_budget_exceeded line that names
// the route template, the count and the budget. The route template comes from
// the mux, never the URL.
func TestQueryBudgetLogsRouteOverBudget(t *testing.T) {
	logs := captureLog(t)
	h := (&Monitor{}).Handler(budgetMux(QueryBudget + 1))
	serve(h, httptest.NewRequest(http.MethodGet, "/v1/tasks/7/many", nil))
	want := fmt.Sprintf(`db.query_budget_exceeded route="/v1/tasks/{id}/many" count=%d budget=%d`, QueryBudget+1, QueryBudget)
	if got := logs.String(); !strings.Contains(got, want) {
		t.Fatalf("log %q, want a line with %q", got, want)
	}
}

// A request at the budget logs nothing, and each request counts alone: the
// count of an earlier request never adds to a later one.
func TestQueryBudgetCountsEachRequestAlone(t *testing.T) {
	logs := captureLog(t)
	h := (&Monitor{}).Handler(budgetMux(QueryBudget))
	for range 2 {
		serve(h, httptest.NewRequest(http.MethodGet, "/v1/tasks/7/many", nil))
	}
	if got := logs.String(); got != "" {
		t.Fatalf("at the budget, log %q, want none", got)
	}
}

// WithQueryCount counts every statement run under its context, traced or not.
func TestQueryCountCountsEveryStatement(t *testing.T) {
	conn := protocolConn(t)
	ctx, count := WithQueryCount(t.Context())
	for range 3 {
		if _, err := conn.Exec(ctx, "SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := count.Count(); got != 3 {
		t.Fatalf("count=%d, want 3", got)
	}
	// A context without a count is not counted.
	if _, err := conn.Exec(t.Context(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if got := count.Count(); got != 3 {
		t.Fatalf("uncounted statement changed count to %d", got)
	}
}

// Every query span carries its label: the explicit one when Label names it,
// else the verb and first table. The span text stays normalized and literal-free.
func TestQuerySpanCarriesLabel(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	ctx, parent := provider.Tracer("test").Start(context.Background(), "server")
	defer parent.End()

	tracer := QueryTracer{}
	run := func(ctx context.Context, sql string) {
		ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: sql, Args: []any{"secret-arg"}})
		tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
	}
	run(ctx, "SELECT 'secret-literal' FROM tasks WHERE id = $1")
	run(Label(ctx, "task_drift.merged_page"), "SELECT id FROM tasks")
	run(Label(ctx, "Not A Label!"), "SELECT id FROM task_events")

	labels := map[string]string{}
	for _, span := range exporter.GetSpans() {
		if span.Name != "db.sql.query" {
			continue
		}
		labels[attrString(span.Attributes, "db.query.text")] = attrString(span.Attributes, "db.query.label")
	}
	want := map[string]string{
		"SELECT ? FROM tasks WHERE id = ?": "select tasks",
		"SELECT id FROM tasks":             "task_drift.merged_page",
		"SELECT id FROM task_events":       "select task_events",
	}
	for text, label := range want {
		if got := labels[text]; got != label {
			t.Fatalf("label for %q = %q, want %q (all: %v)", text, got, label, labels)
		}
	}
	for _, span := range exporter.GetSpans() {
		for _, kv := range span.Attributes {
			if strings.Contains(kv.Value.Emit(), "secret") && kv.Key != "db.query.text" {
				t.Fatalf("span attribute %s carries request content: %q", kv.Key, kv.Value.Emit())
			}
		}
	}
}

func TestShapeLabel(t *testing.T) {
	for _, tc := range []struct{ sql, want string }{
		{"SELECT id FROM tasks WHERE id=$1", "select tasks"},
		{`SELECT id FROM "tasks" WHERE id=$1`, "select tasks"},
		{"UPDATE tasks SET state=$1", "update tasks"},
		{"INSERT INTO task_events (task_id) VALUES ($1)", "insert task_events"},
		{"SELECT 1", "select"},
		{"", "statement"},
		// A quoted name that carries more than an identifier yields no table.
		{`SELECT x FROM "secret-name"`, "select"},
	} {
		if got := shapeLabel(tc.sql); got != tc.want {
			t.Fatalf("shapeLabel(%q)=%q, want %q", tc.sql, got, tc.want)
		}
	}
}

func TestLabelRejectsInvalidNames(t *testing.T) {
	for _, name := range []string{"", "Upper", "has space", "secret value", strings.Repeat("a", 65)} {
		ctx := Label(context.Background(), name)
		if got := queryLabel(ctx, "SELECT 1 FROM tasks"); got != "select tasks" {
			t.Fatalf("Label(%q) changed the label to %q", name, got)
		}
	}
}
