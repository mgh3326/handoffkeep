// Package telemetry sends bounded, content-free HTTP and database telemetry.
package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/getsentry/sentry-go"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const flushTimeout = 2 * time.Second

// pollingTraceRate is the head sampling rate for the high-volume fleet polling
// and heartbeat routes, which are most of the span volume. Their error events
// are still sent at SampleRate 1. A default rate below this one lowers it too.
const pollingTraceRate = 0.005

// pollingRoutes are transaction names ("METHOD /route", as reported) of the
// routes that dominate span volume. Writes that run on a fixed heartbeat are
// included with the reads.
var pollingRoutes = map[string]bool{
	"GET /v1/tasks/{id}":                  true,
	"GET /v1/quota/accounts/{key}/latest": true,
	"GET /v1/relay/events":                true,
	"POST /v1/quota/accounts/{key}/lease": true,
	"POST /v1/changes/batch":              true,
	"GET /v1/jobs/{job}":                  true,
	"GET /v1/changes":                     true,
	"GET /v1/chat/messages":               true,
	"GET /v1/documents":                   true,
	"PUT /v1/quota/presence":              true,
}

// tracesSampler keys the head decision on the transaction name. Transactions
// are sampled when they start, so a 5xx or a slow request cannot be kept after
// the fact: the Go SDK has no tail sampling. Child spans inherit their
// transaction's decision before the sampler is consulted.
func tracesSampler(defaultRate float64) sentry.TracesSampler {
	return func(ctx sentry.SamplingContext) float64 {
		// A HEAD request is served by the GET route, so it shares the rate.
		name := ctx.Span.Name
		if rest, ok := strings.CutPrefix(name, "HEAD "); ok {
			name = "GET " + rest
		}
		rate := defaultRate
		if pollingRoutes[name] {
			rate = math.Min(pollingTraceRate, defaultRate)
		}
		if sampleObserver != nil {
			sampleObserver(ctx.Span.Name, rate)
		}
		return rate
	}
}

// sampleObserver is nil outside tests. A test sets it to see the transaction
// name and rate each head decision receives.
var sampleObserver func(name string, rate float64)

// Monitor owns a dedicated Sentry client and an optional OTLP exporter; it never
// changes the process-global hub. A nil client is the disabled Sentry state,
// including when SENTRY_DSN is set alone. A nil otlp is disabled OTLP tracing.
type Monitor struct {
	client    *sentry.Client
	otlp      *otlpExporter
	closeOnce sync.Once
}

func FromEnv(release string) (*Monitor, error) {
	return fromEnv(release, nil)
}

func fromEnv(release string, transport sentry.Transport) (*Monitor, error) {
	return monitorFromEnv(release, transport, nil)
}

// monitorFromEnv builds each backend that is configured. spans, when non-nil,
// replaces the OTLP/HTTP exporter; tests use it so that nothing reaches a
// network.
func monitorFromEnv(release string, transport sentry.Transport, spans sdktrace.SpanExporter) (*Monitor, error) {
	environment := deploymentEnvironment()
	settings, err := otlpSettingsFromEnv(environment, release)
	if err != nil {
		return nil, err
	}
	m := &Monitor{}
	if settings != nil {
		if m.otlp, err = newOTLPExporter(context.Background(), settings, spans); err != nil {
			return nil, err
		}
	}
	dsn := strings.TrimSpace(os.Getenv("HANDOFFKEEP_SENTRY_DSN"))
	if dsn == "" {
		return m, nil
	}
	rate := 0.2
	if raw := os.Getenv("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE"); raw != "" {
		rate, err = strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
			m.shutdownOTLP()
			return nil, errors.New("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE must be between 0 and 1")
		}
	}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn: dsn, Environment: environment, Release: release,
		EnableTracing: true, TracesSampleRate: rate, TracesSampler: tracesSampler(rate), SampleRate: 1,
		MaxSpans: 1000, SendDefaultPII: false, Transport: transport,
		BeforeSend: scrubEvent, BeforeSendTransaction: scrubEvent,
	})
	if err != nil {
		m.shutdownOTLP()
		// SDK configuration errors can contain the DSN.
		return nil, errors.New("invalid HANDOFFKEEP_SENTRY_DSN")
	}
	m.client = client
	return m, nil
}

// deploymentEnvironment is shared by Sentry and OTLP: HANDOFFKEEP_SENTRY_ENV
// keeps its name, and "production" is the default.
func deploymentEnvironment() string {
	environment := strings.TrimSpace(os.Getenv("HANDOFFKEEP_SENTRY_ENV"))
	if environment == "" {
		environment = "production"
	}
	return environment
}

func (m *Monitor) shutdownOTLP() {
	if m.otlp != nil {
		m.otlp.shutdown()
	}
}

// Close flushes after HTTP draining and worker shutdown, with a fixed budget.
func (m *Monitor) Close() {
	m.closeOnce.Do(func() {
		if m.client != nil {
			m.client.Flush(flushTimeout)
			m.client.Close()
		}
		m.shutdownOTLP()
	})
}

