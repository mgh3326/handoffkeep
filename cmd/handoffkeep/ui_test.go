package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
)

func TestUIFromEnvLeavesRouteAbsentWhenAccessConfigIncomplete(t *testing.T) {
	for _, missing := range []string{
		"HANDOFFKEEP_UI_CF_TEAM_DOMAIN",
		"HANDOFFKEEP_UI_CF_AUD",
		"HANDOFFKEEP_UI_ALLOWED_EMAILS",
		"all",
	} {
		t.Run(missing, func(t *testing.T) {
			t.Setenv("HANDOFFKEEP_UI_CF_TEAM_DOMAIN", "example.cloudflareaccess.com")
			t.Setenv("HANDOFFKEEP_UI_CF_AUD", "example-ui-audience")
			t.Setenv("HANDOFFKEEP_UI_ALLOWED_EMAILS", "admin@example.com")
			if missing == "all" {
				t.Setenv("HANDOFFKEEP_UI_CF_TEAM_DOMAIN", "   ")
				t.Setenv("HANDOFFKEEP_UI_CF_AUD", "\t")
				t.Setenv("HANDOFFKEEP_UI_ALLOWED_EMAILS", "")
			} else {
				t.Setenv(missing, " ")
			}
			handler, err := uiFromEnv(nil)
			if err != nil || handler != nil {
				t.Fatalf("handler=%v err=%v", handler, err)
			}
			server := api.Server{UI: handler}.Handler()
			for _, path := range []string{"/ui", "/ui/timeline", "/ui/events", "/ui/static/htmx.min.js"} {
				response := httptest.NewRecorder()
				server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
				if response.Code != http.StatusNotFound {
					t.Fatalf("%s status=%d want 404", path, response.Code)
				}
			}
		})
	}
}
