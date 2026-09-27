package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// projectPost is the raw-JSON twin of relanePost: the project assertions read
// the error body verbatim, so the helper returns status and decoded map.
func projectRequest(t *testing.T, hServer string, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, hServer+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The required-project refusal is the old-CLI contract: a 400 carrying
// task_project_required and a reason that names --project.
func TestTasksCreateProjectGateAPI(t *testing.T) {
	h, _ := relaneAPIServer(t)
	for _, tc := range []struct {
		name  string
		body  map[string]any
		code  string
		names string
	}{
		{"absent field", map[string]any{"lane": "d", "title": "x", "kind": "implement"}, "task_project_required", "--project"},
		{"null", map[string]any{"lane": "d", "title": "x", "kind": "implement", "project": nil}, "task_project_required", "--project"},
		{"empty", map[string]any{"lane": "d", "title": "x", "kind": "implement", "project": ""}, "task_project_required", "--project"},
		{"unknown name", map[string]any{"lane": "d", "title": "x", "kind": "implement", "project": "not-a-project"}, "task_project_unknown", "tasks projects"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/tasks", "tok", tc.body)
			if code != 400 || out["error"] != tc.code {
				t.Fatalf("code=%d body=%v", code, out)
			}
			reason, _ := out["reason"].(string)
			if !strings.Contains(reason, tc.names) {
				t.Fatalf("reason %q must name %s", reason, tc.names)
			}
		})
	}
	// A known name creates the row and the field round-trips.
	code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/tasks", "tok", map[string]any{"lane": "d", "title": "x", "kind": "implement", "project": "experiment"})
	if code != 201 || out["project"] != "experiment" {
		t.Fatalf("create code=%d body=%v", code, out)
	}
}

// POST /v1/tasks/<id>/project reclassifies and records a kind='project'
// event — the operator-visible audit trail from #763.
func TestTaskProjectRouteAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	x := relaneAPITask(t, st, relaneLane(t, "api-proj"), "reclassify me")

	code, out := projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/project", x.ID), "tok", map[string]any{"project": "wrk", "note": "queue cleanup"})
	if code != 200 || out["project"] != "wrk" {
		t.Fatalf("project code=%d body=%v", code, out)
	}
	got, _, err := st.GetTask(context.Background(), x.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ev *store.TaskEvent
	for i := range got.Events {
		if got.Events[i].Kind == store.TaskEventProject {
			ev = &got.Events[i]
		}
	}
	if ev == nil || ev.From != "experiment" || ev.To != "wrk" || ev.By != "tester" || ev.Note != "queue cleanup" {
		t.Fatalf("project event=%+v", got.Events)
	}
	// Unknown name and unauthenticated both refuse.
	if code, out = projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/project", x.ID), "tok", map[string]any{"project": "nope"}); code != 400 || out["error"] != "task_project_unknown" {
		t.Fatalf("unknown project code=%d body=%v", code, out)
	}
	if code, _ = projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/project", x.ID), "", map[string]any{"project": "wrk"}); code != 401 {
		t.Fatalf("unauthenticated code=%d", code)
	}
}

// GET /v1/tasks/projects returns the seeded vocabulary; POST extends it and
// the extension immediately validates on create.
func TestTaskProjectsVocabularyAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/tasks/projects", "tok", nil)
	if code != 200 {
		t.Fatalf("list code=%d", code)
	}
	names, _ := out["projects"].([]any)
	if len(names) != 18 {
		t.Fatalf("seeded projects=%v", names)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/projects", "tok", map[string]any{"name": "new-vocab"})
	if code != 201 || out["created"] != true {
		t.Fatalf("add code=%d body=%v", code, out)
	}
	// Idempotent re-add reports created=false with 200.
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/projects", "tok", map[string]any{"name": "new-vocab"})
	if code != 200 || out["created"] != false {
		t.Fatalf("re-add code=%d body=%v", code, out)
	}
	x, err := st.CreateTask(context.Background(), store.Task{Lane: "d", Title: "grown vocab", Kind: "implement", CreatedBy: "t", Project: projectStr("new-vocab")})
	if err != nil || x.Project == nil || *x.Project != "new-vocab" {
		t.Fatalf("create with added project=%+v err=%v", x, err)
	}
	// An invalid name is a 400, never a 500.
	if code, _ = projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/projects", "tok", map[string]any{"name": "bad name!"}); code != 400 {
		t.Fatalf("bad name code=%d", code)
	}
}

func projectStr(v string) *string { return &v }

// ?project=<p> narrows the list; ?project= selects the legacy NULL bucket.
func TestTasksListProjectFilterAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	lane := relaneLane(t, "api-proj-filter")
	for _, tc := range []struct{ title, project string }{{"a", "wrk"}, {"b", "handoffkeep"}} {
		if _, err := st.CreateTask(context.Background(), store.Task{Lane: lane, Title: tc.title, Kind: "implement", CreatedBy: "t", Project: projectStr(tc.project)}); err != nil {
			t.Fatal(err)
		}
	}
	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?lane="+lane+"&project=wrk", "tok", nil)
	if code != 200 {
		t.Fatalf("filtered list code=%d", code)
	}
	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["project"] != "wrk" {
		t.Fatalf("filtered tasks=%v", tasks)
	}
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?lane="+lane+"&project=", "tok", nil)
	if code != 200 {
		t.Fatalf("legacy list code=%d", code)
	}
	if got, _ := out["tasks"].([]any); len(got) != 0 {
		t.Fatalf("legacy bucket on a clean lane=%v", got)
	}
	// The export scope carries the same filter.
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks/export?lane="+lane+"&project=handoffkeep", "tok", nil)
	if code != 200 {
		t.Fatalf("export code=%d", code)
	}
	scope, _ := out["scope"].(map[string]any)
	if scope["project"] != "handoffkeep" {
		t.Fatalf("export scope=%v", scope)
	}
	exportTasks, _ := out["tasks"].([]any)
	if len(exportTasks) != 1 || exportTasks[0].(map[string]any)["project"] != "handoffkeep" {
		t.Fatalf("export tasks=%v", exportTasks)
	}
}
