package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// stateModelCall sends one authenticated request and returns the status and
// raw body.
func stateModelCall(t *testing.T, h *httptest.Server, method, path string, body any) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, h.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer node-token")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func stateModelTask(t *testing.T, raw []byte) store.Task {
	t.Helper()
	var x store.Task
	if err := json.Unmarshal(raw, &x); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return x
}

func stateModelForce(t *testing.T, s *store.Store, id int64, state string) {
	t.Helper()
	pool := benchPool(t)
	if _, err := pool.Exec(t.Context(), `UPDATE tasks SET state=$2, claimed_by='seed-worker', refs=refs||'{"job_id":"seed-job"}'::jsonb WHERE id=$1`, id, state); err != nil {
		t.Fatal(err)
	}
}

func stateModelKind(t *testing.T, s *store.Store, lane, title, kind string) store.Task {
	t.Helper()
	x, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: title, Kind: kind, Priority: 3, CreatedBy: "test-node", Project: projectRef("experiment")})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

// TestOldStateNamesOnInput is the design's "Old names on input" table over
// the HTTP API: each old name is treated as the table says, and the response
// carries only current names.
func TestOldStateNamesOnInput(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	post := func(id int64, body map[string]any) (int, []byte) {
		return stateModelCall(t, h, "POST", fmt.Sprintf("/v1/tasks/%d/transition", id), body)
	}

	// in_progress and claimed: the spawn rule's sub-state, with the same
	// linkage guard as before.
	for _, old := range []string{"in_progress", "claimed"} {
		for kind, want := range map[string]string{"implement": "developing", "ops": "developing", "verify": "researching", "decide": "researching"} {
			task := stateModelKind(t, s, lane, old+" "+kind, kind)
			if status, _ := post(task.ID, map[string]any{"to": old, "note": "start"}); status != http.StatusConflict {
				t.Fatalf("%s without linkage: status=%d, want 409 task_job_required", old, status)
			}
			status, raw := post(task.ID, map[string]any{"to": old, "note": "start", "no_job": "operator started it by hand"})
			got := stateModelTask(t, raw)
			if status != http.StatusOK || got.State != want || got.Category != "started" {
				t.Fatalf("%s on a %s task: status=%d %s", old, kind, status, raw)
			}
			// Repeating it on a started task changes nothing.
			if status, raw = post(task.ID, map[string]any{"to": old, "note": "again"}); status != http.StatusOK || stateModelTask(t, raw).State != want {
				t.Fatalf("repeated %s: status=%d %s", old, status, raw)
			}
		}
	}

	// verifying and join: reviewing.
	for _, old := range []string{"verifying", "join"} {
		task := stateModelKind(t, s, lane, old, "implement")
		stateModelForce(t, s, task.ID, "developing")
		status, raw := post(task.ID, map[string]any{"to": old, "note": "PR open"})
		if got := stateModelTask(t, raw); status != http.StatusOK || got.State != "reviewing" {
			t.Fatalf("%s: status=%d %s", old, status, raw)
		}
		if strings.Contains(string(raw), `"state":"`+old+`"`) {
			t.Fatalf("the response carries the old name %q: %s", old, raw)
		}
	}

	// hold: icebox.
	hold := stateModelKind(t, s, lane, "hold", "implement")
	if status, raw := post(hold.ID, map[string]any{"to": "hold", "note": "park"}); status != http.StatusOK || stateModelTask(t, raw).State != "icebox" {
		t.Fatalf("hold: status=%d %s", status, raw)
	}

	// needs_decision: sets awaiting_decision and keeps the work state; the
	// question is required.
	ask := stateModelKind(t, s, lane, "ask", "implement")
	stateModelForce(t, s, ask.ID, "developing")
	if status, _ := post(ask.ID, map[string]any{"to": "needs_decision"}); status != http.StatusBadRequest {
		t.Fatalf("needs_decision without a question: status=%d, want 400", status)
	}
	status, raw := post(ask.ID, map[string]any{"to": "needs_decision", "note": "Which interface?"})
	if got := stateModelTask(t, raw); status != http.StatusOK || !got.AwaitingDecision || got.State != "developing" {
		t.Fatalf("needs_decision: status=%d %s", status, raw)
	}
	// claimed on a flagged task is the answer: it clears the flag.
	status, raw = post(ask.ID, map[string]any{"to": "claimed", "note": "use A"})
	if got := stateModelTask(t, raw); status != http.StatusOK || got.AwaitingDecision || got.State != "developing" {
		t.Fatalf("claimed on a flagged task: status=%d %s", status, raw)
	}

	// reopened: the reopen path, which needs a reason and a settled task.
	done := stateModelKind(t, s, lane, "done", "implement")
	stateModelForce(t, s, done.ID, "merged")
	if status, _ := post(done.ID, map[string]any{"to": "reopened"}); status != http.StatusBadRequest {
		t.Fatalf("reopened without a reason: status=%d, want 400", status)
	}
	if status, raw := post(done.ID, map[string]any{"to": "reopened", "note": "regressed"}); status != http.StatusOK || stateModelTask(t, raw).State != "unstarted" {
		t.Fatalf("reopened: status=%d %s", status, raw)
	}

	// An unknown name is still refused.
	if status, _ := post(done.ID, map[string]any{"to": "nonsense", "note": "x"}); status != http.StatusBadRequest {
		t.Fatalf("unknown state: status=%d, want 400", status)
	}
}

