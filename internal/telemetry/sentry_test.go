package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type testTransport struct {
	mu      sync.Mutex
	events  []*sentry.Event
	flushed time.Duration
	closed  bool
}

func (*testTransport) Configure(sentry.ClientOptions) {}
func (t *testTransport) SendEvent(event *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, event)
}
func (t *testTransport) Flush(timeout time.Duration) bool { t.flushed = timeout; return true }
func (t *testTransport) Close()                           { t.closed = true }

func testMonitor(t *testing.T) (*Monitor, *testTransport) {
	t.Helper()
	// A syntactically valid, invented DSN. The test transport never uses a network.
	dsn := (&url.URL{Scheme: "https", Host: strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")), User: url.User("test"), Path: "/1"}).String()
	t.Setenv("HANDOFFKEEP_SENTRY_DSN", dsn)
	t.Setenv("HANDOFFKEEP_SENTRY_ENV", "test")
	t.Setenv("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE", "1")
	transport := &testTransport{}
	monitor, err := fromEnv("test-revision", transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(monitor.Close)
	return monitor, transport
}

func TestFromEnvDisabled(t *testing.T) {
	t.Setenv("HANDOFFKEEP_SENTRY_DSN", "")
	t.Setenv("SENTRY_DSN", "must not initialize the SDK")
	t.Setenv("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE", "invalid but disabled")
	monitor, err := FromEnv("test-revision")
	if err != nil || monitor.client != nil {
		t.Fatalf("disabled monitor=%+v err=%v", monitor, err)
	}
	handler := http.NewServeMux()
	if monitor.Handler(handler) != http.Handler(handler) {
		t.Fatal("disabled middleware must return the original handler")
	}
	monitor.Close()
}

func TestFromEnvOptionsAndFlush(t *testing.T) {
	monitor, transport := testMonitor(t)
	options := monitor.client.Options()
	if options.Environment != "test" || options.Release != "test-revision" || options.SendDefaultPII {
		t.Fatalf("unexpected options: env=%q release=%q pii=%v", options.Environment, options.Release, options.SendDefaultPII)
	}
	t.Setenv("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE", "")
	defaults, err := fromEnv("test-revision", &testTransport{})
	if err != nil {
		t.Fatal(err)
	}
	defer defaults.Close()
	if defaults.client.Options().TracesSampleRate != 0.2 {
		t.Fatal("wrong default sample rate")
	}
	monitor.Close()
	if transport.flushed != flushTimeout || !transport.closed {
		t.Fatal("shutdown did not flush and close within the budget")
	}
}

func TestFromEnvInvalidSampleRates(t *testing.T) {
	t.Setenv("HANDOFFKEEP_SENTRY_DSN", "configured")
	for _, value := range []string{"-1", "1.1", "NaN", "+Inf", "invalid"} {
		t.Setenv("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE", value)
		if _, err := FromEnv("test-revision"); err == nil {
			t.Fatalf("accepted rate %q", value)
		}
	}
}

func TestRequestTransactionAndQuerySpans(t *testing.T) {
	monitor, transport := testMonitor(t)
	conn := protocolConn(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 4; i++ {
			if _, err := conn.Exec(r.Context(), "SELECT $1::text, 'literal private content', 123", "bound private content"); err != nil {
				t.Fatal(err)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	monitor.Handler(mux).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/tasks/123?token=secret", nil))
	if len(transport.events) != 1 {
		t.Fatalf("got %d events", len(transport.events))
	}
	event := transport.events[0]
	if event.Type != "transaction" || event.Transaction != "GET /v1/tasks/{id}" {
		t.Fatalf("unexpected transaction: %q %q", event.Type, event.Transaction)
	}
	if event.TransactionInfo.Source != sentry.SourceRoute {
		t.Fatal("missing route source")
	}
	if event.Extra["http.response.status_code"] != http.StatusNoContent || !event.Timestamp.After(event.StartTime) {
		t.Fatal("missing status or duration")
	}
	if event.Tags["request_id"] == "" || event.Tags["request_id"] != response.Header().Get("X-Request-ID") {
		t.Fatal("missing request id correlation")
	}
	if len(event.Spans) != 4 {
		t.Fatalf("got %d DB spans", len(event.Spans))
	}
	for _, span := range event.Spans {
		if span.Op != "db.sql.query" || span.Description != "SELECT ?::text, ?, ?" {
			t.Fatalf("bad query span: %+v", span)
		}
		if span.Data["db.system"] != "postgresql" || span.Data["db.rows_affected"] != int64(1) {
			t.Fatalf("bad DB metadata: %+v", span.Data)
		}
		if span.ParentSpanID.String() != fmt.Sprint(event.Contexts["trace"]["span_id"]) || !span.EndTime.After(span.StartTime) {
			t.Fatal("span missing parent or duration")
		}
	}
	assertNoContent(t, transport.events, "bound private content", "literal private content", "token=secret", "/tasks/123")
}

func TestHealthAndUnsampledRequests(t *testing.T) {
	monitor, transport := testMonitor(t)
	for _, rate := range []float64{1, 0} {
		options := monitor.client.Options()
		options.TracesSampleRate = rate
		client, err := sentry.NewClient(options)
		if err != nil {
			t.Fatal(err)
		}
		m := &Monitor{client: client}
		path := "/healthz"
		if rate == 0 {
			path = "/v1/tasks/123"
		}
		m.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := QueryTracer{}.TraceQueryStart(r.Context(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
			if sentry.SpanFromContext(ctx) != sentry.SpanFromContext(r.Context()) {
				t.Fatal("unexpected DB span")
			}
			if path == "/healthz" && sentry.SpanFromContext(ctx) != nil {
				t.Fatal("health created a transaction")
			}
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
		m.Close()
	}
	if len(transport.events) != 0 {
		t.Fatalf("got %d events", len(transport.events))
	}
}

func TestErrorsAndPanicsScrubContent(t *testing.T) {
	for _, panicRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "5xx", true: "panic"}[panicRequest], func(t *testing.T) {
			monitor, transport := testMonitor(t)
			secret, body := "Bearer token_private_very_sensitive", "private document body"
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/documents/{key...}", func(w http.ResponseWriter, r *http.Request) {
				hub := sentry.GetHubFromContext(r.Context())
				hub.Scope().SetRequest(r)
				hub.Scope().SetUser(sentry.User{ID: secret})
				hub.Scope().SetExtra("document_body", body)
				hub.Scope().SetTag("authorization", secret)
				hub.Scope().SetContext("document", sentry.Context{"body": body})
				hub.Scope().SetFingerprint([]string{secret})
				hub.AddBreadcrumb(&sentry.Breadcrumb{Message: body}, nil)
				spanCtx := QueryTracer{}.TraceQueryStart(r.Context(), nil, pgx.TraceQueryStartData{SQL: "SELECT $1", Args: []any{body}})
				QueryTracer{}.TraceQueryEnd(spanCtx, nil, pgx.TraceQueryEndData{Err: errors.New(secret)})
				if panicRequest {
					panic(secret + body)
				}
				http.Error(w, secret+body, http.StatusServiceUnavailable)
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/documents/private-key?token=private-query", strings.NewReader(body))
			request.Header.Set("Authorization", secret)
			request.Header.Set("Cookie", "token="+secret)
			request.Header.Set("X-Request-ID", secret)
			response := httptest.NewRecorder()
			func() {
				defer func() {
					got := recover()
					if panicRequest && got != secret+body {
						t.Fatalf("panic semantics changed: %v", got)
					}
					if !panicRequest && got != nil {
						t.Fatalf("unexpected panic: %v", got)
					}
				}()
				monitor.Handler(mux).ServeHTTP(response, request)
			}()
			if len(transport.events) != 2 {
				t.Fatalf("got %d events, want error + transaction", len(transport.events))
			}
			for _, event := range transport.events {
				if event.Transaction != "POST /v1/documents/{key...}" || event.Tags["request_id"] != response.Header().Get("X-Request-ID") {
					t.Fatal("missing route or request id")
				}
			}
			assertNoContent(t, transport.events, secret, body, "private-key", "private-query", "Authorization", "Cookie", "document_body")
		})
	}
}

func TestAbortHandlerIsNotAnError(t *testing.T) {
	monitor, transport := testMonitor(t)
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Fatalf("abort not re-panicked: %v", got)
			}
		}()
		monitor.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/events", nil))
	}()
	if len(transport.events) != 1 || transport.events[0].Type != "transaction" || transport.events[0].Extra["http.response.status_code"] == http.StatusInternalServerError {
		t.Fatalf("abort produced an error event or a 500: %d events", len(transport.events))
	}
}

func TestHeadOnGetPatternNamesByRealMethod(t *testing.T) {
	monitor, transport := testMonitor(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tasks/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	monitor.Handler(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodHead, "/v1/tasks/1", nil))
	if len(transport.events) != 1 || transport.events[0].Transaction != "HEAD /v1/tasks/{id}" {
		t.Fatalf("got %d events, name %q", len(transport.events), transport.events[0].Transaction)
	}
}

func TestHTTPStatusSemantics(t *testing.T) {
	for name, write := range map[string]func(http.ResponseWriter){
		"informational": func(w http.ResponseWriter) { w.WriteHeader(http.StatusEarlyHints); w.WriteHeader(http.StatusOK) },
		"implicit": func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("body"))
			w.WriteHeader(http.StatusServiceUnavailable)
		},
		"flush": func(w http.ResponseWriter) { w.(http.Flusher).Flush(); w.WriteHeader(http.StatusServiceUnavailable) },
	} {
		t.Run(name, func(t *testing.T) {
			monitor, transport := testMonitor(t)
			monitor.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { write(w) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
			if len(transport.events) != 1 || transport.events[0].Extra["http.response.status_code"] != http.StatusOK {
				t.Fatal("incorrect status or spurious 5xx event")
			}
		})
	}
}

func TestSpanLimitAndUnsampledErrors(t *testing.T) {
	monitor, transport := testMonitor(t)
	monitor.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 1100; i++ {
			ctx := QueryTracer{}.TraceQueryStart(r.Context(), nil, pgx.TraceQueryStartData{SQL: "SELECT $1"})
			QueryTracer{}.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got := len(transport.events[0].Spans); got == 0 || got > 999 {
		t.Fatalf("unbounded or missing spans: %d", got)
	}
	t.Setenv("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE", "0")
	unsampledTransport := &testTransport{}
	unsampled, err := fromEnv("test-revision", unsampledTransport)
	if err != nil {
		t.Fatal(err)
	}
	defer unsampled.Close()
	unsampled.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if len(unsampledTransport.events) != 1 || unsampledTransport.events[0].Type == "transaction" {
		t.Fatal("unsampled request lost its error or emitted a transaction")
	}
}

func TestQueryErrorHasNoErrorText(t *testing.T) {
	monitor, transport := testMonitor(t)
	monitor.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := QueryTracer{}.TraceQueryStart(r.Context(), nil, pgx.TraceQueryStartData{SQL: "UPDATE tasks SET title=$1", Args: []any{"private"}})
		QueryTracer{}.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("UPDATE 0"), Err: errors.New("private detail")})
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	span := transport.events[0].Spans[0]
	if span.Status != sentry.SpanStatusInternalError {
		t.Fatal("missing failed query status")
	}
	assertNoContent(t, transport.events, "private")
	ctx := context.Background()
	if (QueryTracer{}).TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{}) != ctx {
		t.Fatal("background query created a span")
	}
}

func assertNoContent(t *testing.T, events []*sentry.Event, forbidden ...string) {
	t.Helper()
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range forbidden {
		if strings.Contains(string(raw), value) {
			t.Fatalf("captured event leaked %q", value)
		}
	}
}
