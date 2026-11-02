// Package telemetry sends bounded, content-free HTTP and database telemetry.
package telemetry

import (
	"crypto/rand"
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
)

const flushTimeout = 2 * time.Second

// Monitor owns a dedicated client; it never changes the process-global hub.
// A nil client is the disabled state, including when SENTRY_DSN is set alone.
type Monitor struct {
	client    *sentry.Client
	closeOnce sync.Once
}

func FromEnv(release string) (*Monitor, error) {
	return fromEnv(release, nil)
}

func fromEnv(release string, transport sentry.Transport) (*Monitor, error) {
	dsn := strings.TrimSpace(os.Getenv("HANDOFFKEEP_SENTRY_DSN"))
	if dsn == "" {
		return &Monitor{}, nil
	}
	rate := 0.2
	if raw := os.Getenv("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE"); raw != "" {
		var err error
		rate, err = strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
			return nil, errors.New("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE must be between 0 and 1")
		}
	}
	environment := strings.TrimSpace(os.Getenv("HANDOFFKEEP_SENTRY_ENV"))
	if environment == "" {
		environment = "production"
	}
	client, err := sentry.NewClient(sentry.ClientOptions{
		Dsn: dsn, Environment: environment, Release: release,
		EnableTracing: true, TracesSampleRate: rate, SampleRate: 1,
		MaxSpans: 1000, SendDefaultPII: false, Transport: transport,
		BeforeSend: scrubEvent, BeforeSendTransaction: scrubEvent,
	})
	if err != nil {
		// SDK configuration errors can contain the DSN.
		return nil, errors.New("invalid HANDOFFKEEP_SENTRY_DSN")
	}
	return &Monitor{client: client}, nil
}

// Close flushes after HTTP draining and worker shutdown, with a fixed budget.
func (m *Monitor) Close() {
	m.closeOnce.Do(func() {
		if m.client != nil {
			m.client.Flush(flushTimeout)
			m.client.Close()
		}
	})
}

// Handler wraps the complete server once. Dispatchers must set Request.Pattern
// to a static template (ServeMux does this automatically). No URL is attached.
func (m *Monitor) Handler(next http.Handler) http.Handler {
	if m.client == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		hub := sentry.NewHub(m.client, sentry.NewScope())
		requestID := rand.Text()
		hub.Scope().SetTag("request_id", requestID)
		w.Header().Set("X-Request-ID", requestID)
		tx := sentry.StartTransaction(sentry.SetHubOnContext(r.Context(), hub), "<unmatched>",
			sentry.WithOpName("http.server"), sentry.WithTransactionSource(sentry.SourceRoute))
		r = r.WithContext(tx.Context())
		status := http.StatusOK
		wroteHeader := false
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
				return func(b []byte) (int, error) { wroteHeader = true; return next(b) }
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
			name := r.Pattern
			if name == "" || name == "/" {
				name = "<unmatched>"
			}
			method := safeMethod(r.Method)
			// A "GET /x" pattern also serves HEAD; name by the real method.
			if verb, rest, ok := strings.Cut(name, " "); ok && verb != "" && !strings.ContainsAny(verb, "/{") {
				name = rest
			}
			name = method + " " + name
			tx.Name = name
			tx.Status = sentry.HTTPtoSpanStatus(status)
			tx.SetData("http.request.method", method)
			tx.SetData("http.response.status_code", status)
			if !aborted && (panicked != nil || status >= http.StatusInternalServerError) {
				event := sentry.NewEvent()
				event.Level, event.Transaction = sentry.LevelError, name
				event.Extra = map[string]any{"http.request.method": method, "http.response.status_code": status}
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

func safeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}
