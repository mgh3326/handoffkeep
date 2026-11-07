package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func lifecycleHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	_, url := testdb.Schema(t, "ui_lifecycle")
	st, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return &Handler{store: st, csrfKey: []byte("lifecycle-test")}, st
}

// lifecycleSeed files a backlog task and walks it to state through the store's
// own transitions, so the seeded row obeys the guards the actions will.
func lifecycleSeed(t *testing.T, st *store.Store, lane, state, kind string) store.Task {
	t.Helper()
	ctx := context.Background()
	project := "experiment"
	x, err := st.CreateTask(ctx, store.Task{Lane: lane, Title: "lifecycle " + state + " " + kind, Kind: kind, Priority: 3, CreatedBy: "seed", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	move := func(in store.TransitionInput) {
		in.By, in.NoJob = "seed", "seed without a job"
		if _, err := st.TransitionTaskWith(ctx, x.ID, in); err != nil {
			t.Fatalf("seed %s: %v", state, err)
		}
	}
	switch state {
	case store.StateBacklog:
	case store.StateUnstarted:
		move(store.TransitionInput{To: store.StateUnstarted})
	case store.StateDeveloping:
		move(store.TransitionInput{To: store.StateDeveloping})
	case store.StateReviewing:
		move(store.TransitionInput{To: store.StateDeveloping})
		move(store.TransitionInput{To: store.StateReviewing})
	case store.StateMerged:
		move(store.TransitionInput{To: store.StateDeveloping})
		move(store.TransitionInput{To: store.StateReviewing})
		move(store.TransitionInput{To: store.StateMerged, Evidence: "https://example.com/pr/1"})
	case store.StateIcebox:
		move(store.TransitionInput{To: store.StateIcebox})
	default:
		t.Fatalf("unsupported seed state %q", state)
	}
	got, found, err := st.GetTask(ctx, x.ID)
	if err != nil || !found {
		t.Fatalf("seed lookup found=%t err=%v", found, err)
	}
	return got
}

type lifecycleCall struct {
	path   string
	origin string
	body   string
	csrf   bool
}

func (h *Handler) lifecycleRequest(identity cfaccess.Identity, call lifecycleCall) (int, map[string]any) {
	origin := call.origin
	if origin == "" {
		origin = "http://example.com"
	}
	r := httptest.NewRequest(http.MethodPost, "http://example.com"+call.path, strings.NewReader(call.body))
	r.Header.Set("Origin", origin)
	if call.csrf {
		token, err := h.newCSRFToken(identity.Email)
		if err == nil {
			r.AddCookie(&http.Cookie{Name: csrfCookie, Value: token})
			r.Header.Set("X-CSRF-Token", token)
		}
	}
	w := httptest.NewRecorder()
	h.serveAPI(w, r, identity)
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	return w.Code, result
}

func TestTaskLifecycleGuards(t *testing.T) {
	h := &Handler{csrfKey: []byte("lifecycle-guard")}
	operator := cfaccess.Identity{Email: "operator@example.com"}
	for _, action := range []string{"close-merged", "close-completed", "drop", "hold", "reopen", "relane"} {
		path := "/ui/api/tasks/1/" + action
		t.Run(action, func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				identity cfaccess.Identity
				call     lifecycleCall
				status   int
				code     string
			}{
				{"service", cfaccess.Identity{ServiceName: "svc"}, lifecycleCall{path: path, body: `{}`}, 403, "operator_only"},
				{"origin", operator, lifecycleCall{path: path, origin: "http://evil.example", body: `{}`, csrf: true}, 403, "origin_rejected"},
				{"csrf", operator, lifecycleCall{path: path, body: `{}`}, 403, "csrf_rejected"},
				{"unknown", operator, lifecycleCall{path: path, body: `{"unknown":true}`, csrf: true}, 400, "invalid_request"},
				{"trailing", operator, lifecycleCall{path: path, body: `{} {}`, csrf: true}, 400, "invalid_request"},
				{"missing-key", operator, lifecycleCall{path: path, body: `{}`, csrf: true}, 400, "event_key_required"},
			} {
				status, got := h.lifecycleRequest(tc.identity, tc.call)
				if status != tc.status || got["error"] != tc.code {
					t.Fatalf("%s: %d %v, want %d %s", tc.name, status, got, tc.status, tc.code)
				}
			}
		})
	}
}

