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

// areaServer records requests (method, path with query, decoded body) and
// answers each route with the minimal body the client expects.
func areaServer(t *testing.T) (*httptest.Server, *[]relaneRequest) {
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
		switch {
		case r.URL.Path == "/v1/task-areas":
			resp, _ := json.Marshal(map[string]any{"areas": store.TaskAreas[:]})
			_, _ = w.Write(resp)
		case strings.HasSuffix(r.URL.Path, "/area"):
			// Echo what the client sent so the client's applied-check passes.
			resp, _ := json.Marshal(map[string]any{"id": 7, "area": x.Body["area"]})
			_, _ = w.Write(resp)
		case r.URL.Path == "/v1/tasks" && r.Method == http.MethodPost:
			resp, _ := json.Marshal(map[string]any{"id": 9, "project": x.Body["project"], "area": x.Body["area"]})
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(resp)
		case r.URL.Path == "/v1/tasks":
			resp := map[string]any{"tasks": []any{}}
			if r.URL.Query().Has("area") {
				resp["area"] = r.URL.Query().Get("area")
			}
			b, _ := json.Marshal(resp)
			_, _ = w.Write(b)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(h.Close)
	return h, &seen
}

func areaRun(t *testing.T, h *httptest.Server, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(append(args, "--url", h.URL, "--token", "tok"), &out, io.Discard)
	return out.String(), err
}

// resolveSetArea is the store-free argument contract of tasks set-area.
func TestResolveSetArea(t *testing.T) {
	str := func(v string) *string { return &v }
	for _, tc := range []struct {
		name       string
		positional []string
		clear      bool
		wantID     int64
		wantArea   *string
		wantErr    string
	}{
		{"set", []string{"7", "docs"}, false, 7, str("docs"), ""},
		{"clear", []string{"7"}, true, 7, nil, ""},
		{"no args", nil, false, 0, nil, "usage"},
		{"id only without clear", []string{"7"}, false, 0, nil, "usage"},
		{"clear with a value", []string{"7", "docs"}, true, 0, nil, "--clear"},
		{"too many", []string{"7", "docs", "ops"}, false, 0, nil, "usage"},
		{"zero id", []string{"0", "docs"}, false, 0, nil, "positive"},
		{"negative id", []string{"-3", "docs"}, false, 0, nil, "positive"},
		{"non-numeric id", []string{"x", "docs"}, false, 0, nil, "positive"},
		{"unknown area", []string{"7", "Blender"}, false, 0, nil, "blender, unity"},
		{"padded area", []string{"7", " docs"}, false, 0, nil, "blender, unity"},
		{"empty area", []string{"7", ""}, false, 0, nil, "blender, unity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, area, err := resolveSetArea(tc.positional, tc.clear)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || id != tc.wantID {
				t.Fatalf("id=%d err=%v", id, err)
			}
			if (area == nil) != (tc.wantArea == nil) || (area != nil && *area != *tc.wantArea) {
				t.Fatalf("area=%v, want %v", area, tc.wantArea)
			}
		})
	}
}

