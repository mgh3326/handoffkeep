package api

import (
	"net/http"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestProjectFrontierRoute(t *testing.T) {
	h, st := changesServer(t)
	project := "frontier-api"
	if _, err := st.AddTaskProject(t.Context(), project, "tester"); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/" + project + "/frontier"
	status, body := changesCall(t, h, http.MethodGet, path, "peer-token", nil)
	if status != http.StatusOK {
		t.Fatalf("%d %v", status, body)
	}
	keys := make([]string, 0, len(body))
	for k := range body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"changed_since", "changes", "counts", "gates", "generated_at", "project", "since", "truncated"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys %v", keys)
	}
	for _, query := range []string{"?since=garbage", "?since=" + time.Now().UTC().Add(-31*24*time.Hour).Format(time.RFC3339)} {
		status, body = changesCall(t, h, http.MethodGet, path+query, "peer-token", nil)
		if status != http.StatusBadRequest || !reflect.DeepEqual(body, map[string]any{"error": "invalid_context"}) {
			t.Fatalf("%s: %d %v", query, status, body)
		}
	}
	status, body = changesCall(t, h, http.MethodGet, "/v1/projects/absent-frontier/frontier", "peer-token", nil)
	if status != http.StatusNotFound || body["error"] != "project_not_found" {
		t.Fatalf("%d %v", status, body)
	}
}