func TestTaskLifecycleActions(t *testing.T) {
	h, st := lifecycleHandler(t)
	operator := cfaccess.Identity{Email: "operator@example.com"}
	laneA, laneB := "lifecycle-a-"+t.Name(), "lifecycle-b-"+t.Name()
	lifecycleSeed(t, st, laneB, store.StateBacklog, "implement")
	target := lifecycleSeed(t, st, laneA, store.StateBacklog, "implement")

	cases := []struct {
		name   string
		seed   func() store.Task
		action string
		body   func(x store.Task) string
		fields map[string]any
		replay bool
	}{
		{
			name:   "close-merged",
			seed:   func() store.Task { return lifecycleSeed(t, st, laneA, store.StateDeveloping, "implement") },
			action: "close-merged",
			body: func(store.Task) string {
				return `{"evidence":"https://example.com/pr/2","reason":"머지 완료","event_key":"k-merge"}`
			},
			fields: map[string]any{"state": "merged"},
		},
		{
			name:   "close-completed",
			seed:   func() store.Task { return lifecycleSeed(t, st, laneA, store.StateReviewing, "ops") },
			action: "close-completed",
			body: func(store.Task) string {
				return `{"evidence":"https://example.com/run/2","reason":"완료","event_key":"k-complete"}`
			},
			fields: map[string]any{"state": "completed"},
		},
		{
			name:   "drop",
			seed:   func() store.Task { return lifecycleSeed(t, st, laneA, store.StateDeveloping, "implement") },
			action: "drop",
			body: func(store.Task) string {
				return `{"closure":"obsolete","reason":"더 이상 필요 없음","event_key":"k-drop"}`
			},
			fields: map[string]any{"state": "dropped", "closure_type": "obsolete"},
		},
		{
			name:   "duplicate",
			seed:   func() store.Task { return lifecycleSeed(t, st, laneA, store.StateDeveloping, "implement") },
			action: "drop",
			body: func(store.Task) string {
				return fmt.Sprintf(`{"closure":"superseded","reason":"대체됨","duplicate_of":%d,"event_key":"k-dup"}`, target.ID)
			},
			fields: map[string]any{"state": "duplicate", "closure_type": "superseded"},
		},
		{
			name:   "hold",
			seed:   func() store.Task { return lifecycleSeed(t, st, laneA, store.StateBacklog, "implement") },
			action: "hold",
			body:   func(store.Task) string { return `{"held":true,"event_key":"k-hold"}` },
			fields: map[string]any{"previous": "backlog", "state": "icebox"},
		},
		{
			name:   "reopen",
			seed:   func() store.Task { return lifecycleSeed(t, st, laneA, store.StateMerged, "implement") },
			action: "reopen",
			body:   func(store.Task) string { return `{"reason":"다시 열기","event_key":"k-reopen"}` },
			fields: map[string]any{"state": "unstarted"},
		},
		{
			name:   "relane",
			seed:   func() store.Task { return lifecycleSeed(t, st, laneA, store.StateBacklog, "implement") },
			action: "relane",
			body: func(store.Task) string {
				return fmt.Sprintf(`{"lane":%q,"reason":"레인 이동","event_key":"k-relane"}`, laneB)
			},
			fields: map[string]any{"previous": laneA, "lane": laneB, "relay": "unconfigured"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := tc.seed()
			path := fmt.Sprintf("/ui/api/tasks/%d/%s", x.ID, tc.action)
			status, first := h.lifecycleRequest(operator, lifecycleCall{path: path, body: tc.body(x), csrf: true})
			if status != http.StatusOK {
				t.Fatalf("first: %d %v", status, first)
			}
			for key, want := range tc.fields {
				if first[key] != want {
					t.Fatalf("%s=%v, want %v (body %v)", key, first[key], want, first)
				}
			}
			status, replay := h.lifecycleRequest(operator, lifecycleCall{path: path, body: tc.body(x), csrf: true})
			if status != http.StatusOK {
				t.Fatalf("replay: %d %v", status, replay)
			}
			if fmt.Sprint(replay) != fmt.Sprint(first) {
				t.Fatalf("replay=%v, first=%v", replay, first)
			}
		})
	}
}

