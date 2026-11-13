package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The chain tests in internal/telemetry cover the mux patterns through the
// full server. This covers v1Handler.Handler, which telemetry reads before the
// request is served.
func TestV1HandlerReportsMuxRoute(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	h := v1Handler{mux: mux}
	if _, pattern := h.Handler(httptest.NewRequest(http.MethodGet, "/healthz", nil)); pattern != "GET /healthz" {
		t.Fatalf("healthz pattern=%q", pattern)
	}
	if _, pattern := h.Handler(httptest.NewRequest(http.MethodPost, "/v1/unregistered", nil)); pattern != "" {
		t.Fatalf("unregistered pattern=%q, want no match", pattern)
	}
}