func TestTasksSetAreaSendsBody(t *testing.T) {
	h, seen := areaServer(t)
	// Flags may come before or after the positionals.
	for _, args := range [][]string{
		{"tasks", "set-area", "7", "docs", "--note", "readme only"},
		{"tasks", "set-area", "--note", "readme only", "7", "docs"},
	} {
		*seen = nil
		if _, err := areaRun(t, h, args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		req := (*seen)[0]
		if req.Method != "POST" || req.Path != "/v1/tasks/7/area" || req.Body["area"] != "docs" || req.Body["note"] != "readme only" || req.Auth != "Bearer tok" {
			t.Fatalf("%v: request=%+v", args, req)
		}
	}
}

func TestTasksSetAreaClearSendsNull(t *testing.T) {
	h, seen := areaServer(t)
	if _, err := areaRun(t, h, "tasks", "set-area", "7", "--clear", "--note", "wrong tag"); err != nil {
		t.Fatal(err)
	}
	req := (*seen)[0]
	v, present := req.Body["area"]
	if req.Path != "/v1/tasks/7/area" || !present || v != nil || req.Body["note"] != "wrong tag" {
		t.Fatalf("clear request=%+v (area key must be present and null)", req)
	}
}

// Bad arguments are refused locally: no request reaches the server.
func TestTasksSetAreaLocalRefusals(t *testing.T) {
	h, seen := areaServer(t)
	for _, args := range [][]string{
		{"tasks", "set-area"},
		{"tasks", "set-area", "7"},
		{"tasks", "set-area", "7", "docs", "--clear"},
		{"tasks", "set-area", "7", "Blender"},
		{"tasks", "set-area", "0", "docs"},
		{"tasks", "set-area", "7", "docs", "ops"},
	} {
		if _, err := areaRun(t, h, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("refused commands reached the server: %+v", *seen)
	}
}

func TestTasksAreasListsVocabulary(t *testing.T) {
	h, seen := areaServer(t)
	out, err := areaRun(t, h, "tasks", "areas")
	if err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].Method != "GET" || (*seen)[0].Path != "/v1/task-areas" {
		t.Fatalf("request=%+v", (*seen)[0])
	}
	var got struct {
		Areas []string `json:"areas"`
	}
	if err = json.Unmarshal([]byte(out), &got); err != nil || strings.Join(got.Areas, ",") != strings.Join(store.TaskAreas[:], ",") {
		t.Fatalf("areas output=%q err=%v", out, err)
	}
	if _, err = areaRun(t, h, "tasks", "areas", "extra"); err == nil {
		t.Fatal("tasks areas accepted an argument")
	}
}

func TestTasksAddAreaFlag(t *testing.T) {
	h, seen := areaServer(t)
	if _, err := areaRun(t, h, "tasks", "add", "--lane", "d", "--title", "x", "--project", "experiment", "--area", "web-qa"); err != nil {
		t.Fatal(err)
	}
	if got := (*seen)[0].Body["area"]; got != "web-qa" {
		t.Fatalf("create body area=%v", got)
	}
	// Without --area the create body carries no area key at all, so a new
	// CLI still works against a server whose decoder predates the field.
	*seen = nil
	if _, err := areaRun(t, h, "tasks", "add", "--lane", "d", "--title", "x", "--project", "experiment"); err != nil {
		t.Fatal(err)
	}
	if _, present := (*seen)[0].Body["area"]; present {
		t.Fatalf("create without --area sent an area key: %+v", (*seen)[0].Body)
	}
	// An out-of-vocabulary or empty --area is refused locally.
	*seen = nil
	for _, bad := range []string{"Blender", "", "go", " docs"} {
		_, err := areaRun(t, h, "tasks", "add", "--lane", "d", "--title", "x", "--project", "experiment", "--area", bad)
		if err == nil || !strings.Contains(err.Error(), "blender, unity") {
			t.Errorf("--area %q err=%v, want a refusal naming the allowed set", bad, err)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("refused adds reached the server: %+v", *seen)
	}
}

func TestTasksListAreaFilter(t *testing.T) {
	h, seen := areaServer(t)
	if _, err := areaRun(t, h, "tasks", "list", "--area", "docs"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*seen)[0].Path, "area=docs") {
		t.Fatalf("list path=%q", (*seen)[0].Path)
	}
	// An explicit empty value selects tasks with no area.
	if _, err := areaRun(t, h, "tasks", "list", "--area", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*seen)[1].Path, "area=") || strings.Contains((*seen)[1].Path, "area=docs") {
		t.Fatalf("no-area list path=%q", (*seen)[1].Path)
	}
	// No --area, no area parameter.
	if _, err := areaRun(t, h, "tasks", "list"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*seen)[2].Path, "area") {
		t.Fatalf("unfiltered list path=%q", (*seen)[2].Path)
	}
	if _, err := areaRun(t, h, "tasks", "list", "--area", "Blender"); err == nil {
		t.Fatal("unknown --area filter accepted")
	}
}

// A server that predates the area filter drops the parameter and answers
// unfiltered, with no echo; the client must not print that as a result.
func TestTasksListAreaOldServerRefusal(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tasks":[]}`))
	}))
	t.Cleanup(old.Close)
	_, err := areaRun(t, old, "tasks", "list", "--area", "docs")
	if err == nil || !strings.Contains(err.Error(), "list_area_filter_ignored") {
		t.Fatalf("old-server err=%v, want list_area_filter_ignored", err)
	}
}

// A server that predates the field answers a set-area with 404/405; a server
// that accepts the body but drops the field answers 200 without the area.
func TestTasksSetAreaOldServerRefusals(t *testing.T) {
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(gone.Close)
	if _, err := areaRun(t, gone, "tasks", "set-area", "7", "docs"); err == nil || !strings.Contains(err.Error(), "task_area_unsupported") {
		t.Fatalf("404 err=%v, want task_area_unsupported", err)
	}
	dropped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7}`))
	}))
	t.Cleanup(dropped.Close)
	if _, err := areaRun(t, dropped, "tasks", "set-area", "7", "docs"); err == nil || !strings.Contains(err.Error(), "area_not_applied") {
		t.Fatalf("dropped-field err=%v, want area_not_applied", err)
	}
}