func TestTaskLifecycleRefusals(t *testing.T) {
	h, st := lifecycleHandler(t)
	operator := cfaccess.Identity{Email: "operator@example.com"}
	lane := "lifecycle-refuse"
	for _, tc := range []struct {
		name   string
		seed   func() store.Task
		action string
		body   string
		status int
		code   string
	}{
		{"merge-from-backlog", func() store.Task { return lifecycleSeed(t, st, lane, store.StateBacklog, "implement") }, "close-merged", `{"evidence":"https://example.com/pr/3","event_key":"r1"}`, 409, "task_merge_path"},
		{"completed-wrong-kind", func() store.Task { return lifecycleSeed(t, st, lane, store.StateReviewing, "implement") }, "close-completed", `{"evidence":"https://example.com/run/3","event_key":"r2"}`, 409, "task_conflict"},
		{"completed-bad-evidence", func() store.Task { return lifecycleSeed(t, st, lane, store.StateReviewing, "ops") }, "close-completed", `{"evidence":"not-a-link","event_key":"r3"}`, 400, "closure_invalid"},
		{"drop-bad-closure", func() store.Task { return lifecycleSeed(t, st, lane, store.StateDeveloping, "implement") }, "drop", `{"closure":"resolved","reason":"x","event_key":"r4"}`, 400, "invalid_request"},
		{"drop-no-duplicate-of", func() store.Task { return lifecycleSeed(t, st, lane, store.StateDeveloping, "implement") }, "drop", `{"closure":"superseded","reason":"x","event_key":"r5"}`, 400, "duplicate_of_required"},
		{"release-not-held", func() store.Task { return lifecycleSeed(t, st, lane, store.StateBacklog, "implement") }, "hold", `{"held":false,"event_key":"r6"}`, 409, "task_conflict"},
		{"reopen-open-task", func() store.Task { return lifecycleSeed(t, st, lane, store.StateBacklog, "implement") }, "reopen", `{"reason":"x","event_key":"r7"}`, 409, "task_conflict"},
		{"reopen-without-reason", func() store.Task { return lifecycleSeed(t, st, lane, store.StateMerged, "implement") }, "reopen", `{"reason":"   ","event_key":"r8"}`, 400, "invalid_request"},
		{"relane-unknown-lane", func() store.Task { return lifecycleSeed(t, st, lane, store.StateBacklog, "implement") }, "relane", `{"lane":"no-such-lane","event_key":"r9"}`, 400, "unknown_lane"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := tc.seed()
			path := fmt.Sprintf("/ui/api/tasks/%d/%s", x.ID, tc.action)
			status, got := h.lifecycleRequest(operator, lifecycleCall{path: path, body: tc.body, csrf: true})
			if status != tc.status || got["error"] != tc.code {
				t.Fatalf("%d %v, want %d %s", status, got, tc.status, tc.code)
			}
		})
	}
}

