package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestAttachMigrateDryRunOpensLegacySchema(t *testing.T) {
	_, dbURL := testdb.Schema(t, "attach_migrate")
	st, err := store.Open(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = st.PutAttachment(t.Context(), store.Attachment{
		SHA256: strings.Repeat("a", 64), SizeBytes: 23, MIME: "text/plain",
		OriginalName: "brief.txt", CreatedBy: "test", RefKind: "none",
	}, 1024, 10)
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	for _, prefix := range []string{"HKFROM", "HKTO"} {
		t.Setenv(prefix+"_S3_ENDPOINT", "http://127.0.0.1:9000")
		t.Setenv(prefix+"_S3_BUCKET", "attachments")
		t.Setenv(prefix+"_S3_ACCESS_KEY_ID", "test")
		t.Setenv(prefix+"_S3_SECRET_ACCESS_KEY", "test-secret")
	}
	var out bytes.Buffer
	if err := run([]string{"attach", "migrate", "--from", "HKFROM", "--to", "HKTO"}, &out, &out); err != nil {
		t.Fatalf("attach migrate dry-run: %v", err)
	}
	var got struct {
		DryRun  bool  `json:"dry_run"`
		Objects int64 `json:"objects"`
		Bytes   int64 `json:"bytes"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode output %q: %v", out.String(), err)
	}
	if !got.DryRun || got.Objects != 1 || got.Bytes != 23 {
		t.Fatalf("output = %+v, want dry_run=true objects=1 bytes=23", got)
	}
}

func TestAttachMigrateDryRunDoesNotMigrateSchema(t *testing.T) {
	schema, dbURL := testdb.Schema(t, "attach_migrate_unmigrated")
	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	for _, prefix := range []string{"HKFROM", "HKTO"} {
		t.Setenv(prefix+"_S3_ENDPOINT", "http://127.0.0.1:9000")
		t.Setenv(prefix+"_S3_BUCKET", "attachments")
		t.Setenv(prefix+"_S3_ACCESS_KEY_ID", "test")
		t.Setenv(prefix+"_S3_SECRET_ACCESS_KEY", "test-secret")
	}
	var out bytes.Buffer
	if err := run([]string{"attach", "migrate", "--from", "HKFROM", "--to", "HKTO"}, &out, &out); err == nil {
		t.Fatal("dry-run unexpectedly succeeded on an unmigrated schema")
	}
	adminURL := testdb.URL(t)
	admin, err := pgxpool.New(t.Context(), adminURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var exists bool
	if err = admin.QueryRow(t.Context(), `SELECT to_regclass($1) IS NOT NULL`, schema+`.schema_version`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("dry-run created schema_version in the unmigrated schema")
	}
}

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
