package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// GET /v1/yt/links/status (hk 4640): the read model over HTTP. Off by default,
// one task's view, an unknown task is 404 not_found, a malformed task is 400.
func TestYTLinksStatusRoute(t *testing.T) {
	_, dbURL := testdb.Schema(t, "yt_links_status_api")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"operator": "op-token", "peer": "peer-token"}}.Handler())
	t.Cleanup(h.Close)

	get := func(path string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, h.URL+path, nil)
		req.Header.Set("Authorization", "Bearer peer-token")
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	task, err := st.CreateTask(t.Context(), store.Task{Lane: "yt-links-status-api", Title: "links", Kind: "implement", CreatedBy: "t", Project: &apiTestProject})
	if err != nil {
		t.Fatal(err)
	}
	status, body := get("/v1/yt/links/status")
	if status != http.StatusOK || body["mode"] != "off" || body["open"] != false || body["task_id"] != float64(0) {
		t.Fatalf("all tasks = %d %v", status, body)
	}
	for _, key := range []string{"owned", "unowned", "held", "sweep"} {
		if body[key] == nil {
			t.Fatalf("all tasks: %q missing in %v", key, body)
		}
	}
	status, body = get("/v1/yt/links/status?task=" + strconv.FormatInt(task.ID, 10) + "&limit=5")
	if status != http.StatusOK || body["task_id"] != float64(task.ID) || body["limit"] != float64(5) {
		t.Fatalf("one task = %d %v", status, body)
	}
	if status, body := get("/v1/yt/links/status?task=987654321"); status != http.StatusNotFound || body["error"] != "not_found" {
		t.Fatalf("unknown task = %d %v", status, body)
	}
	if status, body := get("/v1/yt/links/status?task=abc"); status != http.StatusBadRequest || body["error"] != "invalid_context" {
		t.Fatalf("malformed task = %d %v", status, body)
	}
}
