package api

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuotaRequestGuards(t *testing.T) {
	h := Server{Tokens: Tokens{"client": "test-token"}, QuotaAccounts: QuotaAccounts{"client": {"codex:0123456789abcdef": true}}}.Handler()
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

// The scope check runs after quotaAuth's key validation and before the store,
// so these guards need no database.
func TestQuotaWriteScope(t *testing.T) {
	h := Server{
		Tokens:        Tokens{"granted": "token-a", "peer": "token-b", "none": "token-c"},
		QuotaAccounts: QuotaAccounts{"granted": {"codex:0123456789abcdef": true}, "peer": {"codex:aaaaaaaabbbbbbbb": true}},
	}.Handler()
	for _, tc := range []struct {
		name, method, path, body, token string
		status                          int
	}{
		{"put foreign", "PUT", "/v1/quota/accounts/codex:0123456789abcdef/observation", `{}`, "token-b", 403},
		{"lease foreign", "POST", "/v1/quota/accounts/codex:0123456789abcdef/lease", `{}`, "token-b", 403},
		{"release foreign", "DELETE", "/v1/quota/accounts/codex:0123456789abcdef/lease", `{}`, "token-b", 403},
		{"put ungranted", "PUT", "/v1/quota/accounts/claude:0123456789abcdef/observation", `{}`, "token-a", 403},
		{"lease no grants", "POST", "/v1/quota/accounts/codex:0123456789abcdef/lease", `{}`, "token-c", 403},
		// A granted key passes the scope check and reaches body handling.
		{"put granted", "PUT", "/v1/quota/accounts/codex:0123456789abcdef/observation", `{}`, "token-a", 400},
		{"lease granted", "POST", "/v1/quota/accounts/codex:0123456789abcdef/lease", `{}`, "token-a", 400},
		{"release granted", "DELETE", "/v1/quota/accounts/codex:0123456789abcdef/lease", `{}`, "token-a", 400},
		{"put bad key", "PUT", "/v1/quota/accounts/invalid/observation", `{}`, "token-a", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
			}
			if tc.status == 403 && !strings.Contains(w.Body.String(), "quota_account_forbidden") {
				t.Fatalf("error body: %s", w.Body)
			}
		})
	}
}

// Quota grant lines never refuse startup: an unknown client, a bad key or an
// empty client id is logged to stderr and skipped, leaving no scope.
func TestQuotaGrantFile(t *testing.T) {
	for name, body := range map[string]string{
		"bad key":        "HANDOFFKEEP_TOKEN_a=x\nHANDOFFKEEP_QUOTA_a=invalid\n",
		"unknown client": "HANDOFFKEEP_TOKEN_a=x\nHANDOFFKEEP_QUOTA_b=codex:0123456789abcdef\n",
		"empty client":   "HANDOFFKEEP_TOKEN_a=x\nHANDOFFKEEP_QUOTA_=codex:0123456789abcdef\n",
		"empty value":    "HANDOFFKEEP_TOKEN_a=x\nHANDOFFKEEP_QUOTA_a=\n",
		"no equals":      "HANDOFFKEEP_TOKEN_a=x\nHANDOFFKEEP_QUOTA_a\n",
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "auth.env")
			if err := os.WriteFile(p, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			_, grants, err := LoadTokens(p)
			if err != nil {
				t.Fatalf("quota grant refused startup: %v", err)
			}
			if len(grants) != 0 {
				t.Fatalf("skipped grant left scope: %v", grants)
			}
		})
	}
	p := filepath.Join(t.TempDir(), "auth.env")
	body := "HANDOFFKEEP_TOKEN_a=x\nHANDOFFKEEP_QUOTA_a=codex:0123456789abcdef\nHANDOFFKEEP_QUOTA_a=claude:0123456789abcdef\nHANDOFFKEEP_TOKEN_b=y\n"
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	_, grants, err := LoadTokens(p)
	if err != nil {
		t.Fatal(err)
	}
	if !grants["a"]["codex:0123456789abcdef"] || !grants["a"]["claude:0123456789abcdef"] || len(grants["a"]) != 2 || len(grants["b"]) != 0 {
		t.Fatalf("grants=%v", grants)
	}
}