// Handler wraps the complete server once. Every request except /healthz gets
// a request id (see withRequestID) whether or not Sentry is enabled. With
// Sentry or OTLP enabled, dispatchers must set Request.Pattern to a static
// template (ServeMux does this automatically). No URL is attached.
func (m *Monitor) Handler(next http.Handler) http.Handler {
	routes := next
	if m.client != nil {
		next = m.trace(next)
	}
	if m.otlp != nil {
		next = m.otlp.trace(routes, next)
	}
	return withRequestID(next)
}

func (m *Monitor) trace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		hub := sentry.NewHub(m.client, sentry.NewScope())
		hub.Scope().SetTag("request_id", RequestID(r.Context()))
		tx := sentry.StartTransaction(sentry.SetHubOnContext(r.Context(), hub), transactionName(r.Method, routePattern(next, r)),
			sentry.WithOpName("http.server"), sentry.WithTransactionSource(sentry.SourceRoute))
		r = r.WithContext(tx.Context())
		status := http.StatusOK
		wroteHeader, sniffed := false, false
		errorCode := ""
		writer := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					if !wroteHeader && (code >= http.StatusOK || code == http.StatusSwitchingProtocols) {
						status, wroteHeader = code, true
					}
					next(code)
				}
			},
			Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
				return func(b []byte) (int, error) {
					wroteHeader = true
					if !sniffed {
						sniffed = true
						if status >= http.StatusInternalServerError {
							errorCode = apiErrorCode(w.Header().Get("Content-Type"), b)
						}
					}
					return next(b)
				}
			},
			Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
				return func() { wroteHeader = true; next() }
			},
			ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
				return func(src io.Reader) (int64, error) { wroteHeader = true; return next(src) }
			},
		})
		defer func() {
			panicked := recover()
			// http.ErrAbortHandler is net/http's deliberate abort (e.g. a
			// streaming client went away), not a server error.
			aborted := panicked == http.ErrAbortHandler
			if panicked != nil && !aborted {
				status = http.StatusInternalServerError
			}
			method := safeMethod(r.Method)
			name := transactionName(r.Method, r.Pattern)
			tx.Name = name
			tx.Status = sentry.HTTPtoSpanStatus(status)
			tx.SetData("http.request.method", method)
			tx.SetData("http.response.status_code", status)
			// A client that disconnected (context.Canceled) cannot receive the
			// 5xx its cancelled queries produce; that is not a server error.
			// Panics are still reported.
			clientGone := errors.Is(r.Context().Err(), context.Canceled)
			if !aborted && (panicked != nil || (status >= http.StatusInternalServerError && !clientGone)) {
				event := sentry.NewEvent()
				event.Level, event.Transaction = sentry.LevelError, name
				event.Extra = map[string]any{"http.request.method": method, "http.response.status_code": status}
				if errorCode != "" {
					event.Tags = map[string]string{"error_code": errorCode}
				}
				exception := sentry.Exception{Type: "HTTPError", Value: "HTTP " + strconv.Itoa(status)}
				if panicked != nil {
					exception.Type, exception.Value = "panic", "HTTP handler panicked"
					exception.Stacktrace = sentry.NewStacktrace()
				}
				event.Exception = []sentry.Exception{exception}
				hub.CaptureEvent(event)
			}
			tx.Finish()
			if panicked != nil {
				panic(panicked) // retain net/http's panic/connection semantics
			}
		}()
		next.ServeHTTP(writer, r)
	})
}

// patternResolver is implemented by *http.ServeMux. Its Handler method reports
// the matched pattern without serving the request.
type patternResolver interface {
	Handler(*http.Request) (http.Handler, string)
}

// routePattern resolves the route pattern before the transaction starts, so the
// sampler can key on it. Request.Pattern is set only once a mux serves, which
// is too late for a head decision. The outermost mux reports "/" for a nested
// mux, so the innermost real pattern wins.
func routePattern(next http.Handler, r *http.Request) string {
	pattern := ""
	for h := next; ; {
		resolver, ok := h.(patternResolver)
		if !ok {
			return pattern
		}
		var found string
		h, found = resolver.Handler(r)
		if found != "" && found != "/" {
			pattern = found
		}
	}
}

// transactionName is the reported name "METHOD /route". A "GET /x" pattern
// also serves HEAD, so the real method is used. Unmatched requests are
// "<unmatched>".
func transactionName(method, pattern string) string {
	name := routeTemplate(pattern)
	if name == "" {
		name = "<unmatched>"
	}
	return safeMethod(method) + " " + name
}

// routeTemplate drops the method from a mux pattern: "GET /v1/tasks/{id}"
// becomes "/v1/tasks/{id}". It is "" for no match or the catch-all "/".
func routeTemplate(pattern string) string {
	if pattern == "" || pattern == "/" {
		return ""
	}
	if verb, rest, ok := strings.Cut(pattern, " "); ok && verb != "" && !strings.ContainsAny(verb, "/{") {
		return rest
	}
	return pattern
}

// apiErrorCode reads the code of a {"error":"<snake_code>"} JSON error body
// from a 5xx response's first write. Only a closed-vocabulary code survives
// (see validErrorCode); any other body yields "".
func apiErrorCode(contentType string, body []byte) string {
	if !strings.HasPrefix(contentType, "application/json") || len(body) > 4096 {
		return ""
	}
	var v struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil || !validErrorCode(v.Error) {
		return ""
	}
	return v.Error
}

func safeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}
