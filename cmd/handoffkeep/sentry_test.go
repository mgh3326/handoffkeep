package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
	"github.com/mgh3326/handoffkeep/internal/ui"
)

func TestServerSentryRoutes(t *testing.T) {
	envelopes := make(chan []byte, 10)
	capture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		envelopes <- raw
		w.WriteHeader(http.StatusOK)
	}))
	defer capture.Close()
	dsn, err := url.Parse(capture.URL)
	if err != nil {
		t.Fatal(err)
	}
	dsn.User, dsn.Path = url.User("test"), "/1"
	t.Setenv("HANDOFFKEEP_SENTRY_DSN", dsn.String())
	t.Setenv("HANDOFFKEEP_SENTRY_ENV", "test")
	t.Setenv("HANDOFFKEEP_SENTRY_TRACES_SAMPLE_RATE", "1")
	monitor, err := telemetry.FromEnv("test-revision")
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close()
	handler := serverHandler(api.Server{
		UI:            &ui.Handler{}, // no Access header: reject before any store call
		LinearWebhook: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	}, monitor)
	want := map[string]int{}
	for _, request := range []struct{ method, path, name string }{
		{http.MethodGet, "/v1/tasks/123", "GET /v1/tasks/{id}"},
		{http.MethodPost, "/v1/documents/private-doc", "POST /v1/documents/{key...}"},
		{http.MethodPost, "/hooks/linear", "POST /hooks/linear"},
		{http.MethodGet, "/ui/tasks/123", "GET /ui/tasks/{id}"},
		{http.MethodGet, "/mcp", "GET /mcp"},
		{http.MethodGet, "/private-unknown", "GET <unmatched>"},
		{http.MethodGet, "/healthz", ""},
	} {
		r := httptest.NewRequest(request.method, request.path+"?token=private-query", strings.NewReader("private document body"))
		r.Header.Set("Authorization", "Bearer private-token")
		handler.ServeHTTP(httptest.NewRecorder(), r)
		if request.name != "" {
			want[request.name]++
		}
	}
	monitor.Close() // all requests must reach the transport before checking counts
	if len(envelopes) != len(want) {
		t.Fatalf("got %d envelopes, want %d", len(envelopes), len(want))
	}
	for len(envelopes) > 0 {
		raw := <-envelopes
		for _, secret := range []string{"private-token", "private-query", "private document body", "private-doc", "private-unknown", "/tasks/123"} {
			if bytes.Contains(raw, []byte(secret)) {
				t.Fatalf("Sentry envelope leaked %q", secret)
			}
		}
		lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
		if len(lines) != 3 {
			t.Fatalf("unexpected envelope items: %d", len(lines))
		}
		var event sentry.Event
		if err := json.Unmarshal(lines[2], &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "transaction" || want[event.Transaction] != 1 {
			t.Fatalf("unexpected or duplicate transaction %q (%q)", event.Transaction, event.Type)
		}
		want[event.Transaction]--
	}
}
