package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
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

// Presence guards run before the store: authentication, body shape, field
// validation, then the same per-client grant the observation PUT uses.
func TestQuotaPresenceGuards(t *testing.T) {
	h := Server{
		Tokens:        Tokens{"granted": "token-a", "peer": "token-b", "none": "token-c"},
		QuotaAccounts: QuotaAccounts{"granted": {"codex:0123456789abcdef": true}, "peer": {"codex:aaaaaaaabbbbbbbb": true}},
	}.Handler()
	now := time.Now().UTC().Format(time.RFC3339)
	body := func(host, key, at string) string {
		return `{"host_id":"` + host + `","account_key":"` + key + `","last_seen":"` + at + `"}`
	}
	for _, tc := range []struct {
		name, method, path, body, token, err string
		status                               int
	}{
		{"get unauthenticated", "GET", "/v1/quota/presence", "", "", "unauthorized", 401},
		{"put unauthenticated", "PUT", "/v1/quota/presence", body("host-a", "codex:0123456789abcdef", now), "", "unauthorized", 401},
		{"put foreign", "PUT", "/v1/quota/presence", body("host-a", "codex:0123456789abcdef", now), "token-b", "quota_account_forbidden", 403},
		{"put ungranted", "PUT", "/v1/quota/presence", body("host-a", "claude:0123456789abcdef", now), "token-a", "quota_account_forbidden", 403},
		{"put no grants", "PUT", "/v1/quota/presence", body("host-a", "codex:0123456789abcdef", now), "token-c", "quota_account_forbidden", 403},
		{"put bad host", "PUT", "/v1/quota/presence", body("host/a", "codex:0123456789abcdef", now), "token-a", "invalid_quota_presence", 400},
		{"put long host", "PUT", "/v1/quota/presence", body(strings.Repeat("h", 65), "codex:0123456789abcdef", now), "token-a", "invalid_quota_presence", 400},
		{"put bad key", "PUT", "/v1/quota/presence", body("host-a", "codex:raw-identity", now), "token-a", "invalid_quota_presence", 400},
		{"put future", "PUT", "/v1/quota/presence", body("host-a", "codex:0123456789abcdef", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)), "token-a", "invalid_quota_presence", 400},
		{"put missing last_seen", "PUT", "/v1/quota/presence", `{"host_id":"host-a","account_key":"codex:0123456789abcdef"}`, "token-a", "invalid_quota_presence", 400},
		{"put unknown field", "PUT", "/v1/quota/presence", `{"host_id":"host-a","account_key":"codex:0123456789abcdef","last_seen":"` + now + `","hostname":"x"}`, "token-a", "invalid_quota_request", 400},
		{"put trailing", "PUT", "/v1/quota/presence", body("host-a", "codex:0123456789abcdef", now) + ` {}`, "token-a", "invalid_quota_request", 400},
		{"put oversized", "PUT", "/v1/quota/presence", strings.Repeat(" ", 33<<10), "token-a", "invalid_quota_request", 400},
		{"get bad host filter", "GET", "/v1/quota/presence?host_id=host%2Fa", "", "token-c", "invalid_quota_presence_filter", 400},
		{"get bad key filter", "GET", "/v1/quota/presence?account_key=invalid", "", "token-c", "invalid_quota_presence_filter", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.token != "" {
				r.Header.Set("Authorization", "Bearer "+tc.token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.err) {
				t.Fatalf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body)
			}
		})
	}
}

// decodeRuntugPresence mirrors runtug's fleet.go decoder: a bare non-null
// array first, then a non-null list under records/presence/rows.
func decodeRuntugPresence(body []byte) ([]store.QuotaPresence, bool) {
	var rows []store.QuotaPresence
	if json.Unmarshal(body, &rows) == nil && rows != nil {
		return rows, true
	}
	var env map[string]json.RawMessage
	if json.Unmarshal(body, &env) != nil {
		return nil, false
	}
	for _, k := range []string{"records", "presence", "rows"} {
		if v, ok := env[k]; ok && json.Unmarshal(v, &rows) == nil && rows != nil {
			return rows, true
		}
	}
	return nil, false
}

func TestQuotaPresenceAPIPostgres(t *testing.T) {
	_, st := relaneAPIServer(t)
	srv := httptest.NewServer(Server{
		Service:       Service{Store: st},
		Tokens:        Tokens{"writer": "token-w", "reader": "token-r"},
		QuotaAccounts: QuotaAccounts{"writer": {"codex:0123456789abcdef": true}},
	}.Handler())
	t.Cleanup(srv.Close)
	call := func(method, path, token, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}
	// An empty table is an empty list, never JSON null: runtug must parse it.
	code, out := call("GET", "/v1/quota/presence", "token-r", "")
	if rows, ok := decodeRuntugPresence(out); code != 200 || !ok || len(rows) != 0 {
		t.Fatalf("empty presence %d %s", code, out)
	}
	seen := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	put := `{"host_id":"host-0123456789abcdef","account_key":"codex:0123456789abcdef","last_seen":"` + seen.Format(time.RFC3339) + `"}`
	if code, out = call("PUT", "/v1/quota/presence", "token-w", put); code != 200 {
		t.Fatalf("put presence %d %s", code, out)
	}
	// A reader without grants may read but not write.
	if code, out = call("PUT", "/v1/quota/presence", "token-r", put); code != 403 {
		t.Fatalf("ungranted put %d %s", code, out)
	}
	stale := `{"host_id":"host-0123456789abcdef","account_key":"codex:0123456789abcdef","last_seen":"` + seen.Add(-time.Hour).Format(time.RFC3339) + `"}`
	code, out = call("PUT", "/v1/quota/presence", "token-w", stale)
	var stored store.QuotaPresence
	if code != 200 || json.Unmarshal(out, &stored) != nil || !stored.LastSeen.Equal(seen) {
		t.Fatalf("stale put %d %s", code, out)
	}
	for _, path := range []string{"/v1/quota/presence", "/v1/quota/presence?account_key=codex:0123456789abcdef", "/v1/quota/presence?host_id=host-0123456789abcdef"} {
		code, out = call("GET", path, "token-r", "")
		rows, ok := decodeRuntugPresence(out)
		if code != 200 || !ok || len(rows) != 1 || rows[0].HostID != "host-0123456789abcdef" || rows[0].AccountKey != "codex:0123456789abcdef" || !rows[0].LastSeen.Equal(seen) {
			t.Fatalf("GET %s: %d %s", path, code, out)
		}
	}
	code, out = call("GET", "/v1/quota/presence?account_key=claude:0123456789abcdef", "token-r", "")
	if rows, ok := decodeRuntugPresence(out); code != 200 || !ok || len(rows) != 0 {
		t.Fatalf("filtered-out presence %d %s", code, out)
	}
}
