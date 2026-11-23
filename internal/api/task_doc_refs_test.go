package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestTaskDocRefRoutesAPI covers /v1/tasks/{id}/doc-refs: any authenticated
// client may cite a stored document, a repeat answers 200 with changed=false,
// a missing document is 400 doc_not_found, and removal names the document in
// the doc query parameter.
func TestTaskDocRefRoutesAPI(t *testing.T) {
	h, st := orderAPIServer(t)
	ctx := context.Background()
	task := relaneAPITask(t, st, relaneLane(t, "doc-refs-api"), "cite a schema doc")
	key := fmt.Sprintf("tbls/doc-refs-api-%d/doc", task.ID)
	if _, _, err := st.PutDocument(ctx, store.Document{Key: key, Kind: "design", Body: "# schema", CreatedBy: "tester"}); err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/v1/tasks/%d/doc-refs", task.ID)
	body := map[string]any{"doc": key}

	if code, out := projectRequest(t, h.URL, http.MethodPost, path, "", body); code != http.StatusUnauthorized {
		t.Fatalf("no token: code=%d body=%v, want 401", code, out)
	}
	code, out := projectRequest(t, h.URL, http.MethodPost, path, "lane-tok", body)
	if code != http.StatusCreated || out["changed"] != true {
		t.Fatalf("add: code=%d body=%v, want 201 changed", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, path, "lane-tok", body)
	if code != http.StatusOK || out["changed"] != false {
		t.Fatalf("repeat add: code=%d body=%v, want 200 unchanged", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, path, "lane-tok", map[string]any{"doc": key + "-missing"})
	if code != http.StatusBadRequest || out["error"] != "doc_not_found" {
		t.Fatalf("missing document: code=%d body=%v, want 400 doc_not_found", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodGet, path, "lane-tok", nil)
	refs, _ := out["doc_refs"].([]any)
	if code != http.StatusOK || len(refs) != 1 {
		t.Fatalf("list: code=%d body=%v, want one reference", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodDelete, path+"?doc="+url.QueryEscape(key), "lane-tok", nil)
	if code != http.StatusOK || out["changed"] != true {
		t.Fatalf("remove: code=%d body=%v, want 200 changed", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodDelete, path, "lane-tok", nil)
	if code != http.StatusBadRequest || out["error"] != "invalid_context" {
		t.Fatalf("remove without doc: code=%d body=%v, want 400 invalid_context", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/doc-refs", task.ID+1_000_000), "lane-tok", body)
	if code != http.StatusNotFound || out["error"] != "not_found" {
		t.Fatalf("unknown task: code=%d body=%v, want 404 not_found", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks/nope/doc-refs", "lane-tok", nil)
	if code != http.StatusBadRequest || out["error"] != "invalid_context" {
		t.Fatalf("malformed id: code=%d body=%v, want 400 invalid_context", code, out)
	}
}
