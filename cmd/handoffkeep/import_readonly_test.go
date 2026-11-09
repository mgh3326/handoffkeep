package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/telemetry"
)

func TestServerHandlerDeniesImportOnlyClientOnMCP(t *testing.T) {
	t.Setenv("HANDOFFKEEP_SENTRY_DSN", "")
	monitor, err := telemetry.FromEnv("test")
	if err != nil {
		t.Fatal(err)
	}
	defer monitor.Close()
	h := serverHandler(api.Server{
		Tokens:           api.Tokens{"youtrack": "import-secret"},
		ImportReadGrants: map[string]bool{"youtrack": true},
	}, monitor, nil)
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"tools/call"}`))
	r.Header.Set("Authorization", "Bearer import-secret")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"error":"import_read_only"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
