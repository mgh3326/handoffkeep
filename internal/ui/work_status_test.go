package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
)

func TestWorkStatusAPIRefusesServicePrincipals(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.serveAPI(rec, httptest.NewRequest(http.MethodGet, "/ui/api/work-status", nil), cfaccess.Identity{ServiceName: "svc"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}
