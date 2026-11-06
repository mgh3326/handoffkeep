package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
)

func TestOperatorWriteGuards(t *testing.T) {
	h := &Handler{csrfKey: []byte("guard-test")}
	operator := cfaccess.Identity{Email: "operator@example.com"}
	for _, path := range []string{"/ui/api/decision-requests/resolve", "/ui/api/decision-requests/deny", "/ui/api/tasks/1/priority"} {
		t.Run(path, func(t *testing.T) {
			for _, tc := range []struct {
				name         string
				identity     cfaccess.Identity
				origin, body string
				csrf         bool
				want         string
			}{
				{"service", cfaccess.Identity{ServiceName: "svc"}, "http://example.com", `{}`, false, "operator_only"},
				{"origin", operator, "http://evil.example", `{}`, true, "origin_rejected"},
				{"csrf", operator, "http://example.com", `{}`, false, "csrf_rejected"},
				{"unknown", operator, "http://example.com", `{"unknown":true}`, true, "invalid_request"},
				{"trailing", operator, "http://example.com", `{} {}`, true, "invalid_request"},
				{"missing-key", operator, "http://example.com", `{}`, true, "event_key_required"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					r := httptest.NewRequest(http.MethodPost, "http://example.com"+path, strings.NewReader(tc.body))
					r.Header.Set("Origin", tc.origin)
					if tc.csrf {
						token, err := h.newCSRFToken(tc.identity.Email)
						if err != nil {
							t.Fatal(err)
						}
						r.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
						r.Header.Set("X-CSRF-Token", token)
					}
					w := httptest.NewRecorder()
					h.serveAPI(w, r, tc.identity)
					if !strings.Contains(w.Body.String(), `"error":"`+tc.want+`"`) {
						t.Fatalf("status %d: %s", w.Code, w.Body.String())
					}
				})
			}
		})
	}
}
