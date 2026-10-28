package tests

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

const quotaE2EKey = "codex:0123456789abcdef"
const quotaE2EPeerKey = "claude:0123456789abcdef"

// Write paths are scoped to the client's granted account keys; reads stay open.
func TestQuotaWriteScopePostgres(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL integration tests")
	}
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewServer(api.Server{
		Service: api.Service{Store: s},
		Tokens:  api.Tokens{"writer": "writer-token", "peer": "peer-token"},
		QuotaAccounts: api.QuotaAccounts{
			"writer": {quotaE2EKey: true},
			"peer":   {quotaE2EPeerKey: true},
		},
	}.Handler())
	defer h.Close()
	c := h.Client()
	obs := map[string]any{
		"holder": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"observation": map[string]any{
			"kind":        "auto",
			"measured_at": time.Now().UTC().Format(time.RFC3339),
			"buckets":     []map[string]any{{"window": "5h", "used_pct": 40, "scope": "account"}},
		},
	}
	// A foreign account_key refuses 403 on each write verb.
	for _, tc := range []struct{ method, suffix string }{
		{"PUT", "/observation"},
		{"POST", "/lease"},
		{"DELETE", "/lease"},
	} {
		res := request(t, c, tc.method, h.URL+"/v1/quota/accounts/"+quotaE2EKey+tc.suffix, "peer-token", map[string]any{"holder": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
		if res.StatusCode != 403 {
			t.Fatalf("%s foreign: %d", tc.method, res.StatusCode)
		}
		var body map[string]string
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil || body["error"] != "quota_account_forbidden" {
			t.Fatalf("%s foreign body: %v %+v", tc.method, err, body)
		}
		res.Body.Close()
	}
	// The writer's own unlisted key refuses too.
	res := request(t, c, "POST", h.URL+"/v1/quota/accounts/"+quotaE2EPeerKey+"/lease", "writer-token", map[string]any{"holder": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if res.StatusCode != 403 {
		t.Fatalf("unlisted key: %d", res.StatusCode)
	}
	res.Body.Close()
	// Reads are open: the peer reads the writer's account.
	res = request(t, c, "GET", h.URL+"/v1/quota/accounts/"+quotaE2EKey+"/latest", "peer-token", nil)
	if res.StatusCode != 200 {
		t.Fatalf("read foreign: %d", res.StatusCode)
	}
	res.Body.Close()
	// Happy path on the granted key: lease, publish, identical republish.
	res = request(t, c, "POST", h.URL+"/v1/quota/accounts/"+quotaE2EKey+"/lease", "writer-token", map[string]any{"holder": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "ttl_s": 120})
	var lease struct {
		Acquired bool `json:"acquired"`
	}
	if err := json.NewDecoder(res.Body).Decode(&lease); err != nil || res.StatusCode != 200 || !lease.Acquired {
		t.Fatalf("lease: %d %+v %v", res.StatusCode, lease, err)
	}
	res.Body.Close()
	res = request(t, c, "PUT", h.URL+"/v1/quota/accounts/"+quotaE2EKey+"/observation", "writer-token", obs)
	var first store.QuotaObservation
	if err := json.NewDecoder(res.Body).Decode(&first); err != nil || res.StatusCode != 201 {
		t.Fatalf("publish: %d %v", res.StatusCode, err)
	}
	res.Body.Close()
	res = request(t, c, "PUT", h.URL+"/v1/quota/accounts/"+quotaE2EKey+"/observation", "writer-token", obs)
	var again store.QuotaObservation
	if err := json.NewDecoder(res.Body).Decode(&again); err != nil || res.StatusCode != 200 || again.ID != first.ID {
		t.Fatalf("dedupe publish: %d id=%d want=%d %v", res.StatusCode, again.ID, first.ID, err)
	}
	res.Body.Close()
	res = request(t, c, "DELETE", h.URL+"/v1/quota/accounts/"+quotaE2EKey+"/lease", "writer-token", map[string]any{"holder": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	if res.StatusCode != 204 {
		t.Fatalf("release: %d", res.StatusCode)
	}
	res.Body.Close()
}
