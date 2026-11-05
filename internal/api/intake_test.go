package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func apiTicketBody(what string) string {
	return "## Current\n" + what + "\n## Desired\nd\n## Key interfaces\nk\n## Out of scope\no\n## Acceptance\n1. a\n"
}

func apiTicketSet(ref string) map[string]any {
	return map[string]any{
		"source": "repo-session", "source_ref": ref,
		"parent": map[string]any{"title": "api set " + ref, "body": apiTicketBody("parent")},
		"children": []any{
			map[string]any{"ref": "a", "title": "api slice a " + ref, "body": apiTicketBody("a")},
			map[string]any{"ref": "b", "title": "api slice b " + ref, "body": apiTicketBody("b"), "blocked_by": []any{"a"}},
		},
	}
}

// POST /v1/intake answers 201 created:true, then 200 created:false with the
// same task for a repeated (source, source_ref).
func TestIntakeRouteAPI(t *testing.T) {
	h, _ := relaneAPIServer(t)
	body := map[string]any{"source": "linear", "source_ref": "ROB-1", "title": "triage me", "body": "details"}
	code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/intake", "tok", body)
	task, _ := out["task"].(map[string]any)
	if code != http.StatusCreated || out["created"] != true || task == nil || task["state"] != "triage" || task["kind"] != "" || task["created_by"] != "tester" {
		t.Fatalf("intake code=%d body=%v", code, out)
	}
	code, again := projectRequest(t, h.URL, http.MethodPost, "/v1/intake", "tok", body)
	if code != http.StatusOK || again["created"] != false || again["task"].(map[string]any)["id"] != task["id"] {
		t.Fatalf("repeat code=%d body=%v", code, again)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/intake", "tok", map[string]any{"source": "linear", "title": "no ref"})
	if code != http.StatusBadRequest || out["error"] != "invalid_context" {
		t.Fatalf("no source_ref code=%d body=%v", code, out)
	}
	if code, _ = projectRequest(t, h.URL, http.MethodPost, "/v1/intake", "", body); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated code=%d", code)
	}
}

// POST /v1/ticket-sets creates the set, returns the same ids for the same
// payload, refuses a changed payload with 409 ticket_set_conflict and a body
// without a section with 400 ticket_set_invalid; GET /v1/triage shows the set
// as one row; the triage exit with set accepts the children.
func TestTicketSetRoutesAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	set := apiTicketSet("api-1")
	code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/ticket-sets", "tok", set)
	if code != http.StatusCreated || out["created"] != true {
		t.Fatalf("emit code=%d body=%v", code, out)
	}
	parentID := out["parent_id"]
	children := fmt.Sprint(out["children"])
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/ticket-sets", "tok", set)
	if code != http.StatusOK || out["created"] != false || out["parent_id"] != parentID || fmt.Sprint(out["children"]) != children {
		t.Fatalf("repeat code=%d body=%v", code, out)
	}
	set["parent"].(map[string]any)["title"] = "renamed"
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/ticket-sets", "tok", set)
	if code != http.StatusConflict || out["error"] != "ticket_set_conflict" {
		t.Fatalf("changed code=%d body=%v", code, out)
	}
	bad := apiTicketSet("api-bad")
	bad["children"].([]any)[0].(map[string]any)["body"] = "## Current\nonly one heading\n"
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/ticket-sets", "tok", bad)
	if code != http.StatusBadRequest || out["error"] != "ticket_set_invalid" || !strings.Contains(fmt.Sprint(out["reason"]), "Acceptance") {
		t.Fatalf("missing heading code=%d body=%v", code, out)
	}
	cyclic := apiTicketSet("api-cycle")
	cyclic["children"].([]any)[0].(map[string]any)["blocked_by"] = []any{"b"}
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/ticket-sets", "tok", cyclic)
	if code != http.StatusConflict || out["error"] != "relation_cycle" {
		t.Fatalf("cycle code=%d body=%v", code, out)
	}

	code, out = projectRequest(t, h.URL, http.MethodGet, "/v1/triage", "tok", nil)
	rows, _ := out["triage"].([]any)
	if code != http.StatusOK || len(rows) != 1 {
		t.Fatalf("triage code=%d body=%v", code, out)
	}
	row := rows[0].(map[string]any)
	if row["ticket_set"] != true || row["child_count"] != float64(2) || row["task"].(map[string]any)["id"] != parentID {
		t.Fatalf("triage row=%v", row)
	}

	code, out = projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%v/transition", parentID), "tok", map[string]any{"to": "backlog", "set": true, "priority": 2, "kind": "fix", "project": "experiment"})
	if code != http.StatusOK || out["state"] != "backlog" || out["priority"] != float64(2) {
		t.Fatalf("accept set code=%d body=%v", code, out)
	}
	children2, err := st.ListTaskChildren(t.Context(), int64(parentID.(float64)))
	if err != nil || len(children2) != 2 || children2[0].State != "backlog" || children2[1].State != "backlog" {
		t.Fatalf("children=%+v err=%v", children2, err)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%v/transition", parentID), "tok", map[string]any{"to": "unstarted", "kind": "fix"})
	if code != http.StatusBadRequest {
		t.Fatalf("triage field outside triage code=%d body=%v", code, out)
	}
}

