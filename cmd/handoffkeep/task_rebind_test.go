package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTasksRebindCLIUsesRouteAndRecordsRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", "")
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", "")
	var method, path, authorization string
	var body map[string]string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, authorization = r.Method, r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42,"state":"claimed","claimed_by":"new-builder","refs":{"job_id":"job-new"}}`)
	}))
	defer h.Close()

	var out bytes.Buffer
	err := run([]string{"tasks", "rebind", "42", "--from-job", "job-old", "--to-job", "job-new", "--by", "new-builder", "--reason", "builder takeover", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/v1/tasks/42/rebind" || authorization != "Bearer tok" {
		t.Fatalf("request method=%q path=%q authorization=%q", method, path, authorization)
	}
	if body["from_job"] != "job-old" || body["to_job"] != "job-new" || body["by"] != "new-builder" || body["reason"] != "builder takeover" {
		t.Fatalf("request body=%v", body)
	}
	if !strings.Contains(out.String(), `"job_id":"job-new"`) {
		t.Fatalf("output=%s", out.String())
	}
}

func TestTasksRebindCLIExplainsMissingServerRoute(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_ID", "")
	t.Setenv("HANDOFFKEEP_CF_ACCESS_CLIENT_SECRET", "")
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/42/rebind" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		http.NotFound(w, r)
	}))
	defer h.Close()

	err := run([]string{"tasks", "rebind", "42", "--from-job", "job-old", "--to-job", "job-new", "--by", "new-builder", "--reason", "builder takeover", "--url", h.URL, "--token", "tok"}, &bytes.Buffer{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "task_rebind_route_missing") ||
		!strings.Contains(err.Error(), "POST /v1/tasks/{id}/rebind") ||
		!strings.Contains(err.Error(), "minimum server version") {
		t.Fatalf("error=%v, want clear missing-route/version-skew error", err)
	}
}
