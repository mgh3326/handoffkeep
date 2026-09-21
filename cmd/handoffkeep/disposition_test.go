package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const testOriginPR = "https://github.com/example/repo/pull/27"

func writeTestFile(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The CLI copies merge facts from gh pr view output and counts residuals from
// the file; it never takes a hand-typed SHA or count.
func TestDispositionAddCopiesToolFacts(t *testing.T) {
	var created store.DispositionInput
	var docKey, docBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/documents/"):
			var doc store.Document
			_ = json.NewDecoder(r.Body).Decode(&doc)
			docKey, docBody = strings.TrimPrefix(r.URL.Path, "/v1/documents/"), doc.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"document": doc, "changed": true})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/tasks/dispositions":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"task": store.Task{ID: 9}, "created": true})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	gh := writeTestFile(t, "pr.json", `{"url":"`+testOriginPR+`","state":"MERGED","mergeCommit":{"oid":"`+strings.Repeat("b", 40)+`"},"mergedAt":"2026-09-21T01:02:03Z"}`)
	residuals := writeTestFile(t, "r.json", `["log rotation unverified",{"title":"probe missing","severity":"SHOULD","evidence":"report §3"}]`)
	var out bytes.Buffer
	err := run([]string{"tasks", "disposition", "add", "--lane", "lane-a", "--origin-pr", testOriginPR, "--gh-json", gh, "--residuals", residuals, "--recommended", "A"}, &out, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if created.MergeSHA != strings.Repeat("b", 40) || created.MergedAt == nil || created.ResidualN != 2 || created.ResidualDoc != docKey || created.Install.State != "unknown" || created.Title != "[처분] "+testOriginPR {
		t.Fatalf("created input=%+v doc=%s", created, docKey)
	}
	if !strings.HasPrefix(docKey, "disposition/") || !strings.Contains(docBody, "1. log rotation unverified") || !strings.Contains(docBody, "2. [SHOULD] probe missing — report §3") {
		t.Fatalf("residual doc %s=%q", docKey, docBody)
	}
}

func TestDispositionAddRejectsUnverifiedGHOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no request expected, got %s %s", r.Method, r.URL.Path)
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	sha := strings.Repeat("c", 40)
	cases := map[string]string{
		"open PR":     `{"url":"` + testOriginPR + `","state":"OPEN","mergeCommit":null,"mergedAt":null}`,
		"other PR":    `{"url":"https://github.com/example/repo/pull/28","state":"MERGED","mergeCommit":{"oid":"` + sha + `"},"mergedAt":"2026-09-21T01:02:03Z"}`,
		"short sha":   `{"url":"` + testOriginPR + `","state":"MERGED","mergeCommit":{"oid":"abc1234"},"mergedAt":"2026-09-21T01:02:03Z"}`,
		"no mergedAt": `{"url":"` + testOriginPR + `","state":"MERGED","mergeCommit":{"oid":"` + sha + `"}}`,
		"not json":    `merged!`,
	}
	for name, body := range cases {
		gh := writeTestFile(t, "pr.json", body)
		if err := run([]string{"tasks", "disposition", "add", "--lane", "lane-a", "--origin-pr", testOriginPR, "--gh-json", gh, "--recommended", "A"}, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, args := range map[string][]string{
		"no origin":       {"tasks", "disposition", "add", "--lane", "d", "--recommended", "A"},
		"both origins":    {"tasks", "disposition", "add", "--lane", "d", "--origin-pr", testOriginPR, "--origin-task", "3", "--recommended", "A"},
		"pr without gh":   {"tasks", "disposition", "add", "--lane", "d", "--origin-pr", testOriginPR, "--recommended", "A"},
		"multi-line item": {"tasks", "disposition", "add", "--lane", "d", "--origin-task", "3", "--residuals", writeTestFile(t, "r.json", `["a\nb"]`), "--recommended", "A"},
	} {
		if err := run(args, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDispositionSummaryPrintsServerLineVerbatim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tasks/dispositions/summary" || r.URL.Query().Get("as_of") != "2026-09-21T03:00:00Z" {
			t.Fatalf("unexpected %s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.DispositionSummary{Line: "미처분 3 · 최고령 2일", Detail: "적용 대기 1(최고령 0일)"})
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	var out bytes.Buffer
	if err := run([]string{"tasks", "disposition", "summary", "--as-of", "2026-09-21T03:00:00Z"}, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "미처분 3 · 최고령 2일\n적용 대기 1(최고령 0일)\n" {
		t.Fatalf("out=%q", out.String())
	}
}
