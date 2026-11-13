package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestPostgresMigrationAndCoreRoundTrip(t *testing.T) {
	url := testdb.URL(t)
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	svc := api.Service{Store: s}
	doc, _, err := s.PutDocument(t.Context(), store.Document{Key: "core/round-trip", Kind: "note", Body: "round trip", CreatedBy: "test-client"})
	if err != nil || doc.CreatedBy != "test-client" {
		t.Fatalf("document=%+v err=%v", doc, err)
	}
	if _, err := svc.Search(t.Context(), "round", "docs", "", 5); err != nil {
		t.Fatalf("search: %v", err)
	}
	if _, _, err := s.PutDocument(t.Context(), store.Document{Key: "core/secret", Kind: "note", Body: "sk-" + strings.Repeat("a", 26), CreatedBy: "test-client"}); err == nil {
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
	url := testdb.URL(t)
	s, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token", "operator": "operator-token"}}.Handler())
	defer h.Close()
	resp := request(t, h.Client(), http.MethodPut, h.URL+"/v1/documents/jobs/http-test/note.md", "", map[string]any{"kind": "note", "session": "http-test", "job": "http-test", "body": "x"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = request(t, h.Client(), http.MethodPut, h.URL+"/v1/documents/jobs/http-test/note.md", "node-token", map[string]any{"kind": "note", "session": "http-test", "job": "http-test", "body": "한국어 배포 trigram search text"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("document=%d", resp.StatusCode)
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
	for i := 0; i < store.MemoryKeep+2; i++ {
		if _, err := s.PutMemory(t.Context(), store.Memory{Agent: "cap-agent", Name: "m" + strconv.Itoa(i), Type: "project", Content: "x", UpdatedBy: "node"}); err != nil {
			t.Fatal(err)
		}
	}
	if xs, err := s.ListMemory(t.Context(), "cap-agent", true); err != nil || len(xs) != store.MemoryKeep {
		t.Fatalf("memory cap=%d err=%v", len(xs), err)
	}
}
