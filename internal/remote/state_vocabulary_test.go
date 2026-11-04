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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(task)
	}))
	t.Cleanup(server.Close)
	return Client{URL: server.URL, Token: "t"}
}

// TestClaimAcceptsBothVocabularies: a claim response in developing or
// researching (a current server) is accepted, and so are claimed and
// in_progress (a server that predates the eleven-state model).
func TestClaimAcceptsBothVocabularies(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"developing", "researching", "reviewing", "claimed", "in_progress"} {
		c := fakeTaskServer(t, store.Task{ID: 7, State: state, ClaimedBy: "worker", Refs: store.TaskRefs{JobID: "job-1"}})
		got, err := c.ClaimTask(ctx, 7, "worker", "job-1", "")
		if err != nil || got.State != state {
			t.Errorf("claim response %q: %+v err=%v", state, got, err)
		}
		if got, err := c.NextTask(ctx, "lane", "worker", "job-1", ""); state != "reviewing" && (err != nil || got.State != state) {
			t.Errorf("next response %q: %+v err=%v", state, got, err)
		}
		if _, err := c.RebindTaskJob(ctx, 7, "job-0", "job-1", "worker", "takeover"); err != nil {
			t.Errorf("rebind response %q: %v", state, err)
		}
	}
	// A response that is not a started state is still a failed claim.
	for _, state := range []string{"backlog", "unstarted", "triage", "merged", "icebox", ""} {
		c := fakeTaskServer(t, store.Task{ID: 7, State: state, ClaimedBy: "worker", Refs: store.TaskRefs{JobID: "job-1"}})
		if _, err := c.ClaimTask(ctx, 7, "worker", "job-1", ""); err == nil || !strings.Contains(err.Error(), "claim_not_applied") {
			t.Errorf("claim response %q: err=%v, want claim_not_applied", state, err)
		}
		if _, err := c.NextTask(ctx, "lane", "worker", "job-1", ""); err == nil || !strings.Contains(err.Error(), "next_not_applied") {
			t.Errorf("next response %q: err=%v, want next_not_applied", state, err)
		}
	}
	// The claimant and the job link are still checked.
	wrong := fakeTaskServer(t, store.Task{ID: 7, State: "developing", ClaimedBy: "someone-else", Refs: store.TaskRefs{JobID: "job-1"}})
	if _, err := wrong.ClaimTask(ctx, 7, "worker", "job-1", ""); err == nil || !strings.Contains(err.Error(), "claim_claimant_not_recorded") {
		t.Errorf("wrong claimant: err=%v", err)
	}
	unlinked := fakeTaskServer(t, store.Task{ID: 7, State: "developing", ClaimedBy: "worker"})
	if _, err := unlinked.ClaimTask(ctx, 7, "worker", "job-1", ""); err == nil || !strings.Contains(err.Error(), "claim_job_id_not_recorded") {
		t.Errorf("missing job link: err=%v", err)
	}
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
