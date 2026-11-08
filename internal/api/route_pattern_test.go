package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The chain tests in internal/telemetry cover the mux patterns through the
// full server. This covers the wrk override, which needs a store to reach.
func TestV1HandlerReportsWrkRouteOnlyWhenRegistered(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	wrk := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	registered := v1Handler{mux: mux, legacy: http.NotFoundHandler(), wrk: wrk}
	for _, path := range []string{"/v1/wrk", "/v1/wrk/", "/v1/wrk/a/b"} {
		handler, pattern := registered.Handler(httptest.NewRequest(http.MethodPost, path, nil))
		if pattern != wrkPattern || handler == nil {
			t.Fatalf("%s: pattern=%q handler=%v, want %q", path, pattern, handler, wrkPattern)
		}
	}
	if _, pattern := registered.Handler(httptest.NewRequest(http.MethodGet, "/healthz", nil)); pattern != "GET /healthz" {
		t.Fatalf("healthz pattern=%q", pattern)
	}
	unregistered := v1Handler{mux: mux, legacy: http.NotFoundHandler()}
	if _, pattern := unregistered.Handler(httptest.NewRequest(http.MethodPost, "/v1/wrk/a", nil)); pattern != "" {
		t.Fatalf("unregistered wrk pattern=%q, want no match", pattern)
	}
}
