package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// returns before the store is consulted, so no database is needed.
func paramsServer(t *testing.T) *httptest.Server {
	t.Helper()
	h := httptest.NewServer(Server{Tokens: Tokens{"tester": "tok"}}.Handler())
	t.Cleanup(h.Close)
	return h
}

// TestTasksListBadParamsNameTheParam proves every refusal on GET /v1/tasks
// keeps the code invalid_context and names the parameter plus what it
// accepts (hk 6479).
func TestTasksListBadParamsNameTheParam(t *testing.T) {
	h := paramsServer(t)
	for _, tc := range []struct {
		name  string
		path  string
		param string
	}{
		{"limit zero", "/v1/tasks?limit=0", "limit"},
		{"limit over max", "/v1/tasks?limit=1001", "limit"},
		{"limit non-numeric", "/v1/tasks?limit=x", "limit"},
		{"unknown state", "/v1/tasks?state=nope", "state"},
		{"bad stalled", "/v1/tasks?stalled=maybe", "stalled"},
		{"change mixed with lane", "/v1/tasks?change=3&lane=d", "change"},
		{"change zero", "/v1/tasks?change=0", "change"},
		{"change non-numeric", "/v1/tasks?change=x", "change"},
		{"bad lane", "/v1/tasks?lane=BAD!", "lane"},
		{"bad parent lane", "/v1/tasks?parent_lane=BAD!", "parent_lane"},
		{"bad project", "/v1/tasks?project=BAD!", "project"},
		{"bad after id", "/v1/tasks?after_id=x", "after_id"},
		{"negative after id", "/v1/tasks?after_id=-1", "after_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := projectRequest(t, h.URL, http.MethodGet, tc.path, "tok", nil)
			if code != http.StatusBadRequest || out["error"] != "invalid_context" {
				t.Fatalf("%s: code=%d body=%v", tc.name, code, out)
			}
			if out["param"] != tc.param {
				t.Fatalf("%s: param=%v, want %q", tc.name, out["param"], tc.param)
			}
			if out["allowed"] == nil {
				t.Fatalf("%s: allowed missing in %v", tc.name, out)
			}
		})
	}

	// The limit range reads exactly, and the state vocabulary names a state.
	_, out := projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?limit=x", "tok", nil)
	if out["allowed"] != "1..1000" {
		t.Fatalf("limit allowed=%v, want 1..1000", out["allowed"])
	}
	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?state=nope", "tok", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("state: code=%d", code)
	}
	allowed, _ := out["allowed"].([]any)
	found := false
	for _, x := range allowed {
		if x == "backlog" {
			found = true
		}
	}
	if !found {
		t.Fatalf("state allowed=%v, want a list holding backlog", out["allowed"])
	}
	_, out = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?stalled=maybe", "tok", nil)
	if out["allowed"] != "true,false" {
		t.Fatalf("stalled allowed=%v, want true,false", out["allowed"])
	}
	// An unknown area keeps its code and names the parameter too.
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?area=Blender", "tok", nil)
	if code != http.StatusBadRequest || out["error"] != "invalid_request" || out["param"] != "area" || out["allowed"] == nil {
		t.Fatalf("area: code=%d body=%v", code, out)
	}
}

// TestTasksExportBadParamsNameTheParam proves the same naming on
// GET /v1/tasks/export, keeping the export code invalid_export_query.
func TestTasksExportBadParamsNameTheParam(t *testing.T) {
	h := paramsServer(t)
	for _, tc := range []struct {
		name  string
		path  string
		param string
	}{
		{"limit zero", "/v1/tasks/export?limit=0", "limit"},
		{"limit over max", "/v1/tasks/export?limit=10001", "limit"},
		{"limit non-numeric", "/v1/tasks/export?limit=x", "limit"},
		{"unknown state", "/v1/tasks/export?state=nope", "state"},
		{"bad lane", "/v1/tasks/export?lane=BAD!", "lane"},
		{"bad project", "/v1/tasks/export?project=BAD!", "project"},
		{"bad parent lane", "/v1/tasks/export?parent_lane=BAD!", "parent_lane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := projectRequest(t, h.URL, http.MethodGet, tc.path, "tok", nil)
			if code != http.StatusBadRequest || out["error"] != "invalid_export_query" {
				t.Fatalf("%s: code=%d body=%v", tc.name, code, out)
			}
			if out["param"] != tc.param {
				t.Fatalf("%s: param=%v, want %q", tc.name, out["param"], tc.param)
			}
			if out["allowed"] == nil {
				t.Fatalf("%s: allowed missing in %v", tc.name, out)
			}
		})
	}
	_, out := projectRequest(t, h.URL, http.MethodGet, "/v1/tasks/export?limit=x", "tok", nil)
	if out["allowed"] != "1..10000" {
		t.Fatalf("export limit allowed=%v, want 1..10000", out["allowed"])
	}
}

