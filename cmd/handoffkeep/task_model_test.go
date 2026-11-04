package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTaskModelCommands(t *testing.T) {
	var method, path string
	var body map[string]any
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		body = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if path == "/v1/labels" && method == "GET" {
			_, _ = w.Write([]byte(`{"labels":[]}`))
		} else {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer h.Close()
	cases := []struct {
		args  []string
		path  string
		key   string
		value any
	}{
		{[]string{"milestone", "7", "--to", "2"}, "/v1/tasks/7/milestone", "milestone_id", float64(2)},
		{[]string{"milestone", "7", "--clear"}, "/v1/tasks/7/milestone", "milestone_id", nil},
		{[]string{"label", "7", "--to", "2"}, "/v1/tasks/7/labels", "attach", true},
		{[]string{"label", "7", "--to", "2", "--remove"}, "/v1/tasks/7/labels", "attach", false},
		{[]string{"due", "7", "2026-01-02T00:00:00Z"}, "/v1/tasks/7/due", "due_at", "2026-01-02T00:00:00Z"},
		{[]string{"due", "7", "--clear"}, "/v1/tasks/7/due", "due_at", nil},
		{[]string{"subscribe", "7", "test"}, "/v1/tasks/7/subscribers", "active", true},
		{[]string{"unsubscribe", "7", "test"}, "/v1/tasks/7/subscribers", "active", false},
		{[]string{"milestone", "create", "m", "--project", "experiment"}, "/v1/milestones", "name", "m"},
		{[]string{"label", "create", "l"}, "/v1/labels", "edit_level", "anyone"},
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s_%d", tc.args[0], i), func(t *testing.T) {
			method, path = "", ""
			var out bytes.Buffer
			args := append([]string{"tasks"}, tc.args...)
			args = append(args, "--url", h.URL, "--token", "tok")
			if err := run(args, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			if method != "POST" || path != tc.path || body[tc.key] != tc.value {
				t.Fatalf("got %s %s %v", method, path, body)
			}
		})
	}
}

func TestMilestoneRenamePreservesOtherFields(t *testing.T) {
	saved := map[string]any{"name": "old", "description": "concurrent edit", "target_at": "2026-02-01T00:00:00Z", "status": "active", "sort_order": float64(3)}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPatch || r.URL.Path != "/v1/milestones/5" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var patch map[string]any
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			t.Error(err)
		}
		for _, key := range []string{"name", "description", "target_at", "status", "sort_order"} {
			if v, ok := patch[key]; ok {
				saved[key] = v
			}
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer h.Close()
	var out bytes.Buffer
	if err := run([]string{"tasks", "milestone", "update", "5", "new", "--project", "experiment", "--url", h.URL, "--token", "tok"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if saved["name"] != "new" || saved["description"] != "concurrent edit" || saved["target_at"] != "2026-02-01T00:00:00Z" || saved["status"] != "active" || saved["sort_order"] != float64(3) {
		t.Fatalf("saved=%v", saved)
	}
}

func TestMilestoneUpdateEmptyTargetSendsNull(t *testing.T) {
	var patch map[string]json.RawMessage
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer h.Close()
	if err := run([]string{"tasks", "milestone", "update", "5", "new", "--project", "experiment", "--target", "", "--url", h.URL, "--token", "tok"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if string(patch["set_target"]) != "true" || string(patch["target_at"]) != "null" {
		t.Fatalf("patch=%v", patch)
	}
}