// TestOldStateNamesInFilters covers list and export filters.
func TestOldStateNamesInFilters(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	mk := func(state string, flagged bool) int64 {
		x := stateModelKind(t, s, lane, state, "implement")
		stateModelForce(t, s, x.ID, state)
		if flagged {
			if _, err := benchPool(t).Exec(t.Context(), `UPDATE tasks SET awaiting_decision=true WHERE id=$1`, x.ID); err != nil {
				t.Fatal(err)
			}
		}
		return x.ID
	}
	researching, developing, reviewing, icebox, unstarted := mk("researching", false), mk("developing", false), mk("reviewing", false), mk("icebox", false), mk("unstarted", false)
	flagged := mk("backlog", true)
	want := map[string][]int64{
		"in_progress":    {researching, developing},
		"claimed":        {researching, developing},
		"verifying":      {reviewing},
		"join":           {reviewing},
		"hold":           {icebox},
		"reopened":       {unstarted},
		"needs_decision": {flagged},
		"developing":     {developing},
	}
	for name, ids := range want {
		status, raw := stateModelCall(t, h, "GET", "/v1/tasks?lane="+lane+"&state="+name, nil)
		var body struct {
			Tasks []store.Task `json:"tasks"`
		}
		if status != http.StatusOK || json.Unmarshal(raw, &body) != nil || len(body.Tasks) != len(ids) {
			t.Fatalf("list state=%s: status=%d %s", name, status, raw)
		}
		have := map[int64]bool{}
		for _, x := range body.Tasks {
			have[x.ID] = true
		}
		for _, id := range ids {
			if !have[id] {
				t.Fatalf("list state=%s: missing %d in %s", name, id, raw)
			}
		}
		status, raw = stateModelCall(t, h, "GET", "/v1/tasks/export?lane="+lane+"&state="+name, nil)
		var export store.TaskExport
		if status != http.StatusOK || json.Unmarshal(raw, &export) != nil || export.Counts.Total != len(ids) {
			t.Fatalf("export state=%s: status=%d %s", name, status, raw)
		}
	}
	if status, _ := stateModelCall(t, h, "GET", "/v1/tasks?state=nonsense", nil); status != http.StatusBadRequest {
		t.Fatalf("unknown state filter: status=%d, want 400", status)
	}
}

