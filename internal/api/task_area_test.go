package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// areaNoDBServer is a Server with no store behind it. Every request in the
// store-free tests below must be refused (or answered) before the service
// touches the store; a nil-store dereference fails the test loudly.
func areaNoDBServer(t *testing.T) *httptest.Server {
	t.Helper()
	h := httptest.NewServer(Server{Service: Service{}, Tokens: Tokens{"tester": "tok"}}.Handler())
	t.Cleanup(h.Close)
	return h
}

func reasonNamesAllowedSet(t *testing.T, out map[string]any) {
	t.Helper()
	reason, _ := out["reason"].(string)
	if !strings.Contains(reason, strings.Join(store.TaskAreas(), ", ")) {
		t.Fatalf("reason %q must name the allowed set in order", reason)
	}
}

// An unknown area is a 400 invalid_request that names the allowed set, on
// create, on set-area and on the list filter. No store is attached: the
// refusal must not depend on a database round trip.
func TestTaskAreaUnknownValueIs400(t *testing.T) {
	h := areaNoDBServer(t)
	for _, bad := range []string{"Blender", "go", "", " docs", "ｏｐｓ"} {
		t.Run("create/"+bad, func(t *testing.T) {
			code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/tasks", "tok", map[string]any{"lane": "d", "title": "x", "kind": "implement", "project": "experiment", "area": bad})
			if code != 400 || out["error"] != "invalid_request" {
				t.Fatalf("create code=%d body=%v", code, out)
			}
			reasonNamesAllowedSet(t, out)
		})
		t.Run("set/"+bad, func(t *testing.T) {
			code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/7/area", "tok", map[string]any{"area": bad, "note": "n"})
			if code != 400 || out["error"] != "invalid_request" {
				t.Fatalf("set code=%d body=%v", code, out)
			}
			reasonNamesAllowedSet(t, out)
		})
	}
	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?area=Blender", "tok", nil)
	if code != 400 || out["error"] != "invalid_request" {
		t.Fatalf("list filter code=%d body=%v", code, out)
	}
	reasonNamesAllowedSet(t, out)
}

// The set-area body must carry the area key. An omitted key is not a clear:
// only an explicit null clears, so a body typo cannot wipe a tag.
func TestTaskAreaSetRequiresExplicitKey(t *testing.T) {
	h := areaNoDBServer(t)
	for name, body := range map[string]map[string]any{
		"empty body":   {},
		"note only":    {"note": "oops"},
		"wrong key":    {"areas": "docs"},
		"wrong type":   {"area": 7},
		"list value":   {"area": []string{"docs"}},
		"object value": {"area": map[string]any{"x": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/7/area", "tok", body)
			if code != 400 {
				t.Fatalf("code=%d body=%v, want 400", code, out)
			}
		})
	}
	if code, _ := projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/0/area", "tok", map[string]any{"area": "docs"}); code != 400 {
		t.Fatalf("task id 0 code=%d, want 400", code)
	}
	if code, _ := projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/7/area", "", map[string]any{"area": "docs"}); code != 401 {
		t.Fatalf("unauthenticated set code=%d, want 401", code)
	}
}

// GET /v1/task-areas answers the vocabulary in its code order and requires
// authentication like every other read.
func TestTaskAreasListAPI(t *testing.T) {
	h := areaNoDBServer(t)
	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/task-areas", "tok", nil)
	if code != 200 {
		t.Fatalf("code=%d body=%v", code, out)
	}
	got, _ := out["areas"].([]any)
	if len(got) != len(store.TaskAreas()) {
		t.Fatalf("areas=%v", got)
	}
	for i, want := range store.TaskAreas() {
		if got[i] != want {
			t.Fatalf("areas[%d]=%v, want %s", i, got[i], want)
		}
	}
	if code, _ = projectRequest(t, h.URL, http.MethodGet, "/v1/task-areas", "", nil); code != 401 {
		t.Fatalf("unauthenticated code=%d", code)
	}
	// Read only: the vocabulary has no write route.
	if code, _ = projectRequest(t, h.URL, http.MethodPost, "/v1/task-areas", "tok", map[string]any{"name": "new"}); code != 405 && code != 404 {
		t.Fatalf("POST /v1/task-areas code=%d, want 404/405", code)
	}
}

// Create with an area, set, no-op, clear, list filter, and the always-present
// key — against a real database.
func TestTaskAreaRoutesAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	lane := relaneLane(t, "api-area")
	code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/tasks", "tok", map[string]any{"lane": lane, "title": "tagged", "kind": "implement", "project": "experiment", "area": "python"})
	if code != 201 || out["area"] != "python" {
		t.Fatalf("create code=%d body=%v", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/tasks", "tok", map[string]any{"lane": lane, "title": "untagged", "kind": "implement", "project": "experiment"})
	if v, ok := out["area"]; code != 201 || !ok || v != nil {
		t.Fatalf("untagged create code=%d body=%v (area key must be present and null)", code, out)
	}
	id := int64(out["id"].(float64))
	path := fmt.Sprintf("/v1/tasks/%d/area", id)

	code, out = projectRequest(t, h.URL, http.MethodPost, path, "tok", map[string]any{"area": "docs", "note": "readme only"})
	if code != 200 || out["area"] != "docs" {
		t.Fatalf("set code=%d body=%v", code, out)
	}
	areaEvents := func() []store.TaskEvent {
		got, _, err := st.GetTask(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		var evs []store.TaskEvent
		for _, e := range got.Events {
			if e.Kind == store.TaskEventArea {
				evs = append(evs, e)
			}
		}
		return evs
	}
	if evs := areaEvents(); len(evs) != 1 || evs[0].From != "" || evs[0].To != "docs" || evs[0].By != "tester" || evs[0].Note != "readme only" {
		t.Fatalf("set event=%+v", evs)
	}
	// Same value again: 200, area unchanged, no new event.
	if code, out = projectRequest(t, h.URL, http.MethodPost, path, "tok", map[string]any{"area": "docs"}); code != 200 || out["area"] != "docs" || len(areaEvents()) != 1 {
		t.Fatalf("no-op code=%d body=%v events=%d", code, out, len(areaEvents()))
	}
	// Filter.
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?lane="+lane+"&area=docs", "tok", nil)
	tasks, _ := out["tasks"].([]any)
	if code != 200 || len(tasks) != 1 || out["area"] != "docs" {
		t.Fatalf("filter code=%d body=%v", code, out)
	}
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?lane="+lane+"&area=", "tok", nil)
	if tasks, _ = out["tasks"].([]any); code != 200 || len(tasks) != 0 {
		t.Fatalf("area='' filter code=%d body=%v (both tasks have an area)", code, out)
	}
	// Explicit null clears.
	code, out = projectRequest(t, h.URL, http.MethodPost, path, "tok", map[string]any{"area": nil, "note": "wrong tag"})
	if v, ok := out["area"]; code != 200 || !ok || v != nil {
		t.Fatalf("clear code=%d body=%v", code, out)
	}
	if evs := areaEvents(); len(evs) != 2 || evs[1].From != "docs" || evs[1].To != "" {
		t.Fatalf("clear event=%+v", evs)
	}
	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks?lane="+lane+"&area=", "tok", nil)
	if tasks, _ = out["tasks"].([]any); code != 200 || len(tasks) != 1 {
		t.Fatalf("area='' after clear code=%d body=%v", code, out)
	}
	if code, _ = projectRequest(t, h.URL, http.MethodPost, "/v1/tasks/999999/area", "tok", map[string]any{"area": "docs"}); code != 404 {
		t.Fatalf("missing task code=%d, want 404", code)
	}
}
