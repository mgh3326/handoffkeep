package telemetry

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// OTLP tracing exports one route group's server spans and their database
// spans to an OTLP/HTTP endpoint. It is independent of Sentry: each keeps its
// own sampling decision, and disabling one never changes the other.

const (
	otlpFlushTimeout  = 2 * time.Second
	otlpExportTimeout = 10 * time.Second
	otlpDefaultRate   = 0.2
	otlpQueueSize     = 2048
	otlpBatchSize     = 512
	otlpBatchDelay    = 5 * time.Second
	otlpTracerName    = "handoffkeep"
)

// otlpRouteGroups names the route sets that OTLP tracing can cover. Matching
// is on the static route template ("/v1/tasks/{id}"), never on the URL.
var otlpRouteGroups = map[string]func(template string) bool{
	"tasks": func(template string) bool {
		return template == "/v1/tasks" || strings.HasPrefix(template, "/v1/tasks/")
	},
}

// otlpSettings is the parsed, validated OTLP configuration. Nil means disabled.
type otlpSettings struct {
	tracesURL     string
	authorization string
	sampleRate    float64
	routes        func(template string) bool
	environment   string
	release       string
}

// otlpExporter owns one provider. Spans are created only for routes in scope
// and when the head sampler keeps the trace.
type otlpExporter struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
	routes   func(template string) bool
}

// otlpSettingsFromEnv returns nil when HANDOFFKEEP_OTLP_ENDPOINT is unset, so
// no exporter is built and nothing is sent. Errors never echo a value: the
// token and instance id are secrets.
func otlpSettingsFromEnv(environment, release string) (*otlpSettings, error) {
	endpoint := strings.TrimSpace(os.Getenv("HANDOFFKEEP_OTLP_ENDPOINT"))
	if endpoint == "" {
		return nil, nil
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("HANDOFFKEEP_OTLP_ENDPOINT must be an https base URL without credentials, query or fragment")
	}
	instance := strings.TrimSpace(os.Getenv("HANDOFFKEEP_OTLP_INSTANCE_ID"))
	token := strings.TrimSpace(os.Getenv("HANDOFFKEEP_OTLP_TOKEN"))
	if instance == "" || token == "" || strings.Contains(instance, ":") {
		return nil, errors.New("HANDOFFKEEP_OTLP_INSTANCE_ID and HANDOFFKEEP_OTLP_TOKEN are required with HANDOFFKEEP_OTLP_ENDPOINT")
	}
	rate := otlpDefaultRate
	if raw := os.Getenv("HANDOFFKEEP_OTLP_SAMPLE_RATE"); raw != "" {
		rate, err = strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
			return nil, errors.New("HANDOFFKEEP_OTLP_SAMPLE_RATE must be between 0 and 1")
		}
	}
	group := strings.TrimSpace(os.Getenv("HANDOFFKEEP_OTLP_ROUTES"))
	if group == "" {
		group = "tasks"
	}
	routes, ok := otlpRouteGroups[group]
	if !ok {
		return nil, errors.New("HANDOFFKEEP_OTLP_ROUTES must name a known route group")
	}
	return &otlpSettings{
		tracesURL:     strings.TrimRight(parsed.String(), "/") + "/v1/traces",
		authorization: "Basic " + base64.StdEncoding.EncodeToString([]byte(instance+":"+token)),
		sampleRate:    rate,
		routes:        routes,
		environment:   environment,
		release:       release,
	}, nil
}

// newOTLPExporter builds the provider. A nil exporter means the real OTLP/HTTP
// exporter; tests pass an in-memory one so that no network call is made.
func newOTLPExporter(ctx context.Context, s *otlpSettings, exporter sdktrace.SpanExporter) (*otlpExporter, error) {
	if exporter == nil {
		var err error
		exporter, err = otlptracehttp.New(ctx,
			otlptracehttp.WithEndpointURL(s.tracesURL),
			otlptracehttp.WithHeaders(map[string]string{"Authorization": s.authorization}),
			otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
			otlptracehttp.WithTimeout(otlpExportTimeout),
		)
		if err != nil {
			return nil, errors.New("HANDOFFKEEP_OTLP_ENDPOINT could not be used")
		}
	}
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			attribute.String("service.name", "handoffkeep"),
			attribute.String("service.version", s.release),
			attribute.String("deployment.environment", s.environment),
		),
	)
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(s.sampleRate))),
		sdktrace.WithBatcher(exporter,
			sdktrace.WithMaxQueueSize(otlpQueueSize),
			sdktrace.WithMaxExportBatchSize(otlpBatchSize),
			sdktrace.WithBatchTimeout(otlpBatchDelay),
		),
	)
	return &otlpExporter{provider: provider, tracer: provider.Tracer(otlpTracerName), routes: s.routes}, nil
}

// trace wraps the request in a server span when its route template is in
// scope. routes is the mux, which resolves the template before the span
// starts. The span is named by method and template, with no URL, query,
// headers, or body. Incoming trace headers are not extracted, so the head
// sampler decides every trace here.
func (e *otlpExporter) trace(routes, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := routePattern(routes, r)
		template := routeTemplate(pattern)
		if pattern == "" || !e.routes(template) {
			next.ServeHTTP(w, r)
			return
		}
		method := safeMethod(r.Method)
		ctx, span := e.tracer.Start(r.Context(), method+" "+template,
			trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String("http.request.method", method), attribute.String("http.route", template)),
		)
		status := http.StatusOK
		wroteHeader := false
		writer := httpsnoop.Wrap(w, httpsnoop.Hooks{
			WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
				return func(code int) {
					// 1xx informational responses (103 Early Hints) do not finish
					// the response, except 101; the final code follows them.
					// 1xx informational responses (103 Early Hints) do not finish
					// the response, except 101; the final code follows them.
					if !wroteHeader && (code >= http.StatusOK || code == http.StatusSwitchingProtocols) {
						status, wroteHeader = code, true
					}
					next(code)
				}
			},
		})
		defer func() {
			panicked := recover()
			// http.ErrAbortHandler is a deliberate abort, not a server error.
			if panicked != nil && panicked != http.ErrAbortHandler {
				status = http.StatusInternalServerError
			}
			span.SetAttributes(attribute.Int("http.response.status_code", status))
			if status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, "")
			}
			span.End()
			if panicked != nil {
				panic(panicked)
			}
		}()
		next.ServeHTTP(writer, r.WithContext(ctx))
	})
}

// shutdown flushes queued spans within the budget. Failures are logged without
// the endpoint or any credential and never change a response.
func (e *otlpExporter) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), otlpFlushTimeout)
	defer cancel()
	if err := e.provider.Shutdown(ctx); err != nil {
		log.Printf("telemetry.otlp_flush_failed err=%q", err.Error())
	}
}

// TraceID returns the id of the trace that ctx belongs to when that trace is
// sampled and so exported; otherwise "". Logs and error bodies carry it only
// then, so an id always names a trace that can be opened.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsSampled() {
		return ""
	}
	return sc.TraceID().String()
}