func TestHierarchyRoutes(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	a, b, c := stateModelKind(t, s, lane, "a", "implement"), stateModelKind(t, s, lane, "b", "implement"), stateModelKind(t, s, lane, "c", "implement")
	parentPath := func(id int64) string { return fmt.Sprintf("/v1/tasks/%d/parent", id) }

	if status, raw := stateModelCall(t, h, "POST", parentPath(b.ID), map[string]any{"parent_id": a.ID}); status != http.StatusOK || stateModelTask(t, raw).ParentID == nil {
		t.Fatalf("set parent: status=%d %s", status, raw)
	}
	if status, raw := stateModelCall(t, h, "POST", parentPath(c.ID), map[string]any{"parent_id": b.ID}); status != http.StatusOK {
		t.Fatalf("set parent: status=%d %s", status, raw)
	}
	status, raw := stateModelCall(t, h, "POST", parentPath(a.ID), map[string]any{"parent_id": c.ID})
	if status != http.StatusConflict || !strings.Contains(string(raw), "parent_cycle") {
		t.Fatalf("cycle: status=%d %s", status, raw)
	}
	if status, raw = stateModelCall(t, h, "POST", parentPath(a.ID), map[string]any{"parent_id": a.ID}); status != http.StatusBadRequest || !strings.Contains(string(raw), "parent_invalid") {
		t.Fatalf("self parent: status=%d %s", status, raw)
	}
	// Detail shows the children.
	status, raw = stateModelCall(t, h, "GET", fmt.Sprintf("/v1/tasks/%d", a.ID), nil)
	if detail := stateModelTask(t, raw); status != http.StatusOK || len(detail.Children) != 1 || detail.Children[0].ID != b.ID {
		t.Fatalf("detail: status=%d %s", status, raw)
	}
	if status, raw = stateModelCall(t, h, "POST", parentPath(b.ID), map[string]any{"parent_id": nil}); status != http.StatusOK || stateModelTask(t, raw).ParentID != nil {
		t.Fatalf("clear parent: status=%d %s", status, raw)
	}

	relPath := func(id int64, suffix string) string { return fmt.Sprintf("/v1/tasks/%d/relations%s", id, suffix) }
	relation := func(id int64, suffix, kind string, other int64) (int, bool) {
		status, raw := stateModelCall(t, h, "POST", relPath(id, suffix), map[string]any{"kind": kind, "other": other})
		var out struct {
			Changed bool       `json:"changed"`
			Task    store.Task `json:"task"`
		}
		_ = json.Unmarshal(raw, &out)
		if status == http.StatusOK && suffix == "" && len(out.Task.Relations) == 0 {
			t.Fatalf("relations missing from %s", raw)
		}
		return status, out.Changed
	}
	if status, changed := relation(a.ID, "", "blocks", b.ID); status != http.StatusOK || !changed {
		t.Fatalf("add blocks: status=%d changed=%t", status, changed)
	}
	if status, changed := relation(a.ID, "", "blocks", b.ID); status != http.StatusOK || changed {
		t.Fatalf("repeat add: status=%d changed=%t", status, changed)
	}
	if status, _ := relation(b.ID, "", "blocks", a.ID); status != http.StatusConflict {
		t.Fatalf("blocks cycle: status=%d, want 409", status)
	}
	if status, _ := relation(a.ID, "", "blocks", a.ID); status != http.StatusBadRequest {
		t.Fatalf("self link: status=%d, want 400", status)
	}
	if status, changed := relation(a.ID, "/remove", "blocks", b.ID); status != http.StatusOK || !changed {
		t.Fatalf("remove: status=%d changed=%t", status, changed)
	}
	if status, changed := relation(a.ID, "/remove", "blocks", b.ID); status != http.StatusOK || changed {
		t.Fatalf("repeat remove: status=%d changed=%t", status, changed)
	}

	// duplicate needs its duplicate_of, and both land together.
	d := stateModelKind(t, s, lane, "d", "implement")
	status, raw = stateModelCall(t, h, "POST", fmt.Sprintf("/v1/tasks/%d/transition", d.ID), map[string]any{"to": "duplicate", "note": "same"})
	if status != http.StatusBadRequest || !strings.Contains(string(raw), "duplicate_of_required") {
		t.Fatalf("duplicate without duplicate_of: status=%d %s", status, raw)
	}
	status, raw = stateModelCall(t, h, "POST", fmt.Sprintf("/v1/tasks/%d/transition", d.ID), map[string]any{"to": "duplicate", "note": "same", "duplicate_of": a.ID})
	if got := stateModelTask(t, raw); status != http.StatusOK || got.State != "duplicate" || got.Category != "canceled" {
		t.Fatalf("duplicate: status=%d %s", status, raw)
	}
	status, raw = stateModelCall(t, h, "GET", fmt.Sprintf("/v1/tasks/%d", d.ID), nil)
	if detail := stateModelTask(t, raw); len(detail.Relations) != 1 || detail.Relations[0].Kind != "duplicate_of" || detail.Relations[0].DstID != a.ID {
		t.Fatalf("duplicate_of relation: %s", raw)
	}

	// The triage gate over HTTP.
	triage := stateModelKind(t, s, lane, "triage", "implement")
	stateModelForce(t, s, triage.ID, "triage")
	if _, err := benchPool(t).Exec(t.Context(), `UPDATE tasks SET priority=0 WHERE id=$1`, triage.ID); err != nil {
		t.Fatal(err)
	}
	status, raw = stateModelCall(t, h, "POST", fmt.Sprintf("/v1/tasks/%d/transition", triage.ID), map[string]any{"to": "backlog", "note": "accept"})
	if status != http.StatusConflict || !strings.Contains(string(raw), "triage_gate") {
		t.Fatalf("triage gate: status=%d %s", status, raw)
	}
}
