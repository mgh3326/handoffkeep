package api

import (
	"fmt"
	"net/http"
	"testing"
)

func TestModelRoutes(t *testing.T) {
	h, st := relaneAPIServer(t)
	task := relaneAPITask(t, st, relaneLane(t, "model"), "model")
	request := func(method, path string, body any, want int) map[string]any {
		t.Helper()
		code, out := projectRequest(t, h.URL, method, path, "tok", body)
		if code != want {
			t.Fatalf("%s %s got=%d body=%v want=%d", method, path, code, out, want)
		}
		return out
	}
	project := "/v1/tasks/projects/experiment/metadata"
	request(http.MethodGet, project, nil, http.StatusOK)
	request(http.MethodGet, "/v1/tasks/projects/missing/metadata", nil, http.StatusNotFound)
	request(http.MethodPut, project, map[string]any{"status": "active"}, http.StatusOK)
	request(http.MethodPut, "/v1/tasks/projects/missing/metadata", map[string]any{"status": "active"}, http.StatusNotFound)
	request(http.MethodPut, project, map[string]any{"status": 123}, http.StatusBadRequest)
	request(http.MethodPatch, project, map[string]any{"status": "paused"}, http.StatusOK)
	request(http.MethodPatch, "/v1/tasks/projects/missing/metadata", map[string]any{"status": "paused"}, http.StatusNotFound)
	request(http.MethodPatch, project, map[string]any{"status": 123}, http.StatusBadRequest)
	m := request(http.MethodPost, "/v1/milestones", map[string]any{"project": "experiment", "name": "first"}, http.StatusOK)
	mid := int64(m["id"].(float64))
	request(http.MethodPost, "/v1/milestones", map[string]any{"project": "experiment"}, http.StatusBadRequest)
	mpath := fmt.Sprintf("/v1/milestones/%d", mid)
	request(http.MethodPut, mpath, map[string]any{"project": "experiment", "name": "changed"}, http.StatusOK)
	request(http.MethodPut, "/v1/milestones/999999", map[string]any{"project": "experiment", "name": "changed"}, http.StatusNotFound)
	request(http.MethodPut, mpath, map[string]any{"project": "experiment"}, http.StatusBadRequest)
	l := request(http.MethodPost, "/v1/labels", map[string]any{"name": "first", "edit_level": "anyone"}, http.StatusOK)
	lid := int64(l["id"].(float64))
	request(http.MethodGet, "/v1/labels", nil, http.StatusOK)
	request(http.MethodPost, "/v1/labels", map[string]any{"name": "bad", "edit_level": "wrong"}, http.StatusBadRequest)
	lpath := fmt.Sprintf("/v1/labels/%d", lid)
	request(http.MethodPut, lpath, map[string]any{"name": "changed", "edit_level": "desk"}, http.StatusOK)
	request(http.MethodPut, "/v1/labels/999999", map[string]any{"name": "changed", "edit_level": "desk"}, http.StatusNotFound)
	request(http.MethodPut, lpath, map[string]any{"name": "changed", "edit_level": "bad"}, http.StatusBadRequest)
	taskpath := fmt.Sprintf("/v1/tasks/%d", task.ID)
	for _, tc := range []struct {
		suffix    string
		good, bad any
	}{
		{"/milestone", map[string]any{"milestone_id": mid}, map[string]any{"milestone_id": -1}},
		{"/labels", map[string]any{"label_id": lid, "attach": true}, map[string]any{"label_id": lid}},
		{"/deadlines", map[string]any{"due_at": "2026-01-01T00:00:00Z"}, map[string]any{"due_at": "invalid"}},
		{"/due", map[string]any{"due_at": "2026-01-01T00:00:00Z"}, map[string]any{"due_at": "invalid"}},
		{"/subscribers", map[string]any{"subscriber": "test", "source": "test", "active": true}, map[string]any{"subscriber": "", "source": "test", "active": true}},
	} {
		request(http.MethodPost, taskpath+tc.suffix, tc.good, http.StatusOK)
		request(http.MethodPost, "/v1/tasks/999999"+tc.suffix, tc.good, http.StatusNotFound)
		request(http.MethodPost, taskpath+tc.suffix, tc.bad, http.StatusBadRequest)
	}
	request(http.MethodPost, lpath+"/retire", nil, http.StatusOK)
	request(http.MethodPost, "/v1/labels/999999/retire", nil, http.StatusNotFound)
	request(http.MethodPost, "/v1/labels/bad/retire", nil, http.StatusBadRequest)
}
