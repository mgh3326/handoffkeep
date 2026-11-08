package telemetry_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

// These tests run the production /v1 chain (api.Server.Handler under
// monitor.Handler, mounted the way serverHandler mounts it) with a recording
// transport. Nothing reaches Sentry.

type recordedTransport struct {
	mu     sync.Mutex
	events []*sentry.Event
}

func (*recordedTransport) Configure(sentry.ClientOptions) {}
func (t *recordedTransport) SendEvent(event *sentry.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, event)
}
func (*recordedTransport) Flush(time.Duration) bool { return true }
func (*recordedTransport) Close()                   {}

// decision is one head sampling call: the transaction name the sampler sees
// and the rate it returns.
type decision struct {
	name string
	rate float64
}

type rig struct {
	handler   http.Handler
	transport *recordedTransport
	decisions []decision
}

const testToken = "test-token"

func newRig(t *testing.T) *rig {
	t.Helper()
	// A syntactically valid, invented DSN. The recording transport never uses a network.
	dsn := (&url.URL{Scheme: "https", Host: "route-sampling.invalid", User: url.User("test"), Path: "/1"}).String()
	t.Setenv("HANDOFFKEEP_SENTRY_DSN", dsn)
	t.Setenv("HANDOFFKEEP_SENTRY_ENV", "test")
	t.Setenv("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE", "1")
	r := &rig{transport: &recordedTransport{}}
	monitor, err := telemetry.FromEnvWithTransportForTest("test-revision", r.transport)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(monitor.Close)
	restore := telemetry.ObserveSamplingForTest(func(name string, rate float64) {
		r.decisions = append(r.decisions, decision{name, rate})
	})
	t.Cleanup(restore)

	server := api.Server{Tokens: api.Tokens{"test-client": testToken}}
	// Mounted as serverHandler mounts it: /mcp beside the /v1 surface.
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	mux.Handle("/", server.Handler())
	r.handler = monitor.Handler(mux)
	return r
}

// serve runs one request through the chain and returns what it panicked with
// (nil when it returned normally). The chain re-panics after recording.
func (r *rig) serve(req *http.Request) (panicked any) {
	defer func() { panicked = recover() }()
	r.handler.ServeHTTP(httptest.NewRecorder(), req)
	return nil
}

// sampleOnce resets the observed decisions, serves req, and returns the single
// decision the sampler received for it.
func (r *rig) sampleOnce(t *testing.T, req *http.Request) decision {
	t.Helper()
	r.decisions = nil
	if panicked := r.serve(req); panicked != nil {
		t.Fatalf("%s %s panicked: %v", req.Method, req.URL.Path, panicked)
	}
	if len(r.decisions) != 1 {
		t.Fatalf("%s %s: got %d sampling decisions, want 1: %+v", req.Method, req.URL.Path, len(r.decisions), r.decisions)
	}
	return r.decisions[0]
}

// pollingCases are the ten routes in the polling set (tracesSampler), one
// concrete request each.
var pollingCases = []struct {
	method, target, name string
}{
	{http.MethodGet, "/v1/tasks/123", "GET /v1/tasks/{id}"},
	{http.MethodGet, "/v1/quota/accounts/acct-1/latest", "GET /v1/quota/accounts/{key}/latest"},
	{http.MethodGet, "/v1/relay/events", "GET /v1/relay/events"},
	{http.MethodPost, "/v1/quota/accounts/acct-1/lease", "POST /v1/quota/accounts/{key}/lease"},
	{http.MethodPost, "/v1/changes/batch", "POST /v1/changes/batch"},
	{http.MethodGet, "/v1/jobs/job-1", "GET /v1/jobs/{job}"},
	{http.MethodGet, "/v1/changes", "GET /v1/changes"},
	{http.MethodGet, "/v1/chat/messages", "GET /v1/chat/messages"},
	{http.MethodGet, "/v1/documents", "GET /v1/documents"},
	{http.MethodPut, "/v1/quota/presence", "PUT /v1/quota/presence"},
}

func TestPollingRoutesSampleAtPollingRateThroughRealChain(t *testing.T) {
	r := newRig(t)
	if len(pollingCases) != 10 {
		t.Fatalf("polling cases = %d, want 10", len(pollingCases))
	}
	for _, tc := range pollingCases {
		// Unauthenticated: the route still keys the decision, and auth rejects before any store call.
		got := r.sampleOnce(t, httptest.NewRequest(tc.method, tc.target, nil))
		if want := (decision{tc.name, 0.005}); got != want {
			t.Errorf("%s %s: sampler got %+v, want %+v", tc.method, tc.target, got, want)
		}
	}
}

func TestHeadOnPollingGetRouteUsesPollingRate(t *testing.T) {
	r := newRig(t)
	got := r.sampleOnce(t, httptest.NewRequest(http.MethodHead, "/v1/tasks/123", nil))
	if want := (decision{"HEAD /v1/tasks/{id}", 0.005}); got != want {
		t.Fatalf("HEAD polling route: sampler got %+v, want %+v", got, want)
	}
}

func TestNonPollingAndUnmatchedRoutesUseDefaultRate(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct {
		method, target string
		want           decision
	}{
		{http.MethodGet, "/v1/search", decision{"GET /v1/search", 1}},
		{http.MethodGet, "/mcp", decision{"GET /mcp", 1}},
		{http.MethodGet, "/private-unknown", decision{"GET <unmatched>", 1}},
	} {
		if got := r.sampleOnce(t, httptest.NewRequest(tc.method, tc.target, nil)); got != tc.want {
			t.Errorf("%s %s: sampler got %+v, want %+v", tc.method, tc.target, got, tc.want)
		}
	}
}

func TestUnauthenticatedRequestIsKeyedByRouteAndLeaksNoCredentialOrURL(t *testing.T) {
	r := newRig(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/search?token=private-query", nil)
	req.Header.Set("Authorization", "Bearer private-token")
	r.decisions = nil
	if panicked := r.serve(req); panicked != nil {
		t.Fatalf("unauthenticated request panicked: %v", panicked)
	}
	if want := []decision{{"GET /v1/search", 1}}; !reflect.DeepEqual(r.decisions, want) {
		t.Fatalf("sampler got %+v, want %+v", r.decisions, want)
	}
	if len(r.transport.events) != 1 {
		t.Fatalf("got %d events, want the one sampled transaction", len(r.transport.events))
	}
	raw, err := json.Marshal(r.transport.events[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-token", "private-query", "token="} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("event leaked %q: %s", secret, raw)
		}
	}
}

func TestPanicOnPollingRouteIsStillReportedAtFullRate(t *testing.T) {
	r := newRig(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/123", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	r.decisions = nil
	// With no store behind an authenticated handler, the route panics; the
	// chain records it as an error event and re-panics.
	if r.serve(req) == nil {
		t.Fatal("authenticated request with no store did not panic")
	}
	// The transaction itself is head-sampled at the polling rate...
	if want := []decision{{"GET /v1/tasks/{id}", 0.005}}; !reflect.DeepEqual(r.decisions, want) {
		t.Fatalf("sampler got %+v, want %+v", r.decisions, want)
	}
	// ...but the error event is not subject to it: SampleRate stays 1.
	errors := 0
	for _, event := range r.transport.events {
		if event.Level == sentry.LevelError {
			errors++
			if event.Transaction != "GET /v1/tasks/{id}" {
				t.Fatalf("error event transaction %q", event.Transaction)
			}
		}
	}
	if errors != 1 {
		t.Fatalf("got %d error events, want 1", errors)
	}
}