// POST /v1/tasks refuses a similar title with 409 similar_tasks listing the
// candidates, unless the body carries link or new.
func TestTasksCreateDuplicateCheckAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	create := func(extra map[string]any) (int, map[string]any) {
		body := map[string]any{"lane": "dup", "title": "rebuild the console bundle", "kind": "fix", "project": "experiment"}
		for k, v := range extra {
			body[k] = v
		}
		return projectRequest(t, h.URL, http.MethodPost, "/v1/tasks", "tok", body)
	}
	code, first := create(nil)
	if code != http.StatusCreated {
		t.Fatalf("first code=%d body=%v", code, first)
	}
	code, out := create(map[string]any{"title": "Rebuild the console bundle"})
	similar, _ := out["similar_tasks"].([]any)
	if code != http.StatusConflict || out["error"] != "similar_tasks" || len(similar) != 1 || similar[0].(map[string]any)["id"] != first["id"] {
		t.Fatalf("similar code=%d body=%v", code, out)
	}
	code, linked := create(map[string]any{"link": first["id"]})
	if code != http.StatusCreated {
		t.Fatalf("link code=%d body=%v", code, linked)
	}
	rels, err := st.ListTaskRelations(t.Context(), int64(linked["id"].(float64)))
	if err != nil || len(rels) != 1 || rels[0].Kind != "related" {
		t.Fatalf("link relations=%+v err=%v", rels, err)
	}
	if code, out = create(map[string]any{"new": true}); code != http.StatusCreated {
		t.Fatalf("new code=%d body=%v", code, out)
	}
	if code, out = create(map[string]any{"title": "write the quota runbook"}); code != http.StatusCreated {
		t.Fatalf("dissimilar code=%d body=%v", code, out)
	}
	if code, out = create(map[string]any{"link": first["id"], "new": true}); code != http.StatusBadRequest {
		t.Fatalf("link and new code=%d body=%v", code, out)
	}
}

func TestSimilarTasksErrorBody(t *testing.T) {
	w := httptest.NewRecorder()
	err := fmt.Errorf("create: %w", &store.SimilarTasksError{Tasks: []store.SimilarTask{{ID: 4, Title: "t", State: "backlog", Lane: "l", Similarity: 0.8}}})
	appErr(w, httptest.NewRequest(http.MethodPost, "/v1/tasks", nil), err)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"error":"similar_tasks"`) || !strings.Contains(w.Body.String(), `"similar_tasks":[{"id":4,`) {
		t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
	}
}
