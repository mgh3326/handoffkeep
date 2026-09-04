package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestPostgresMigrationAndCoreRoundTrip(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL integration tests")
	}
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := api.Service{Store: s}
	got, err := svc.Checkpoint(t.Context(), "test-client", store.Checkpoint{Session: "hk-test", Kind: "checkpoint", Title: "core", Body: "round trip"})
	if err != nil || got.CreatedBy != "test-client" {
		t.Fatalf("checkpoint=%+v err=%v", got, err)
	}
	items, err := svc.Recent(t.Context(), "hk-test", "", 3)
	if err != nil || len(items) == 0 {
		t.Fatalf("recent=%v err=%v", items, err)
	}
	if _, err := svc.Checkpoint(t.Context(), "test-client", store.Checkpoint{Session: "hk-test", Kind: "checkpoint", Title: "bad", Body: "sk-" + strings.Repeat("a", 26)}); err == nil {
		t.Fatal("secret guard accepted content")
	}
}

func request(t *testing.T, c *http.Client, method, url, token string, body any) *http.Response {
	t.Helper()
	var r *bytes.Reader
	if body == nil {
		r = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHTTPPersistenceSearchAndRetention(t *testing.T) {
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for PostgreSQL integration tests")
	}
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token", "operator": "operator-token"}}.Handler())
	defer h.Close()
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/checkpoints", "", map[string]any{"session": "http-test", "kind": "checkpoint", "title": "x", "body": "x"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = request(t, h.Client(), http.MethodPost, h.URL+"/v1/checkpoints", "node-token", map[string]any{"session": "http-test", "kind": "decision", "title": "한국어 배포", "body": "trigram search text"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("checkpoint=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = request(t, h.Client(), http.MethodPut, h.URL+"/v1/documents/jobs/http-test/brief.md", "node-token", map[string]any{"kind": "brief", "session": "http-test", "job": "http-test", "body": "한국어 document search"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("document=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = request(t, h.Client(), http.MethodGet, h.URL+"/v1/search?q=%ED%95%9C%EA%B5%AD%EC%96%B4&scope=all&session=http-test", "operator-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("search=%d", resp.StatusCode)
	}
	var found struct {
		Results []store.SearchResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&found); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(found.Results) < 2 {
		t.Fatalf("search=%+v", found.Results)
	}
	for i := 0; i < store.CheckpointKeep+2; i++ {
		if _, err := s.CreateCheckpoint(t.Context(), store.Checkpoint{Session: "cap-test", Kind: "checkpoint", Title: "x", Body: "x", CreatedBy: "node"}); err != nil {
			t.Fatal(err)
		}
	}
	if xs, err := s.RecentCheckpoints(t.Context(), "cap-test", "", store.CheckpointKeep+1); err != nil || len(xs) != store.CheckpointKeep {
		t.Fatalf("checkpoint cap=%d err=%v", len(xs), err)
	}
	for i := 0; i < store.MemoryKeep+2; i++ {
		if _, err := s.PutMemory(t.Context(), store.Memory{Agent: "cap-agent", Name: "m" + strconv.Itoa(i), Type: "project", Content: "x", UpdatedBy: "node"}); err != nil {
			t.Fatal(err)
		}
	}
	if xs, err := s.ListMemory(t.Context(), "cap-agent", true); err != nil || len(xs) != store.MemoryKeep {
		t.Fatalf("memory cap=%d err=%v", len(xs), err)
	}
}
