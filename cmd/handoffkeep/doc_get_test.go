package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// doc get --id resolves a document through GET /v1/documents?id=<n> and the
// positional-key contract is unchanged (#551).
func TestDocGetByID(t *testing.T) {
	doc := store.Document{ID: 5, Key: "k/five", Kind: "note", Session: "s", Body: "five body"}
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/documents" && r.URL.Query().Get("id") == "5":
			queries = append(queries, r.URL.RawQuery)
			_ = json.NewEncoder(w).Encode(doc)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/documents" && r.URL.Query().Get("id") == "9":
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not_found"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/documents/k/five":
			_ = json.NewEncoder(w).Encode(doc)
		default:
			t.Fatalf("unexpected %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")

	for _, arg := range []string{"5", "#5"} {
		var out bytes.Buffer
		if err := docCmd([]string{"get", "--id", arg}, &out); err != nil {
			t.Fatalf("--id %s: %v", arg, err)
		}
		var got store.Document
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("--id %s output: %v", arg, err)
		}
		if got.ID != 5 || got.Key != "k/five" {
			t.Fatalf("--id %s doc=%+v", arg, got)
		}
	}
	// A missing id fails as not_found, never an empty success.
	if err := docCmd([]string{"get", "--id", "9"}, &bytes.Buffer{}); err == nil || err.Error() != "not_found" {
		t.Fatalf("missing id err=%v", err)
	}
	// Malformed ids fail client-side without reaching the server.
	before := len(queries)
	for _, arg := range []string{"abc", "-3", "0", "1.5", "k/five", "5x"} {
		if err := docCmd([]string{"get", "--id", arg}, &bytes.Buffer{}); err == nil || err.Error() != "invalid document id" {
			t.Fatalf("--id %s err=%v", arg, err)
		}
	}
	if len(queries) != before {
		t.Fatalf("malformed ids reached the server: %v", queries)
	}
	// --id cannot be combined with a positional key.
	if err := docCmd([]string{"get", "--id", "5", "k/five"}, &bytes.Buffer{}); err == nil {
		t.Fatal("--id with key must fail")
	}
	// The positional-key contract is unchanged.
	var out bytes.Buffer
	if err := docCmd([]string{"get", "k/five"}, &out); err != nil {
		t.Fatal(err)
	}
	var got store.Document
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.ID != 5 {
		t.Fatalf("key get doc=%+v err=%v", got, err)
	}
}

// ctx search defaults to 20 rows for every scope (the 3-per-query cap made
// discovery unusable) while an explicit --limit is still honored (#551).
func TestCtxSearchDefaultLimit(t *testing.T) {
	var limits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/search":
			limits = append(limits, r.URL.Query().Get("limit"))
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []store.SearchResult{}})
		case "/v1/checkpoints":
			limits = append(limits, "recent:"+r.URL.Query().Get("limit"))
			_ = json.NewEncoder(w).Encode(map[string]any{"checkpoints": []store.Checkpoint{}})
		default:
			t.Fatalf("path=%s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")

	for _, scope := range []string{"all", "docs", "ctx", "memory", "tasks"} {
		if err := ctxCmd([]string{"search", "--scope", scope, "probe"}, &bytes.Buffer{}); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
	}
	if err := ctxCmd([]string{"search", "--limit", "7", "probe"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	// ctx recent keeps its own default of 3.
	if err := ctxCmd([]string{"recent", "--session", "s"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := []string{"20", "20", "20", "20", "20", "7", "recent:3"}
	if strings.Join(limits, ",") != strings.Join(want, ",") {
		t.Fatalf("limits=%v want %v", limits, want)
	}
}
