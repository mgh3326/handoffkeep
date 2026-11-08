package telemetry

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// otlpTestMonitor enables OTLP tracing with a fake endpoint and token. The
// exporter is an in-memory or failing one, so the endpoint is never dialed.
func otlpTestMonitor(t *testing.T, rate string, spans sdktrace.SpanExporter) *Monitor {
	t.Helper()
	t.Setenv("HANDOFFKEEP_SENTRY_DSN", "")
	t.Setenv("HANDOFFKEEP_OTLP_ENDPOINT", "https://otlp.example.test/otlp")
	t.Setenv("HANDOFFKEEP_OTLP_INSTANCE_ID", "123")
	t.Setenv("HANDOFFKEEP_OTLP_TOKEN", "test-token")
	t.Setenv("HANDOFFKEEP_OTLP_SAMPLE_RATE", rate)
	monitor, err := monitorFromEnv("test-revision", nil, spans)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(monitor.Close)
	return monitor
}

// otlpTestMux serves one route in scope, one out of scope, and routes that
// fail, panic, or run a query. Handlers write no request content.
func otlpTestMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /v1/tasks/{id}/fail", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal_error"}`))
	})
	mux.HandleFunc("GET /v1/tasks/{id}/panic", func(http.ResponseWriter, *http.Request) {
		panic("secret-panic")
	})
	mux.HandleFunc("GET /v1/tasks/{id}/query", func(w http.ResponseWriter, r *http.Request) {
		ctx := QueryTracer{}.TraceQueryStart(r.Context(), nil, pgx.TraceQueryStartData{
			SQL:  "SELECT 'secret-literal' FROM tasks WHERE id = $1",
			Args: []any{"secret-arg"},
		})
		QueryTracer{}.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /v1/documents", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

func flushedSpans(t *testing.T, m *Monitor, exporter *tracetest.InMemoryExporter) tracetest.SpanStubs {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.otlp.provider.ForceFlush(ctx); err != nil {
		t.Fatal(err)
	}
	return exporter.GetSpans()
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOTLPDisabledByDefault(t *testing.T) {
	t.Setenv("HANDOFFKEEP_SENTRY_DSN", "")
	t.Setenv("HANDOFFKEEP_OTLP_ENDPOINT", "")
	// Other OTLP settings are ignored while the endpoint is unset, as the
	// Sentry DSN rule is: no exporter, no network call, no error.
	t.Setenv("HANDOFFKEEP_OTLP_SAMPLE_RATE", "invalid but disabled")
	t.Setenv("HANDOFFKEEP_OTLP_ROUTES", "unknown but disabled")
	monitor, err := FromEnv("test-revision")
	if err != nil || monitor.otlp != nil || monitor.client != nil {
		t.Fatalf("disabled monitor=%+v err=%v", monitor, err)
	}
	if got := TraceID(context.Background()); got != "" {
		t.Fatalf("trace id without a span = %q", got)
	}
}

func TestOTLPSettingsParsing(t *testing.T) {
	valid := map[string]string{
		"HANDOFFKEEP_OTLP_ENDPOINT":    "https://otlp.example.test/otlp/",
		"HANDOFFKEEP_OTLP_INSTANCE_ID": "123",
		"HANDOFFKEEP_OTLP_TOKEN":       "secret-token",
		// Empty means the default, so an operator's shell cannot change the defaults under test.
		"HANDOFFKEEP_OTLP_SAMPLE_RATE": "",
		"HANDOFFKEEP_OTLP_ROUTES":      "",
	}
	setEnv := func(t *testing.T, overrides map[string]string) {
		t.Helper()
		for key, value := range valid {
			t.Setenv(key, value)
		}
		for key, value := range overrides {
			t.Setenv(key, value)
		}
	}
	t.Run("defaults", func(t *testing.T) {
		setEnv(t, nil)
		settings, err := otlpSettingsFromEnv("staging", "rev-1")
		if err != nil || settings == nil {
			t.Fatalf("settings=%v err=%v", settings, err)
		}
		if settings.tracesURL != "https://otlp.example.test/otlp/v1/traces" || settings.sampleRate != otlpDefaultRate {
			t.Fatalf("tracesURL=%q rate=%v", settings.tracesURL, settings.sampleRate)
		}
		if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("123:secret-token")); settings.authorization != want {
			t.Fatalf("authorization header is not the basic credential")
		}
		if settings.environment != "staging" || settings.release != "rev-1" {
			t.Fatalf("environment=%q release=%q", settings.environment, settings.release)
		}
		for path, want := range map[string]bool{"/v1/tasks": true, "/v1/tasks/{id}": true, "/v1/tasks/{id}/comments": true, "/v1/tasksets": false, "/v1/documents": false, "": false} {
			if got := settings.routes(path); got != want {
				t.Fatalf("route %q in scope=%v", path, got)
			}
		}
	})
	for _, tc := range []struct {
		name      string
		overrides map[string]string
	}{
		{"plain http", map[string]string{"HANDOFFKEEP_OTLP_ENDPOINT": "http://otlp.example.test/otlp"}},
		{"credentials in URL", map[string]string{"HANDOFFKEEP_OTLP_ENDPOINT": "https://user:pw@otlp.example.test/otlp"}},
		{"query", map[string]string{"HANDOFFKEEP_OTLP_ENDPOINT": "https://otlp.example.test/otlp?key=1"}},
		{"no host", map[string]string{"HANDOFFKEEP_OTLP_ENDPOINT": "https:///otlp"}},
		{"missing instance", map[string]string{"HANDOFFKEEP_OTLP_INSTANCE_ID": ""}},
		{"missing token", map[string]string{"HANDOFFKEEP_OTLP_TOKEN": " "}},
		{"colon in instance", map[string]string{"HANDOFFKEEP_OTLP_INSTANCE_ID": "1:2"}},
		{"rate above one", map[string]string{"HANDOFFKEEP_OTLP_SAMPLE_RATE": "1.5"}},
		{"rate negative", map[string]string{"HANDOFFKEEP_OTLP_SAMPLE_RATE": "-0.1"}},
		{"rate NaN", map[string]string{"HANDOFFKEEP_OTLP_SAMPLE_RATE": "NaN"}},
		{"rate Inf", map[string]string{"HANDOFFKEEP_OTLP_SAMPLE_RATE": "+Inf"}},
		{"rate text", map[string]string{"HANDOFFKEEP_OTLP_SAMPLE_RATE": "half"}},
		{"unknown route group", map[string]string{"HANDOFFKEEP_OTLP_ROUTES": "all"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.overrides)
			settings, err := otlpSettingsFromEnv("production", "rev")
			if err == nil || settings != nil {
				t.Fatalf("settings=%v err=%v, want an error", settings, err)
			}
			// Errors name the variable, never a value: the token is a secret.
			if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "otlp.example.test") {
				t.Fatalf("error leaks configuration: %v", err)
			}
		})
	}
}

func TestOTLPRouteFilterAndServerSpan(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	monitor := otlpTestMonitor(t, "1", spans)
	h := monitor.Handler(otlpTestMux())
	for _, path := range []string{"/v1/tasks/7", "/v1/documents", "/healthz"} {
		if w := serve(h, httptest.NewRequest(http.MethodGet, path, nil)); w.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, w.Code)
		}
	}
	got := flushedSpans(t, monitor, spans)
	if len(got) != 1 || got[0].Name != "GET /v1/tasks/{id}" || got[0].SpanKind != trace.SpanKindServer {
		t.Fatalf("spans=%v, want one server span for the task route", names(got))
	}
	if status := attrInt(got[0].Attributes, "http.response.status_code"); status != http.StatusOK {
		t.Fatalf("status attribute=%d", status)
	}
}

func TestOTLPStatusAndPanicSpans(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	monitor := otlpTestMonitor(t, "1", spans)
	h := monitor.Handler(otlpTestMux())
	if w := serve(h, httptest.NewRequest(http.MethodGet, "/v1/tasks/7/fail", nil)); w.Code != http.StatusInternalServerError {
		t.Fatalf("fail status=%d", w.Code)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic was swallowed; net/http semantics need it to propagate")
			}
		}()
		serve(h, httptest.NewRequest(http.MethodGet, "/v1/tasks/7/panic", nil))
	}()
	got := flushedSpans(t, monitor, spans)
	if len(got) != 2 {
		t.Fatalf("spans=%v, want two", names(got))
	}
	for _, span := range got {
		if span.Status.Code != codes.Error {
			t.Fatalf("span %q status=%v, want error", span.Name, span.Status.Code)
		}
		if status := attrInt(span.Attributes, "http.response.status_code"); status != http.StatusInternalServerError {
			t.Fatalf("span %q status attribute=%d", span.Name, status)
		}
	}
}

// Nothing a client sends or a query carries reaches a span: no URL, query,
// header, body, path value, or SQL literal or argument.
func TestOTLPSpansCarryNoRequestContent(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	monitor := otlpTestMonitor(t, "1", spans)
	h := monitor.Handler(otlpTestMux())
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/secret-path/query?token=secret-query", strings.NewReader(`{"body":"secret-body"}`))
	req.Header.Set("Authorization", "Bearer secret-header")
	req.Header.Set("Cookie", "session=secret-cookie")
	if w := serve(h, req); w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	got := flushedSpans(t, monitor, spans)
	if len(got) != 2 {
		t.Fatalf("spans=%v, want the server span and one query span", names(got))
	}
	for _, span := range got {
		for _, value := range spanStrings(span) {
			if strings.Contains(value, "secret") {
				t.Fatalf("span %q carries request content: %q", span.Name, value)
			}
		}
	}
	for _, span := range got {
		switch span.Name {
		case "GET /v1/tasks/{id}/query":
			for _, kv := range span.Attributes {
				if !map[string]bool{"http.request.method": true, "http.route": true, "http.response.status_code": true}[string(kv.Key)] {
					t.Fatalf("server span has unexpected attribute %q", kv.Key)
				}
			}
		case "db.sql.query":
			if got := attrString(span.Attributes, "db.query.text"); got != "SELECT ? FROM tasks WHERE id = ?" {
				t.Fatalf("db.query.text=%q", got)
			}
		default:
			t.Fatalf("unexpected span %q", span.Name)
		}
	}
}

// The query span is a child of the request's server span, so one trace holds
// both.
func TestOTLPQuerySpanIsChildOfServerSpan(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	monitor := otlpTestMonitor(t, "1", spans)
	h := monitor.Handler(otlpTestMux())
	serve(h, httptest.NewRequest(http.MethodGet, "/v1/tasks/7/query", nil))
	got := flushedSpans(t, monitor, spans)
	var server, query *tracetest.SpanStub
	for i := range got {
		switch got[i].Name {
		case "GET /v1/tasks/{id}/query":
			server = &got[i]
		case "db.sql.query":
			query = &got[i]
		}
	}
	if server == nil || query == nil {
		t.Fatalf("spans=%v, want server and query", names(got))
	}
	if query.Parent.SpanID() != server.SpanContext.SpanID() || query.SpanContext.TraceID() != server.SpanContext.TraceID() {
		t.Fatal("query span is not a child of the request span in the same trace")
	}
	if rows := attrInt(query.Attributes, "db.rows_affected"); rows != 1 {
		t.Fatalf("db.rows_affected=%d", rows)
	}
}

// A trace id reaches the request context only when its trace is sampled, so
// a logged id always names an exported trace.
func TestOTLPTraceIDOnlyForSampledTraces(t *testing.T) {
	for _, tc := range []struct {
		rate    string
		sampled bool
	}{{"1", true}, {"0", false}} {
		t.Run("rate "+tc.rate, func(t *testing.T) {
			spans := tracetest.NewInMemoryExporter()
			monitor := otlpTestMonitor(t, tc.rate, spans)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(TraceID(r.Context())))
			})
			body := serve(monitor.Handler(mux), httptest.NewRequest(http.MethodGet, "/v1/tasks/7", nil)).Body.String()
			got := flushedSpans(t, monitor, spans)
			if !tc.sampled {
				if body != "" || len(got) != 0 {
					t.Fatalf("unsampled: trace id=%q spans=%d", body, len(got))
				}
				return
			}
			if len(got) != 1 || body != got[0].SpanContext.TraceID().String() {
				t.Fatalf("trace id=%q, exported spans=%v", body, names(got))
			}
		})
	}
}

// A failing exporter never fails a request, and shutdown returns within its
// budget.
func TestOTLPExporterFailureDoesNotFailRequests(t *testing.T) {
	monitor := otlpTestMonitor(t, "1", failingExporter{})
	h := monitor.Handler(otlpTestMux())
	for i := 0; i < 20; i++ {
		if w := serve(h, httptest.NewRequest(http.MethodGet, "/v1/tasks/7", nil)); w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
			t.Fatalf("request %d status=%d body=%q", i, w.Code, w.Body.String())
		}
	}
	start := time.Now()
	monitor.Close()
	if elapsed := time.Since(start); elapsed > otlpFlushTimeout+2*time.Second {
		t.Fatalf("close took %v", elapsed)
	}
}

// A stuck exporter cannot hold shutdown past the fixed budget.
func TestOTLPShutdownHonoursBudgetWhenExporterHangs(t *testing.T) {
	monitor := otlpTestMonitor(t, "1", hangingExporter{})
	serve(monitor.Handler(otlpTestMux()), httptest.NewRequest(http.MethodGet, "/v1/tasks/7", nil))
	start := time.Now()
	monitor.Close()
	if elapsed := time.Since(start); elapsed > otlpFlushTimeout+2*time.Second {
		t.Fatalf("close took %v with a hung exporter", elapsed)
	}
}

type failingExporter struct{}

func (failingExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return errors.New("export refused")
}
func (failingExporter) Shutdown(context.Context) error { return nil }

type hangingExporter struct{}

func (hangingExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	<-ctx.Done()
	return ctx.Err()
}
func (hangingExporter) Shutdown(context.Context) error { return nil }

func names(spans tracetest.SpanStubs) []string {
	out := make([]string, 0, len(spans))
	for _, span := range spans {
		out = append(out, span.Name)
	}
	return out
}

func spanStrings(span tracetest.SpanStub) []string {
	values := []string{span.Name, span.Status.Description}
	for _, kv := range span.Attributes {
		values = append(values, string(kv.Key), kv.Value.Emit())
	}
	return values
}

func attrInt(attrs []attribute.KeyValue, key string) int64 {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsInt64()
		}
	}
	return -1
}

func attrString(attrs []attribute.KeyValue, key string) string {
	for _, kv := range attrs {
		if string(kv.Key) == key {
			return kv.Value.AsString()
		}
	}
	return ""
}