func TestTaskLifecycleTimelineRows(t *testing.T) {
	h, st := lifecycleHandler(t)
	operator := cfaccess.Identity{Email: "operator@example.com"}
	lane := "lifecycle-timeline"
	lifecycleSeed(t, st, lane+"-target", store.StateBacklog, "implement")
	x := lifecycleSeed(t, st, lane, store.StateDeveloping, "implement")
	path := fmt.Sprintf("/ui/api/tasks/%d/close-merged", x.ID)
	status, got := h.lifecycleRequest(operator, lifecycleCall{path: path, body: `{"evidence":"https://example.com/pr/4","reason":"머지","event_key":"t1"}`, csrf: true})
	if status != http.StatusOK {
		t.Fatalf("close: %d %v", status, got)
	}
	r := httptest.NewRequest(http.MethodGet, fmt.Sprintf("http://example.com/ui/api/board/tasks/%d", x.ID), nil)
	w := httptest.NewRecorder()
	h.serveAPI(w, r, operator)
	var detail struct {
		Events []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || w.Code != http.StatusOK {
		t.Fatalf("detail: %d %s %v", w.Code, w.Body.String(), err)
	}
	want := [][2]string{{"developing", "reviewing"}, {"reviewing", "merged"}}
	found := 0
	for _, event := range detail.Events {
		if found < len(want) && event.From == want[found][0] && event.To == want[found][1] {
			found++
		}
	}
	if found != len(want) {
		t.Fatalf("timeline rows %v absent from %+v", want, detail.Events)
	}
}

// TestRelaneUndoRestoresEmptiedLane covers the route's from_key: the undo of
// a move that emptied its source lane may restore that recorded lane, while
// the same target without a matching recorded move stays unknown.
func TestRelaneUndoRestoresEmptiedLane(t *testing.T) {
	h, st := lifecycleHandler(t)
	operator := cfaccess.Identity{Email: "operator@example.com"}
	from, to := "ui-undo-from-"+t.Name(), "ui-undo-to-"+t.Name()
	lifecycleSeed(t, st, to, store.StateBacklog, "implement")
	project := "experiment"
	x, err := st.CreateTask(context.Background(), store.Task{Lane: from, Title: "sole member", Kind: "implement", Priority: 3, CreatedBy: "seed", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/ui/api/tasks/%d/relane", x.ID)
	status, first := h.lifecycleRequest(operator, lifecycleCall{path: path, body: fmt.Sprintf(`{"lane":%q,"reason":"move","event_key":"ui-undo-1"}`, to), csrf: true})
	if status != http.StatusOK || first["lane"] != to {
		t.Fatalf("forward: %d %v", status, first)
	}
	status, undo := h.lifecycleRequest(operator, lifecycleCall{path: path, body: fmt.Sprintf(`{"lane":%q,"reason":"undo","event_key":"ui-undo-2","from_key":"ui-undo-1"}`, from), csrf: true})
	if status != http.StatusOK || undo["lane"] != from || undo["previous"] != to {
		t.Fatalf("undo: %d %v", status, undo)
	}
	// Move out again; the emptied source lane is unknown to a write with no
	// matching recorded move, whether the key is absent or names another move.
	if status, got := h.lifecycleRequest(operator, lifecycleCall{path: path, body: fmt.Sprintf(`{"lane":%q,"reason":"move","event_key":"ui-undo-3"}`, to), csrf: true}); status != http.StatusOK {
		t.Fatalf("second move: %d %v", status, got)
	}
	for _, body := range []string{
		fmt.Sprintf(`{"lane":%q,"reason":"no restore","event_key":"ui-undo-4"}`, from),
		fmt.Sprintf(`{"lane":%q,"reason":"wrong restore","event_key":"ui-undo-5","from_key":"bogus"}`, from),
	} {
		status, got := h.lifecycleRequest(operator, lifecycleCall{path: path, body: body, csrf: true})
		if status != http.StatusBadRequest || got["error"] != "unknown_lane" {
			t.Fatalf("unknown lane: %d %v", status, got)
		}
	}
}

func TestBoardLanesListsKnownLanes(t *testing.T) {
	h, st := lifecycleHandler(t)
	operator := cfaccess.Identity{Email: "operator@example.com"}
	first, second := "lanes-alpha", "lanes-beta"
	lifecycleSeed(t, st, first, store.StateBacklog, "implement")
	lifecycleSeed(t, st, second, store.StateBacklog, "implement")
	r := httptest.NewRequest(http.MethodGet, "http://example.com/ui/api/board/lanes", nil)
	w := httptest.NewRecorder()
	h.serveAPI(w, r, operator)
	if w.Code != http.StatusOK {
		t.Fatalf("lanes: %d %s", w.Code, w.Body.String())
	}
	var response struct {
		Lanes []string `json:"lanes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, lane := range response.Lanes {
		seen[lane] = true
	}
	if !seen[first] || !seen[second] {
		t.Fatalf("lanes=%v missing %q or %q", response.Lanes, first, second)
	}
}
