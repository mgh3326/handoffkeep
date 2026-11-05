package api

import (
	"net/http"
	"testing"
)

const originIssueUUID = "3f2b8c1e-9a4d-4e6f-8b7a-1c2d3e4f5a6b"

func TestTaskOriginsAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	lane := relaneLane(t, "origins-api")
	a := relaneAPITask(t, st, lane, "origin a")
	b := relaneAPITask(t, st, lane, "origin b")
	pathA := "/v1/tasks/" + itoa64(a.ID) + "/origins"
	body := map[string]any{"provider": "linear", "namespace": "watcha", "external_id": originIssueUUID, "display_ref": "MGH-151", "url": "https://linear.app/watcha/issue/MGH-151", "title": "an issue"}

	code, out := projectRequest(t, h.URL, http.MethodPost, pathA, "tok", body)
	origin, _ := out["origin"].(map[string]any)
	if code != http.StatusCreated || out["created"] != true || origin["display_ref"] != "MGH-151" || origin["external_id"] != originIssueUUID {
		t.Fatalf("first POST code=%d body=%v", code, out)
	}
	// The issue moved from MGH to ROB: same UUID, new number, no second row.
	body["display_ref"] = "ROB-1514"
	code, out = projectRequest(t, h.URL, http.MethodPost, pathA, "tok", body)
	origin, _ = out["origin"].(map[string]any)
	if code != http.StatusOK || out["created"] != false || origin["display_ref"] != "ROB-1514" {
		t.Fatalf("second POST code=%d body=%v", code, out)
	}

	code, out = projectRequest(t, h.URL, http.MethodGet, pathA, "tok", nil)
	listed, _ := out["origins"].([]any)
	if code != http.StatusOK || len(listed) != 1 {
		t.Fatalf("GET list code=%d body=%v", code, out)
	}

	// The same UUID on a second task; the lookup returns both, each with its task.
	if code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/"+itoa64(b.ID)+"/origins", "tok", body); code != http.StatusCreated {
		t.Fatalf("POST to task b code=%d body=%v", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/task-origins?provider=linear&external_id="+originIssueUUID, "tok", nil)
	found, _ := out["origins"].([]any)
	if code != http.StatusOK || len(found) != 2 {
		t.Fatalf("find code=%d body=%v", code, out)
	}
	for i, want := range []int64{a.ID, b.ID} {
		item, _ := found[i].(map[string]any)
		if got, _ := item["task_id"].(float64); int64(got) != want {
			t.Errorf("find item %d task_id=%v, want %d", i, item["task_id"], want)
		}
	}

	// Refusals.
	for name, tc := range map[string]struct {
		method, path string
		body         any
		status       int
		errCode      string
	}{
		"bad task id on POST":      {http.MethodPost, "/v1/tasks/abc/origins", body, 400, "invalid_context"},
		"zero task id on POST":     {http.MethodPost, "/v1/tasks/0/origins", body, 400, "invalid_context"},
		"bad task id on GET":       {http.MethodGet, "/v1/tasks/abc/origins", nil, 400, "invalid_context"},
		"unknown task on POST":     {http.MethodPost, "/v1/tasks/999999999/origins", body, 404, "not_found"},
		"unknown task on GET":      {http.MethodGet, "/v1/tasks/999999999/origins", nil, 404, "not_found"},
		"unknown body field":       {http.MethodPost, pathA, map[string]any{"provider": "linear", "namespace": "watcha", "external_id": originIssueUUID, "colour": "red"}, 400, ""},
		"display number as id":     {http.MethodPost, pathA, map[string]any{"provider": "linear", "namespace": "watcha", "external_id": "MGH-151"}, 400, "invalid_context"},
		"unknown provider":         {http.MethodPost, pathA, map[string]any{"provider": "jira", "namespace": "watcha", "external_id": originIssueUUID}, 400, "invalid_context"},
		"find without provider":    {http.MethodGet, "/v1/task-origins?external_id=" + originIssueUUID, nil, 400, "invalid_context"},
		"find without external_id": {http.MethodGet, "/v1/task-origins?provider=linear", nil, 400, "invalid_context"},
		"find with a bad UUID":     {http.MethodGet, "/v1/task-origins?provider=linear&external_id=MGH-151", nil, 400, "invalid_context"},
		"find with a bad provider": {http.MethodGet, "/v1/task-origins?provider=jira&external_id=x", nil, 400, "invalid_context"},
		"find with a bad limit":    {http.MethodGet, "/v1/task-origins?provider=linear&external_id=" + originIssueUUID + "&limit=0", nil, 400, "invalid_context"},
	} {
		code, out := projectRequest(t, h.URL, tc.method, tc.path, "tok", tc.body)
		if code != tc.status || (tc.errCode != "" && out["error"] != tc.errCode) {
			t.Errorf("%s: code=%d body=%v, want %d %s", name, code, out, tc.status, tc.errCode)
		}
	}
	if code, _ := projectRequest(t, h.URL, http.MethodGet, pathA, "", nil); code != http.StatusUnauthorized {
		t.Errorf("GET without a token code=%d, want 401", code)
	}
}

func TestDocumentsTaskAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	task := relaneAPITask(t, st, relaneLane(t, "docs-api"), "doc task")
	other := relaneAPITask(t, st, relaneLane(t, "docs-api"), "other task")
	put := func(key string, body map[string]any) (int, map[string]any) {
		return projectRequest(t, h.URL, http.MethodPut, "/v1/documents/"+key, "tok", body)
	}
	if code, out := put("api-doc/old", map[string]any{"kind": "note", "body": "old shape"}); code != 200 || out["changed"] != true {
		t.Fatalf("old-shape PUT code=%d body=%v", code, out)
	}
	if code, out := put("api-doc/mine", map[string]any{"kind": "design", "body": "b", "task_id": task.ID, "origin_class": "authored"}); code != 200 || out["changed"] != true {
		t.Fatalf("PUT with task_id code=%d body=%v", code, out)
	}
	if code, out := put("api-doc/other", map[string]any{"kind": "note", "body": "b", "task_id": other.ID}); code != 200 {
		t.Fatalf("PUT other code=%d body=%v", code, out)
	}
	if code, out := put("api-doc/mine", map[string]any{"kind": "design", "body": "b"}); code != 200 || out["changed"] != false {
		t.Fatalf("re-PUT without the new fields code=%d body=%v", code, out)
	}
	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/documents/api-doc/mine", "tok", nil)
	if code != 200 || out["origin_class"] != "authored" || out["task_id"] != float64(task.ID) {
		t.Fatalf("GET code=%d body=%v", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/documents?prefix=api-doc/&task="+itoa64(task.ID), "tok", nil)
	docs, _ := out["documents"].([]any)
	if code != 200 || len(docs) != 1 {
		t.Fatalf("list by task code=%d body=%v", code, out)
	}
	for _, q := range []string{"task=abc", "task=0", "task=-3"} {
		if code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/documents?"+q, "tok", nil); code != 400 || out["error"] != "invalid_context" {
			t.Errorf("%s: code=%d body=%v", q, code, out)
		}
	}
	if code, out = put("api-doc/bad", map[string]any{"kind": "note", "body": "b", "origin_class": "robot"}); code != 400 {
		t.Errorf("unknown origin_class code=%d body=%v", code, out)
	}
	if code, out = put("api-doc/bad", map[string]any{"kind": "note", "body": "b", "task_id": 999999999}); code != 404 || out["error"] != "not_found" {
		t.Errorf("missing task code=%d body=%v", code, out)
	}
}
