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

// projectServer records requests and answers with a canned body per route
// shape: /v1/tasks for create, /v1/tasks/<id>/project for relabel,
// /v1/tasks/projects for the vocabulary endpoints.
func projectServer(t *testing.T, status int) (*httptest.Server, *[]relaneRequest) {
	t.Helper()
	var seen []relaneRequest
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := relaneRequest{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
		if r.URL.RawQuery != "" {
			x.Path += "?" + r.URL.RawQuery
		}
		_ = json.NewDecoder(r.Body).Decode(&x.Body)
		seen = append(seen, x)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch {
		case strings.HasSuffix(r.URL.Path, "/project"):
			_, _ = w.Write([]byte(`{"id":7,"project":"wrk"}`))
		case strings.HasSuffix(r.URL.Path, "/projects") && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"projects":["handoffkeep","wrk"]}`))
		case strings.HasSuffix(r.URL.Path, "/projects"):
			_, _ = w.Write([]byte(`{"name":"fleet-ops","created":true}`))
		case r.URL.Path == "/v1/tasks" && r.Method == http.MethodPost:
			_, _ = w.Write([]byte(`{"id":9,"project":"experiment"}`))
		case r.URL.Path == "/v1/tasks":
			// A project-aware server echoes the requested filter; the echo
			// is what lets the client detect an old server on an empty page.
			if r.URL.Query().Has("project") {
				resp, _ := json.Marshal(map[string]any{"tasks": []any{}, "project": r.URL.Query().Get("project")})
				_, _ = w.Write(resp)
			} else {
				_, _ = w.Write([]byte(`{"tasks":[]}`))
			}
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(h.Close)
	return h, &seen
}

// tasks add refuses to file a task without --project, locally — the refusal
// names the flag so a script author sees exactly what to add, and no request
// reaches the server.
func TestTasksAddRequiresProject(t *testing.T) {
	h, seen := projectServer(t, 201)
	var out bytes.Buffer
	err := run([]string{"tasks", "add", "--lane", "d", "--title", "x", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("missing-project add err=%v, want a refusal naming --project", err)
	}
	if len(*seen) != 0 {
		t.Fatalf("ungated add reached the server: %+v", *seen)
	}
}

// --project lands in the create body verbatim; the server-side vocabulary
// check stays authoritative (the CLI does not keep its own copy of the set).
func TestTasksAddSendsProject(t *testing.T) {
	h, seen := projectServer(t, 201)
	var out bytes.Buffer
	err := run([]string{"tasks", "add", "--lane", "d", "--title", "x", "--project", "experiment", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	req := (*seen)[0]
	if req.Method != "POST" || req.Path != "/v1/tasks" || req.Body["project"] != "experiment" {
		t.Fatalf("create request=%+v", req)
	}
}

// tasks list --project p narrows server-side via the query parameter; an
// explicit empty value selects the legacy NULL bucket, not "no filter".
func TestTasksListSendsProjectFilter(t *testing.T) {
	h, seen := projectServer(t, 200)
	var out bytes.Buffer
	err := run([]string{"tasks", "list", "--project", "wrk", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*seen)[0].Path, "project=wrk") {
		t.Fatalf("list path=%q", (*seen)[0].Path)
	}
	out.Reset()
	err = run([]string{"tasks", "list", "--project", "", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*seen)[1].Path, "project=") {
		t.Fatalf("legacy-bucket list path=%q", (*seen)[1].Path)
	}
}

// An old server silently drops ?project and returns an unfiltered list —
// including an empty one. With no echo key in the body the CLI must still
// fail loudly rather than print a legitimate-looking empty result.
func TestTasksListOldServerEmptyPageRefusal(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[]}`))
	}))
	t.Cleanup(old.Close)
	var out bytes.Buffer
	err := run([]string{"tasks", "list", "--project", "wrk", "--url", old.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "list_project_filter_ignored") {
		t.Fatalf("empty-page skew err=%v, want list_project_filter_ignored", err)
	}
	// The legacy bucket on an old server must fail too — unfiltered rows
	// decode as NULL project and would masquerade as the bucket.
	out.Reset()
	err = run([]string{"tasks", "list", "--project", "", "--url", old.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "list_project_filter_ignored") {
		t.Fatalf("legacy-bucket skew err=%v, want list_project_filter_ignored", err)
	}
}

// A project-filtered export against an old server returns an unfiltered
// document with no scope.project — the CLI must refuse it, not emit it.
func TestTasksExportOldServerRefusal(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"snapshot_id":"x","scope":{"lane":"","state":""},"tasks":[]}`))
	}))
	t.Cleanup(old.Close)
	var out bytes.Buffer
	err := run([]string{"tasks", "export", "--project", "wrk", "--url", old.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "export_project_filter_ignored") {
		t.Fatalf("old-server export err=%v, want export_project_filter_ignored", err)
	}
}

// tasks project posts {project, note} to the reclassification route; "by" is
// the token identity and must never be sent by the client.
func TestTasksProjectPostsRelabel(t *testing.T) {
	h, seen := projectServer(t, 200)
	var out bytes.Buffer
	err := run([]string{"tasks", "project", "7", "wrk", "--note", "regroup", "--url", h.URL, "--token", "tok"}, &out, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	req := (*seen)[0]
	if req.Method != "POST" || req.Path != "/v1/tasks/7/project" || req.Auth != "Bearer tok" {
		t.Fatalf("request=%+v", req)
	}
	if req.Body["project"] != "wrk" || req.Body["note"] != "regroup" {
		t.Fatalf("body=%v", req.Body)
	}
	if _, present := req.Body["by"]; present {
		t.Fatalf("client must not send by: %v", req.Body)
	}
	if !strings.Contains(out.String(), `"project":"wrk"`) {
		t.Fatalf("out=%s", out.String())
	}
}

// tasks projects lists the server vocabulary; tasks projects add extends it.
func TestTasksProjectsVocabulary(t *testing.T) {
	h, seen := projectServer(t, 200)
	var out bytes.Buffer
	if err := run([]string{"tasks", "projects", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].Method != "GET" || (*seen)[0].Path != "/v1/tasks/projects" {
		t.Fatalf("list request=%+v", (*seen)[0])
	}
	if !strings.Contains(out.String(), `"handoffkeep"`) {
		t.Fatalf("out=%s", out.String())
	}
	out.Reset()
	if err := run([]string{"tasks", "projects", "add", "fleet-ops", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if (*seen)[1].Method != "POST" || (*seen)[1].Path != "/v1/tasks/projects" || (*seen)[1].Body["name"] != "fleet-ops" {
		t.Fatalf("add request=%+v", (*seen)[1])
	}
}

// A pre-project server has no /v1/tasks/<id>/project route: the CLI must
// name that as unsupported, not surface a bare 404.
func TestTasksProjectOldServerRefusal(t *testing.T) {
	h, _ := projectServer(t, http.StatusNotFound)
	// The canned handler still writes the project payload; stand up a plain
	// 404 server so the response carries no project echo either.
	_ = h
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(plain.Close)
	var out bytes.Buffer
	err := run([]string{"tasks", "project", "7", "wrk", "--url", plain.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "task_project_unsupported") {
		t.Fatalf("old-server project relabel err=%v, want task_project_unsupported", err)
	}
}

// A pre-project server answers create with invalid_context for the unknown
// "project" field; the CLI must say the server is too old rather than
// echoing the generic code.
func TestTasksAddOldServerCompatError(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_context"}`))
	}))
	t.Cleanup(old.Close)
	var out bytes.Buffer
	err := run([]string{"tasks", "add", "--lane", "d", "--title", "x", "--project", "experiment", "--url", old.URL, "--token", "tok"}, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "create_project_rejected") {
		t.Fatalf("old-server add err=%v, want create_project_rejected", err)
	}
}
