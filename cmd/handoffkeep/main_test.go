package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestListenValidation(t *testing.T) {
	for _, a := range []string{"0.0.0.0:8800", "8.8.8.8:8800", "[::]:8800"} {
		if validListen(a, false) == nil {
			t.Fatalf("unsafe bind accepted: %s", a)
		}
	}
	if err := validListen("127.0.0.1:8800", false); err != nil {
		t.Fatal(err)
	}
	if err := validListen("100.122.100.56:8800", true); err != nil {
		t.Fatal(err)
	}
}

func TestR2AlertThresholdBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		value       int64
		which, want string
	}{
		{"a699", 699000, "a", ""}, {"a700", 700000, "a", "70%"}, {"a899", 899000, "a", "70%"}, {"a900", 900000, "a", "90%"},
		{"b699", 6990000, "b", ""}, {"b700", 7000000, "b", "70%"}, {"b899", 8990000, "b", "70%"}, {"b900", 9000000, "b", "90%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := int64(0), int64(0)
			if tc.which == "a" {
				a = tc.value
			} else {
				b = tc.value
			}
			got := r2AlertReason(0, a, b)
			if tc.want == "" && got != "" {
				t.Fatalf("got %q", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("got %q want %s", got, tc.want)
			}
		})
	}
}

func TestMemoryPushPullPreservesBytes(t *testing.T) {
	var saved store.Memory
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/memory/hk-probe/probe-2":
			if err := json.NewDecoder(r.Body).Decode(&saved); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(saved)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memory/hk-probe":
			_ = json.NewEncoder(w).Encode(map[string]any{"memory": []store.Memory{{Agent: saved.Agent, Name: saved.Name, Description: saved.Description, Type: saved.Type}}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/memory/hk-probe/probe-2":
			_ = json.NewEncoder(w).Encode(saved)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	source, destination := t.TempDir(), t.TempDir()
	raw := "---\nname: probe-2\ndescription: retained description\nmetadata:\n  type: reference\n---\n\n프로브 메모리 2 본문\n"
	if err := os.WriteFile(filepath.Join(source, "probe-2.md"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := memoryCmd([]string{"push", "--agent", "hk-probe", "--dir", source, "--apply"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if saved.Content != raw {
		t.Fatalf("push content=%q want raw=%q", saved.Content, raw)
	}
	if err := memoryCmd([]string{"pull", "--agent", "hk-probe", "--dir", destination, "--apply"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "probe-2.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(raw)) {
		t.Fatalf("round trip differs\ngot:  %q\nwant: %q", got, raw)
	}
}

func TestCheckpointRefsAndSearchFlagsAfterQuery(t *testing.T) {
	var checkpoint store.Checkpoint
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/checkpoints":
			if err := json.NewDecoder(r.Body).Decode(&checkpoint); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(checkpoint)
		case "/v1/search":
			if r.URL.Query().Get("q") != "실증" || r.URL.Query().Get("session") != "hk-probe" {
				t.Fatalf("query=%s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []store.SearchResult{{
				Scope: "ctx", Session: "hk-probe", Refs: store.Refs{"prs": []string{"26"}},
			}}})
		default:
			t.Fatalf("path=%s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	if err := ctxCmd([]string{"checkpoint", "--session", "hk-probe", "--title", "test", "--body", "body", "--ref", "prs=26", "--ref", "prs=27", "--ref", "jobs=hk-v0"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	want := store.Refs{"prs": []string{"26", "27"}, "jobs": []string{"hk-v0"}}
	if !reflect.DeepEqual(checkpoint.Refs, want) {
		t.Fatalf("refs=%#v", checkpoint.Refs)
	}
	var out bytes.Buffer
	if err := ctxCmd([]string{"search", "실증", "--session", "hk-probe"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := ctxCmd([]string{"search", "--session", "hk-probe", "실증"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"refs":{"prs":["26"]}`)) {
		t.Fatalf("search did not print refs: %s", out.String())
	}
}

func TestTasksNextEmptyUsesExitCodeThree(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/next" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "queue_empty"})
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	err := run([]string{"tasks", "next", "--lane", "empty-lane", "--by", "captain"}, &bytes.Buffer{}, &bytes.Buffer{})
	var exit exitCodeError
	if !errors.As(err, &exit) || exit.code != 3 {
		t.Fatalf("error=%v", err)
	}
}

// TestTasksExportPrintsDocumentVerbatim proves the CLI forwards the evidence
// document byte-for-byte: the served body and stdout must be identical, and
// every evidence field decoded from stdout must equal the served value.
func TestTasksExportPrintsDocumentVerbatim(t *testing.T) {
	document := []byte(`{"snapshot_id":"10:14:10,11,12","db_time":"2026-09-19T08:00:00.123456Z","source":{"vcs_revision":"0123456789abcdef"},"scope":{"lane":"lane-a","state":"backlog","parent_lane":"parent-a","limit":7},"watermark":{"task_event_max_id":42,"relay_event_max_id":9},"counts":{"total":2,"by_state":{"backlog":2},"by_lane":{"lane-a":2}},"rows_returned":2,"complete":true,"truncated":false,"ids_sha256":"` + "a1b2c3" + `","rows_sha256":"` + "d4e5f6" + `","tasks":[{"id":3,"lane":"lane-a","parent_lane":"parent-a","title":"유니코드 <&> \"quoted\"","kind":"implement","state":"backlog","priority":5,"refs":{"pr":"12","head_sha":"abc"},"created_by":"node","created_at":"2026-09-19T07:59:00Z","updated_at":"2026-09-19T07:59:00Z"},{"id":9,"lane":"lane-a","parent_lane":"parent-a","title":"second","kind":"verify","state":"backlog","priority":0,"refs":{},"claimed_by":"cap","created_by":"node","created_at":"2026-09-19T07:59:30Z","updated_at":"2026-09-19T07:59:30Z"}]}` + "\n")
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tasks/export" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(document)
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")

	var out bytes.Buffer
	if err := tasksCmd([]string{"export", "--lane", "lane-a", "--state", "backlog", "--parent-lane", "parent-a", "--limit", "7"}, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), document) {
		t.Fatalf("stdout differs from served document\ngot:  %q\nwant: %q", out.Bytes(), document)
	}
	if len(queries) != 1 {
		t.Fatalf("queries=%v", queries)
	}
	values, err := url.ParseQuery(queries[0])
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"lane": "lane-a", "state": "backlog", "parent_lane": "parent-a", "limit": "7"} {
		if got := values.Get(key); got != want {
			t.Fatalf("query %s=%q want %q", key, got, want)
		}
	}

	var served, printed map[string]json.RawMessage
	if err := json.Unmarshal(document, &served); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &printed); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"snapshot_id", "db_time", "watermark", "counts", "ids_sha256", "rows_sha256", "tasks"} {
		if !bytes.Equal(served[field], printed[field]) {
			t.Fatalf("field %s differs: %s vs %s", field, served[field], printed[field])
		}
	}

	// Without --limit the route's own default applies; the CLI must not
	// invent one on the wire.
	out.Reset()
	if err := tasksCmd([]string{"export"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Fatalf("queries=%v", queries)
	}
	if values, err = url.ParseQuery(queries[1]); err != nil || values.Get("limit") != "" {
		t.Fatalf("limit=%q err=%v", values.Get("limit"), err)
	}
	if !bytes.Equal(out.Bytes(), document) {
		t.Fatal("stdout differs from served document")
	}
}

// TestTasksExportRejectsInvalidLimit proves an explicitly passed --limit
// outside 1..ExportLimitMax fails client-side with invalid_export_query and
// never reaches the route, while an in-range --limit is sent through.
func TestTasksExportRejectsInvalidLimit(t *testing.T) {
	var queries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tasks/export" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"snapshot_id":"1:2:","tasks":[]}` + "\n"))
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")

	for _, value := range []string{"0", "-5", "10001"} {
		if err := tasksCmd([]string{"export", "--limit", value}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "invalid_export_query") {
			t.Fatalf("--limit %s error=%v", value, err)
		}
	}
	if len(queries) != 0 {
		t.Fatalf("invalid limits reached the route: %v", queries)
	}

	var out bytes.Buffer
	if err := tasksCmd([]string{"export", "--limit", "7"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 1 {
		t.Fatalf("queries=%v", queries)
	}
	if values, err := url.ParseQuery(queries[0]); err != nil || values.Get("limit") != "7" {
		t.Fatalf("query=%q err=%v", queries[0], err)
	}
}

func TestTasksExportErrorSurfacesCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_export_query"})
	}))
	defer server.Close()
	t.Setenv("HANDOFFKEEP_URL", server.URL)
	t.Setenv("HANDOFFKEEP_TOKEN", "test-token")
	if err := tasksCmd([]string{"export", "--limit", "abc"}, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error")
	}
	if err := tasksCmd([]string{"export"}, &bytes.Buffer{}); err == nil || err.Error() != "invalid_export_query" {
		t.Fatalf("error=%v", err)
	}
}
