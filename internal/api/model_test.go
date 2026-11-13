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
	request(http.MethodPatch, project, map[string]any{"start_at": "2026-01-01T00:00:00Z"}, http.StatusBadRequest)
	m := request(http.MethodPost, "/v1/milestones", map[string]any{"project": "experiment", "name": "first"}, http.StatusOK)
	mid := int64(m["id"].(float64))
	request(http.MethodPost, "/v1/milestones", map[string]any{"project": "experiment"}, http.StatusBadRequest)
	mpath := fmt.Sprintf("/v1/milestones/%d", mid)
	request(http.MethodGet, mpath, nil, http.StatusOK)
	request(http.MethodGet, "/v1/milestones/999999", nil, http.StatusNotFound)
	request(http.MethodGet, "/v1/milestones/bad", nil, http.StatusBadRequest)
	request(http.MethodPatch, mpath, map[string]any{"name": "patched"}, http.StatusOK)
	request(http.MethodPatch, "/v1/milestones/999999", map[string]any{"name": "patched"}, http.StatusNotFound)
	request(http.MethodPatch, mpath, map[string]any{"name": ""}, http.StatusBadRequest)

	request(http.MethodPut, mpath, map[string]any{"project": "experiment", "name": "changed"}, http.StatusOK)
	request(http.MethodPut, "/v1/milestones/999999", map[string]any{"project": "experiment", "name": "changed"}, http.StatusNotFound)
	request(http.MethodPut, mpath, map[string]any{"project": "experiment"}, http.StatusBadRequest)
	taskpath := fmt.Sprintf("/v1/tasks/%d", task.ID)
	request(http.MethodPost, taskpath+"/milestone", map[string]any{"milestone_id": mid}, http.StatusOK)
	request(http.MethodPost, "/v1/tasks/999999/milestone", map[string]any{"milestone_id": mid}, http.StatusNotFound)
	request(http.MethodPost, taskpath+"/milestone", map[string]any{"milestone_id": -1}, http.StatusBadRequest)
}