// TestTasksSearchBadParamsNameTheParam proves GET /v1/tasks/search routes
// past /v1/tasks/{id} and names its bad parameters. It needs no database:
// every refusal returns before the search runs.
func TestTasksSearchBadParamsNameTheParam(t *testing.T) {
	h := paramsServer(t)
	for _, tc := range []struct{ path, param string }{
		{"/v1/tasks/search?q=", "q"},
		{"/v1/tasks/search?q=x&state=nope", "state"},
		{"/v1/tasks/search?q=x&limit=0", "limit"},
		{"/v1/tasks/search?q=x&limit=51", "limit"},
		{"/v1/tasks/search?q=x&limit=x", "limit"},
		{"/v1/tasks/search?q=x&lane=BAD!", "lane"},
		{"/v1/tasks/search?q=x&project=BAD!", "project"},
		{"/v1/tasks/search?q=x&open=maybe", "open"},
		{"/v1/tasks/search?q=x&open=true&state=backlog", "open"},
	} {
		t.Run(tc.param+tc.path, func(t *testing.T) {
			code, out := projectRequest(t, h.URL, http.MethodGet, tc.path, "tok", nil)
			if code != http.StatusBadRequest || out["error"] != "invalid_context" || out["param"] != tc.param || out["allowed"] == nil {
				t.Fatalf("%s: code=%d body=%v", tc.path, code, out)
			}
		})
	}
}

// TestTasksSearchAPI proves GET /v1/tasks/search finds words across states
// with the project, state and open filters, and names a bad parameter. It
// needs a database and runs in CI.
func TestTasksSearchAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	ctx := context.Background()
	lane := relaneLane(t, "search-api")
	other := lane + "-other"
	// mk files a task and moves it to toState: claim reaches developing,
	// then one more edge reaches reviewing.
	mk := func(lane, title, toState string, job int) {
		t.Helper()
		x := relaneAPITask(t, st, lane, title+" searchprobe")
		if toState == store.StateBacklog {
			return
		}
		if _, err := st.ClaimTask(ctx, x.ID, "search-api-test", fmt.Sprintf("job-searchprobe-%d", job), ""); err != nil {
			t.Fatal(err)
		}
		if toState == store.StateReviewing {
			if _, err := st.TransitionTaskWith(ctx, x.ID, store.TransitionInput{To: store.StateReviewing, By: "search-api-test"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk(lane, "alpha", store.StateBacklog, 1)
	mk(lane, "beta", store.StateDeveloping, 2)
	mk(other, "gamma", store.StateReviewing, 3)

	get := func(path string) (int, map[string]any) {
		t.Helper()
		return projectRequest(t, h.URL, http.MethodGet, path, "tok", nil)
	}
	tasksOf := func(body map[string]any) []any {
		t.Helper()
		tasks, _ := body["tasks"].([]any)
		return tasks
	}

	// Words find tasks in several states at once.
	code, body := get("/v1/tasks/search?q=searchprobe")
	if code != http.StatusOK || len(tasksOf(body)) != 3 {
		t.Fatalf("unfiltered: code=%d body=%v", code, body)
	}
	// The state, lane and open filters narrow.
	code, body = get("/v1/tasks/search?q=searchprobe&state=developing")
	if code != http.StatusOK || len(tasksOf(body)) != 1 {
		t.Fatalf("state filter: code=%d body=%v", code, body)
	}
	code, body = get("/v1/tasks/search?q=searchprobe&lane=" + other)
	if code != http.StatusOK || len(tasksOf(body)) != 1 {
		t.Fatalf("lane filter: code=%d body=%v", code, body)
	}
	code, body = get("/v1/tasks/search?q=searchprobe&open=true")
	if code != http.StatusOK || len(tasksOf(body)) != 3 {
		t.Fatalf("open filter: code=%d body=%v", code, body)
	}
	// An explicit empty project selects legacy rows with no project, and the
	// search stays a filtered one: every returned row has no project.
	code, body = get("/v1/tasks/search?q=searchprobe&project=")
	if code != http.StatusOK {
		t.Fatalf("empty project: code=%d body=%v", code, body)
	}
	for _, x := range tasksOf(body) {
		if p := x.(map[string]any)["project"]; p != nil {
			t.Fatalf("empty project returned a row with project %v", p)
		}
	}
	// An empty match is 200 with no rows.
	code, body = get("/v1/tasks/search?q=searchprobe&state=icebox")
	if code != http.StatusOK || len(tasksOf(body)) != 0 {
		t.Fatalf("empty: code=%d body=%v", code, body)
	}
	if body["truncated"] == nil {
		t.Fatalf("truncated missing in %v", body)
	}
	// Bad parameters name themselves.
	for _, tc := range []struct{ path, param string }{
		{"/v1/tasks/search?q=", "q"},
		{"/v1/tasks/search?q=x&state=nope", "state"},
		{"/v1/tasks/search?q=x&limit=0", "limit"},
		{"/v1/tasks/search?q=x&open=true&state=backlog", "open"},
	} {
		code, body := get(tc.path)
		if code != http.StatusBadRequest || body["error"] != "invalid_context" || body["param"] != tc.param || body["allowed"] == nil {
			t.Fatalf("%s: code=%d body=%v", tc.path, code, body)
		}
	}
}
