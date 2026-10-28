package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQuotaRequestGuards(t *testing.T) {
	h := Server{Tokens: Tokens{"client": "test-token"}}.Handler()
	for _, tc := range []struct {
		method, path, body, token string
		status                    int
	}{
		{"GET", "/v1/quota/accounts/codex:0123456789abcdef/latest", "", "", 401},
		{"GET", "/v1/quota/accounts/invalid/latest", "", "test-token", 400},
		{"POST", "/v1/quota/accounts/codex:0123456789abcdef/lease", `{"holder":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ttl_s":121}`, "test-token", 400},
		{"POST", "/v1/quota/accounts/codex:0123456789abcdef/lease", `{"holder":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {}`, "test-token", 400},
		{"POST", "/v1/quota/accounts/codex:0123456789abcdef/lease", strings.Repeat(" ", 33<<10), "test-token", 400},
		{"DELETE", "/v1/quota/accounts/codex:0123456789abcdef/lease", `{"holder":"wrong"}`, "test-token", 400},
		{"PUT", "/v1/quota/accounts/codex:0123456789abcdef/observation", `{"holder":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","observation":{"kind":"auto"}}`, "test-token", 400},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
		}
	}
}
