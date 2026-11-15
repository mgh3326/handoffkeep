package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// fakeTaskServer answers every task route with the given task JSON.
func fakeTaskServer(t *testing.T, task store.Task) Client {
	t.Helper()
	resetCapabilitiesCache()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerCurrentCapabilities(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(task)
	}))
	t.Cleanup(server.Close)
	return Client{URL: server.URL, Token: "t"}
}

// TestTransitionJudgesResponseInEitherVocabulary: the target the caller asked
// for, in the old or the new name, is checked against whatever vocabulary the
// server answers in.
func TestTransitionJudgesResponseInEitherVocabulary(t *testing.T) {
	ctx := context.Background()
	linked := store.TaskRefs{JobID: "job-1"}
	cases := []struct {
		name string
		to   string
		resp store.Task
		ok   bool
	}{
		// current names
		{"developing", "developing", store.Task{State: "developing", Category: "started", ClaimedBy: "w", Refs: linked}, true},
		{"reviewing", "reviewing", store.Task{State: "reviewing", Category: "started"}, true},
		{"icebox", "icebox", store.Task{State: "icebox", Category: "canceled"}, true},
		{"merged response after deploy", store.StateMerged, store.Task{State: store.StateDeployed, Category: "completed"}, true},
		{"merged response does not accept unrelated state", store.StateMerged, store.Task{State: store.StateDropped, Category: "completed"}, false},
		{"wrong state", "reviewing", store.Task{State: "developing", Category: "started"}, false},
		// old names answered by a current server
		{"in_progress -> developing", "in_progress", store.Task{State: "developing", Category: "started", ClaimedBy: "w", Refs: linked}, true},
		{"in_progress -> researching", "in_progress", store.Task{State: "researching", Category: "started", ClaimedBy: "w", Refs: linked}, true},
		{"claimed clears the flag", "claimed", store.Task{State: "backlog", Category: "backlog"}, true},
		{"claimed leaves the flag", "claimed", store.Task{State: "backlog", Category: "backlog", AwaitingDecision: true}, false},
		{"verifying -> reviewing", "verifying", store.Task{State: "reviewing", Category: "started"}, true},
		{"join -> reviewing", "join", store.Task{State: "reviewing", Category: "started"}, true},
		{"hold -> icebox", "hold", store.Task{State: "icebox", Category: "canceled"}, true},
		{"needs_decision sets the flag", "needs_decision", store.Task{State: "developing", Category: "started", AwaitingDecision: true}, true},
		{"needs_decision without the flag", "needs_decision", store.Task{State: "developing", Category: "started"}, false},
		{"reopened -> unstarted", "reopened", store.Task{State: "unstarted", Category: "unstarted"}, true},
		// old names answered by a server that predates the model
		{"old server in_progress", "in_progress", store.Task{State: "in_progress", ClaimedBy: "w", Refs: linked}, true},
		{"old server claimed", "claimed", store.Task{State: "claimed", ClaimedBy: "w", Refs: linked}, true},
		{"old server hold", "hold", store.Task{State: "hold"}, true},
		{"old server needs_decision", "needs_decision", store.Task{State: "needs_decision"}, true},
		{"old server verifying", "verifying", store.Task{State: "verifying"}, true},
	}
	for _, c := range cases {
		client := fakeTaskServer(t, c.resp)
		_, err := client.TransitionTask(ctx, 1, c.to, "note", nil, "")
		if (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%t", c.name, err, c.ok)
		}
	}
	// Entering an active state still needs the accountable pair in the response.
	bare := fakeTaskServer(t, store.Task{State: "developing", Category: "started"})
	if _, err := bare.TransitionTask(ctx, 1, "developing", "note", nil, ""); err == nil || !strings.Contains(err.Error(), "transition_linkage_missing") {
		t.Errorf("unlinked developing: err=%v, want transition_linkage_missing", err)
	}
	if _, err := bare.TransitionTask(ctx, 1, "developing", "note", nil, "no job: operator started it"); err != nil {
		t.Errorf("unlinked developing with a no-job reason: %v", err)
	}
}

func TestTaskCategoryAndHierarchyFieldsDecode(t *testing.T) {
	parent := int64(3)
	c := fakeTaskServer(t, store.Task{ID: 9, State: "reviewing", Category: "started", AwaitingDecision: true, ParentID: &parent,
		Children:  []store.TaskChild{{ID: 10, Title: "child", State: "backlog", Category: "backlog"}},
		Relations: []store.TaskRelation{{ID: 1, Kind: "blocks", SrcID: 9, DstID: 11}}})
	got, found, err := c.GetTask(context.Background(), 9)
	if err != nil || !found || got.Category != "started" || !got.AwaitingDecision || got.ParentID == nil || *got.ParentID != 3 || len(got.Children) != 1 || len(got.Relations) != 1 {
		t.Fatalf("decoded task: %+v found=%t err=%v", got, found, err)
	}
	out, err := c.SetTaskParent(context.Background(), 9, &parent)
	if err != nil || out.ParentID == nil {
		t.Fatalf("set parent: %+v err=%v", out, err)
	}
	// A server that ignores the route's body answers the old row: not applied.
	stale := fakeTaskServer(t, store.Task{ID: 9, State: "backlog"})
	if _, err := stale.SetTaskParent(context.Background(), 9, &parent); err == nil || !strings.Contains(err.Error(), "parent_not_applied") {
		t.Fatalf("stale server: err=%v", err)
	}
}
