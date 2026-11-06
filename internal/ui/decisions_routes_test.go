package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
)

func TestDecisionShellPathAndPattern(t *testing.T) {
	h := newAssetTestHandler(t, "")
	h.csrfKey = []byte("fixture")
	for _, tc := range []struct {
		raw    string
		status int
	}{{"42", http.StatusOK}, {"abc", http.StatusBadRequest}, {"0", http.StatusBadRequest}, {"1234567890123456", http.StatusBadRequest}} {
		r := httptest.NewRequest(http.MethodGet, "/ui/decisions/"+tc.raw, nil)
		r = r.WithContext(context.WithValue(r.Context(), shellEmailKey{}, "operator@example.com"))
		w := httptest.NewRecorder()
		h.serveSubroute(w, r, "operator@example.com")
		if w.Code != tc.status {
			t.Fatalf("%s: %d", tc.raw, w.Code)
		}
		if tc.status == http.StatusOK && (!strings.Contains(w.Body.String(), "shell-root") || !strings.Contains(w.Body.String(), "hk-csrf")) {
			t.Fatal("missing shell or csrf")
		}
	}
	if got := routePattern("/ui/decisions/42"); got != "/ui/decisions/{id}" {
		t.Fatal(got)
	}
}

func TestDecisionsAPIServiceForbidden(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.decisionsAPI(w, httptest.NewRequest(http.MethodGet, "/ui/api/decisions", nil), cfaccess.Identity{ServiceName: "service"})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"forbidden"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDecisionLegacyRedirect(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.decisionResult(w, httptest.NewRequest(http.MethodPost, "/ui/decisions/answer", nil), "operator@example.com", "saved", http.StatusOK)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/ui/decisions/legacy?result=saved" {
		t.Fatalf("status=%d location=%q", w.Code, w.Header().Get("Location"))
	}
}
