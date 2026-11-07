package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestProjectOwnersListAPI: GET /v1/tasks/projects/owners lists every project
// with both design_lane and the operator-desk fallback.
func TestProjectOwnersListAPI(t *testing.T) {
	h, _ := relaneAPIServer(t)
	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/tasks/projects/owners", "tok", nil)
	if code != 200 {
		t.Fatalf("list code=%d body=%v", code, out)
	}
	projects, _ := out["projects"].([]any)
	if len(projects) != 18 {
		t.Fatalf("owners=%v", projects)
	}
	for _, raw := range projects {
		row := raw.(map[string]any)
		if row["design_lane"] != "" || row["effective_owner"] != store.OperatorDeskLane {
			t.Fatalf("unclaimed row=%v", row)
		}
	}
	// Unauthenticated refuses.
	if code, _ = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks/projects/owners", "", nil); code != 401 {
		t.Fatalf("unauthenticated code=%d", code)
	}
}

// TestProjectOwnerRoutesAPI covers GET one, PUT set/clear, the actor from the
// token, 404 not_found for an unknown project and 400 invalid_context for a
// bad lane.
func TestProjectOwnerRoutesAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	path := "/v1/tasks/projects/handoffkeep/owner"

	// A project nobody claimed answers the fallback and no events.
	code, out := projectRequest(t, h.URL, http.MethodGet, path, "tok", nil)
	if code != 200 || out["name"] != "handoffkeep" || out["design_lane"] != "" || out["effective_owner"] != store.OperatorDeskLane {
		t.Fatalf("get code=%d body=%v", code, out)
	}
	if events, _ := out["events"].([]any); len(events) != 0 {
		t.Fatalf("fresh events=%v", events)
	}

	// PUT sets it; the actor is the authenticated client, never the body.
	code, out = projectRequest(t, h.URL, http.MethodPut, path, "tok", map[string]any{"design_lane": "handoffkeep-design", "note": "decision/2026-11-07/design-session-desk-authority"})
	if code != 200 || out["design_lane"] != "handoffkeep-design" || out["effective_owner"] != "handoffkeep-design" {
		t.Fatalf("put code=%d body=%v", code, out)
	}
	assertOwnerHistory(t, st, "handoffkeep", 1, "", "handoffkeep-design", "tester")

	// An attempt to supply the actor is a strict-decoder refusal.
	if code, out = projectRequest(t, h.URL, http.MethodPut, path, "tok", map[string]any{"design_lane": "handoffkeep-design", "actor": "someone"}); code != 400 {
		t.Fatalf("client-supplied actor accepted: code=%d body=%v", code, out)
	}

	// GET now carries the owner and its one event.
	code, out = projectRequest(t, h.URL, http.MethodGet, path, "tok", nil)
	if code != 200 || out["design_lane"] != "handoffkeep-design" {
		t.Fatalf("get after set code=%d body=%v", code, out)
	}
	events, _ := out["events"].([]any)
	if len(events) != 1 || events[0].(map[string]any)["actor"] != "tester" {
		t.Fatalf("events=%v", events)
	}

	// PUT with an empty lane clears the owner and falls back.
	code, out = projectRequest(t, h.URL, http.MethodPut, path, "tok", map[string]any{"design_lane": ""})
	if code != 200 || out["design_lane"] != "" || out["effective_owner"] != store.OperatorDeskLane {
		t.Fatalf("clear code=%d body=%v", code, out)
	}
	assertOwnerHistory(t, st, "handoffkeep", 2, "handoffkeep-design", "", "tester")

	// An unknown project is not_found; a malformed lane is invalid_context.
	if code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/tasks/projects/not-a-project/owner", "tok", nil); code != 404 || out["error"] != "not_found" {
		t.Fatalf("unknown project get code=%d body=%v", code, out)
	}
	if code, out = projectRequest(t, h.URL, http.MethodPut, "/v1/tasks/projects/not-a-project/owner", "tok", map[string]any{"design_lane": "x-design"}); code != 404 || out["error"] != "not_found" {
		t.Fatalf("unknown project put code=%d body=%v", code, out)
	}
	if code, out = projectRequest(t, h.URL, http.MethodPut, path, "tok", map[string]any{"design_lane": "bad lane!"}); code != 400 || out["error"] != "invalid_context" {
		t.Fatalf("bad lane code=%d body=%v", code, out)
	}
	// The bad-lane refusal wrote nothing.
	assertOwnerHistory(t, st, "handoffkeep", 2, "handoffkeep-design", "", "tester")
	// Unauthenticated PUT refuses.
	if code, _ = projectRequest(t, h.URL, http.MethodPut, path, "", map[string]any{"design_lane": "x-design"}); code != 401 {
		t.Fatalf("unauthenticated put code=%d", code)
	}
}

