package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// GET /v1/documents?id=<n> fetches one document by numeric id behind the
// existing bearer auth; it is a read-only extension of the collection route
// and never a write path (#551).
func TestDocumentGetByIDRoute(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL integration tests")
	}
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token"}}.Handler())
	defer h.Close()

	key := fmt.Sprintf("k/idroute-%d", time.Now().UnixNano())
	resp := request(t, h.Client(), http.MethodPut, h.URL+"/v1/documents/"+key, "node-token", map[string]any{"kind": "note", "body": "id route body"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put=%d", resp.StatusCode)
	}
	var put struct {
		Document store.Document `json:"document"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&put); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	id := put.Document.ID

	// Found: 200 with the same document the key route returns.
	resp = request(t, h.Client(), http.MethodGet, fmt.Sprintf("%s/v1/documents?id=%d", h.URL, id), "node-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get id=%d status=%d", id, resp.StatusCode)
	}
	var got store.Document
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.ID != id || got.Key != key || got.Body != "id route body" {
		t.Fatalf("doc=%+v", got)
	}

	// Missing: 404 not_found, distinct from a 200 empty payload.
	resp = request(t, h.Client(), http.MethodGet, h.URL+"/v1/documents?id=999999999", "node-token", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing id status=%d", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error != "not_found" {
		t.Fatalf("missing id body err=%v e=%+v", err, e)
	}
	resp.Body.Close()

	// Malformed and non-positive ids are 400, not a list fallback.
	for _, bad := range []string{"abc", "0", "-5", "1.5"} {
		resp = request(t, h.Client(), http.MethodGet, h.URL+"/v1/documents?id="+bad, "node-token", nil)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("id=%s status=%d", bad, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// No id parameter: the route still lists.
	resp = request(t, h.Client(), http.MethodGet, h.URL+"/v1/documents?prefix="+key[:3], "node-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", resp.StatusCode)
	}
	resp.Body.Close()

	// Auth boundary is unchanged.
	resp = request(t, h.Client(), http.MethodGet, fmt.Sprintf("%s/v1/documents?id=%d", h.URL, id), "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", resp.StatusCode)
	}
	resp.Body.Close()
}
