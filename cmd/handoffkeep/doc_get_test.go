package main

import (
	"bytes"
	"encoding/json"
	"io"
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
	for _, arg := range []string{"", "abc", "-3", "0", "1.5", "k/five", "5x"} {
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

// A server that predates ?id= ignores the parameter and answers 200 with the
// documents list envelope. That must surface as an error, never as an empty
// document with rc 0 (#584).
func TestDocGetByIDOldServerSkew(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"documents": []store.Document{
			{ID: 3, Key: "k/three", Kind: "note"},
		}})
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")

	for _, id := range []string{"1", "3", "999999"} {
		var out bytes.Buffer
		err := docCmd([]string{"get", "--id", id}, &out)
		if err == nil {
			t.Fatalf("--id %s: expected error, got output %s", id, out.String())
		}
		if !strings.Contains(err.Error(), "?id=") {
			t.Fatalf("--id %s: error should name the unsupported lookup: %v", id, err)
		}
		if strings.Contains(out.String(), `"id"`) {
			t.Fatalf("--id %s: printed a document on failure: %s", id, out.String())
		}
	}
}

// doc list --after-id pages by id and prints the next cursor; without the flag
// the request and the printed array are the ones the command always sent.
func TestDocListCursor(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/documents" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"documents":     []store.Document{{ID: 9, Key: "notes/b", Kind: "note"}},
			"next_after_id": 9,
		})
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")

	var out bytes.Buffer
	if err := docCmd([]string{"list", "--prefix", "notes/", "--after-id", "0"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 || queries[0] != "after_id=0&kind=&limit=100&prefix=notes%2F&session=" {
		t.Fatalf("query=%v", queries)
	}
	var printed map[string]any
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil || printed["next_after_id"] != float64(9) {
		t.Fatalf("out=%s err=%v", out.String(), err)
	}

	out.Reset()
	if err := docCmd([]string{"list", "--prefix", "notes/"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 || queries[1] != "kind=&limit=100&prefix=notes%2F&session=" {
		t.Fatalf("query=%v", queries)
	}
	if !strings.HasPrefix(out.String(), "[") {
		t.Fatalf("no-cursor list printed %q, want the plain array", out.String())
	}
	if err := docCmd([]string{"list", "--after-id", "-1"}, io.Discard); err == nil {
		t.Fatal("negative --after-id accepted")
	}
}