// TestProjectMetadataLeavesDesignLaneAPI pins that the existing metadata
// routes keep working and do not clear the owner set through the owner route.
func TestProjectMetadataLeavesDesignLaneAPI(t *testing.T) {
	h, _ := relaneAPIServer(t)
	ownerPath := "/v1/tasks/projects/wrk/owner"
	if code, out := projectRequest(t, h.URL, http.MethodPut, ownerPath, "tok", map[string]any{"design_lane": "wrk-design"}); code != 200 {
		t.Fatalf("set owner code=%d body=%v", code, out)
	}
	metaPath := "/v1/tasks/projects/wrk/metadata"
	if code, out := projectRequest(t, h.URL, http.MethodPut, metaPath, "tok", map[string]any{"name": "wrk", "status": "active"}); code != 200 {
		t.Fatalf("put metadata code=%d body=%v", code, out)
	}
	if code, out := projectRequest(t, h.URL, http.MethodPatch, metaPath, "tok", map[string]any{"status": "paused"}); code != 200 {
		t.Fatalf("patch metadata code=%d body=%v", code, out)
	}
	code, out := projectRequest(t, h.URL, http.MethodGet, ownerPath, "tok", nil)
	if code != 200 || out["design_lane"] != "wrk-design" || out["effective_owner"] != "wrk-design" {
		t.Fatalf("metadata write cleared the owner: code=%d body=%v", code, out)
	}
}

func assertOwnerHistory(t *testing.T, st *store.Store, project string, want int, previous, lane, actor string) {
	t.Helper()
	events, err := st.ProjectOwnerHistory(context.Background(), project, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != want {
		t.Fatalf("%s history has %d events, want %d: %+v", project, len(events), want, events)
	}
	latest := events[0]
	if latest.PreviousLane != previous || latest.DesignLane != lane || latest.Actor != actor {
		t.Fatalf("%s latest event=%+v, want previous=%q lane=%q actor=%q", project, latest, previous, lane, actor)
	}
}

// TestProjectOwnerRouteDoesNotShadowVocabulary pins that the literal owners
// route wins over /v1/tasks/projects/{project}/... patterns and that the
// vocabulary list keeps its exact wire shape.
func TestProjectOwnerRouteDoesNotShadowVocabulary(t *testing.T) {
	h, _ := relaneAPIServer(t)
	code, out := projectRequest(t, h.URL, http.MethodGet, "/v1/tasks/projects", "tok", nil)
	if code != 200 {
		t.Fatalf("vocabulary code=%d", code)
	}
	names, _ := out["projects"].([]any)
	if len(names) != 18 || names[0] != "admiral" {
		t.Fatalf("vocabulary=%v", names)
	}
	if _, ok := out["effective_owner"]; ok {
		t.Fatalf("vocabulary response gained an owner field: %v", out)
	}
	// An unknown path under /owner does not fall back to the metadata route.
	code, out = projectRequest(t, h.URL, http.MethodGet, fmt.Sprintf("/v1/tasks/projects/%s/owner", "missing"), "tok", nil)
	if code != 404 || out["error"] != "not_found" {
		t.Fatalf("missing owner code=%d body=%v", code, out)
	}
}
