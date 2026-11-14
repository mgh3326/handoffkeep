package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

const eventPR = "https://github.com/example/app/pull/7"

// taskEventServer answers POST /v1/task-events by task id the way the server
// does: 1 applied, 2 not applied, 3 redelivered, 4 unknown source, 5 missing.
func taskEventServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/task-events" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var in store.TaskSourceEvent
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch in.TaskID {
		case 1:
			_ = json.NewEncoder(w).Encode(store.TaskEventResult{Applied: true, State: "merged"})
		case 2:
			_ = json.NewEncoder(w).Encode(store.TaskEventResult{State: "dropped", Reason: "the task is canceled"})
		case 3:
			_ = json.NewEncoder(w).Encode(store.TaskEventResult{Redelivered: true, State: "merged"})
		case 4:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_context"}`))
		case 5:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found"}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestApplyTaskEvent(t *testing.T) {
	h := taskEventServer(t)
	defer h.Close()
	c := Client{URL: h.URL, Token: "t"}
	ctx := t.Context()
	ev := func(id int64) store.TaskSourceEvent {
		return store.TaskSourceEvent{Source: store.SourceMergewakeMerge, SourceRef: eventPR, TaskID: id}
	}
	if res, err := c.ApplyTaskEvent(ctx, ev(1)); err != nil || !res.Applied || res.State != "merged" {
		t.Fatalf("applied: %+v %v", res, err)
	}
	if res, err := c.ApplyTaskEvent(ctx, ev(2)); err != nil || res.Applied || res.Reason == "" {
		t.Fatalf("not applied: %+v %v", res, err)
	}
	if res, err := c.ApplyTaskEvent(ctx, ev(3)); err != nil || res.Applied || !res.Redelivered {
		t.Fatalf("redelivered: %+v %v", res, err)
	}
	if _, err := c.ApplyTaskEvent(ctx, ev(4)); ErrorCode(err) != "invalid_context" {
		t.Fatalf("unknown source: %v", err)
	}
	if _, err := c.ApplyTaskEvent(ctx, ev(5)); ErrorCode(err) != "not_found" {
		t.Fatalf("missing task: %v", err)
	}
	// A server without the route says so instead of a bare 404.
	if _, err := c.ApplyTaskEvent(ctx, ev(6)); err == nil || !strings.Contains(err.Error(), "task_events_unsupported") {
		t.Fatalf("old server: %v", err)
	}
}

func TestListTasksFilteredStalledEcho(t *testing.T) {
	echo := true
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("stalled") != "true" {
			t.Errorf("query %s", r.URL.RawQuery)
		}
		body := map[string]any{"tasks": []store.Task{{ID: 1, State: "developing", Stalled: true}}}
		if echo {
			body["stalled"] = true
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer h.Close()
	c := Client{URL: h.URL, Token: "t"}
	xs, err := c.ListTasksFiltered(t.Context(), store.TaskQuery{Stalled: true, Limit: 20})
	if err != nil || len(xs) != 1 || !xs[0].Stalled {
		t.Fatalf("stalled list: %+v %v", xs, err)
	}
	echo = false
	if _, err := c.ListTasksFiltered(t.Context(), store.TaskQuery{Stalled: true, Limit: 20}); err == nil || !strings.Contains(err.Error(), "list_stalled_filter_ignored") {
		t.Fatalf("old server: %v", err)
	}
}

func TestSweepPreview(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tasks/sweep" || r.URL.Query().Get("days") != "14" {
			t.Errorf("request %s %s", r.Method, r.URL.String())
		}
		_, _ = w.Write([]byte(`{"days":14,"dry_run":true,"tasks":[{"id":3,"lane":"l","title":"idle","idle_since":"2026-10-01T00:00:00Z"}]}`))
	}))
	defer h.Close()
	xs, err := Client{URL: h.URL, Token: "t"}.SweepPreview(t.Context(), 14)
	if err != nil || len(xs) != 1 || xs[0].ID != 3 {
		t.Fatalf("preview: %+v %v", xs, err)
	}
}

func TestTaskHygieneRemote(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tasks/hygiene" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var in struct {
			Project string `json:"project"`
			DryRun  *bool  `json:"dry_run"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if in.Project != "my-project" {
			t.Errorf("expected project my-project, got %q", in.Project)
		}
		if in.DryRun == nil || !*in.DryRun {
			t.Errorf("expected dry_run=true, got %v", in.DryRun)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(store.TaskHygieneReport{
			DryRun:           true,
			Project:          in.Project,
			MechanicalCloses: []store.TaskHygieneClose{{TaskID: 10, ToState: "merged", Rule: "merged_pr"}},
		})
	}))
	defer h.Close()
	c := Client{URL: h.URL, Token: "t"}
	rep, err := c.TaskHygiene(t.Context(), "my-project", true)
	if err != nil || !rep.DryRun || rep.Project != "my-project" || len(rep.MechanicalCloses) != 1 || rep.MechanicalCloses[0].TaskID != 10 {
		t.Fatalf("TaskHygiene: %+v %v", rep, err)
	}

	hOld := httptest.NewServer(http.NotFoundHandler())
	defer hOld.Close()
	cOld := Client{URL: hOld.URL, Token: "t"}
	if _, err := cOld.TaskHygiene(t.Context(), "", true); err == nil || !strings.Contains(err.Error(), "task_hygiene_unsupported") {
		t.Fatalf("old server: %v", err)
	}
}
