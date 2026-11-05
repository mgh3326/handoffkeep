package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type fakeLiveStore struct {
	tasks          []store.Task
	pages, queries int
	ids            []string
}

func (f *fakeLiveStore) ListTasksPage(_ context.Context, _, _, _ string, _, _ *string, _ int64, _ int) ([]store.Task, error) {
	f.pages++
	return f.tasks, nil
}
func (f *fakeLiveStore) TasksByJobIDs(_ context.Context, ids []string) ([]store.Task, error) {
	f.queries++
	f.ids = ids
	var result []store.Task
	for _, task := range f.tasks {
		for _, id := range ids {
			if task.Refs.JobID == id {
				result = append(result, task)
				break
			}
		}
	}
	return result, nil
}
func testLiveHandler() *Handler {
	p := newHubProxy("http://hub.invalid", "token", nil)
	p.cacheTTL = time.Hour
	p.liveJobsC = liveJobsCache{at: time.Now(), last: liveJobsSection{Status: "ok", FetchedAt: "now", Items: []liveJob{{JobID: "job-a"}}}}
	p.liveNodesC = liveNodesCache{at: time.Now(), last: liveNodesSection{Status: "ok", FetchedAt: "now", Items: []liveNode{}}}
	return &Handler{hub: p}
}
func testLiveTasks() []store.Task {
	return []store.Task{{ID: 1, Refs: store.TaskRefs{JobID: "job-a"}}, {ID: 2, Refs: store.TaskRefs{JobID: "job-b"}}, {ID: 3, State: store.StateDeveloping}}
}
func TestLiveScopeJobs(t *testing.T) {
	f := &fakeLiveStore{tasks: testLiveTasks()}
	h := testLiveHandler()
	w := httptest.NewRecorder()
	h.liveAPIWithStore(w, httptest.NewRequest(http.MethodGet, "/ui/api/live?scope=jobs", nil), f)
	var got liveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || f.pages != 0 || f.queries != 1 || !reflect.DeepEqual(f.ids, []string{"job-a"}) || len(got.Links) != 1 || got.Links[0].TaskID != 1 {
		t.Fatalf("status=%d store=%+v response=%+v", w.Code, f, got)
	}
	w = httptest.NewRecorder()
	h.liveAPIWithStore(w, httptest.NewRequest(http.MethodGet, "/ui/api/live?scope=bogus", nil), f)
	if w.Code != http.StatusBadRequest || w.Body.String() != "{\"error\":\"invalid_context\"}" {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}
func TestLiveDefaultScope(t *testing.T) {
	f := &fakeLiveStore{tasks: testLiveTasks()}
	w := httptest.NewRecorder()
	testLiveHandler().liveAPIWithStore(w, httptest.NewRequest(http.MethodGet, "/ui/api/live", nil), f)
	var got liveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || f.pages != 1 || f.queries != 0 || len(got.Links) != 2 || got.Links[1].TaskID != 2 || !reflect.DeepEqual(got.Mismatch.TasksWithoutJob, []int64{3}) {
		t.Fatalf("status=%d store=%+v response=%+v", w.Code, f, got)
	}
}
