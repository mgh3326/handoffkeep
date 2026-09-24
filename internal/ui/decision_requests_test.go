package ui

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The queue computes its badge and count in the browser from list refs; the
// Decisions page and glance compute theirs here. Both sides read this one
// fixture (web/console/src/queue-proto/decision.test.tsx is the other half),
// so the two implementations of the split cannot drift apart silently.
func TestDecisionMarksFixtureMatchesServerCounts(t *testing.T) {
	raw, err := os.ReadFile("testdata/decision_marks.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Tasks []store.Task `json:"tasks"`
		Want  struct {
			Pending   int `json:"pending"`
			Uncleaned int `json:"uncleaned"`
		} `json:"want"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	pending, uncleaned := store.DecisionRequestCounts(fixture.Tasks)
	if pending != fixture.Want.Pending || uncleaned != fixture.Want.Uncleaned {
		t.Fatalf("server split %d/%d, fixture %d/%d", pending, uncleaned, fixture.Want.Pending, fixture.Want.Uncleaned)
	}
}

// Display states never read a passed deadline as an application, and an
// open request on a terminal task is uncleaned rather than answered.
func TestDecisionViewStates(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	open := store.DecisionRequest{ID: "dr-1-1", Revision: 1, Status: store.DecisionRequestOpen, DefaultAction: "보류"}
	cases := []struct {
		name      string
		request   store.DecisionRequest
		taskState string
		want      string
	}{
		{"no deadline", open, "in_progress", store.DecisionViewOpen},
		{"future deadline", func() store.DecisionRequest { r := open; r.DueAt = &future; return r }(), "backlog", store.DecisionViewOpen},
		{"past deadline", func() store.DecisionRequest { r := open; r.DueAt = &past; return r }(), "in_progress", store.DecisionViewOverdue},
		{"merged open", open, "merged", store.DecisionViewUncleaned},
		{"dropped past deadline", func() store.DecisionRequest { r := open; r.DueAt = &past; return r }(), "dropped", store.DecisionViewUncleaned},
		{"applied with receipt", func() store.DecisionRequest {
			r := open
			r.Status = store.DecisionRequestDefaultApplied
			r.Resolution = &store.DecisionResolution{Kind: store.DecisionRequestDefaultApplied, Receipt: "hk:doc r"}
			return r
		}(), "in_progress", store.DecisionViewDefaultApplied},
		{"answered on merged", func() store.DecisionRequest {
			r := open
			r.Status = store.DecisionRequestAnswered
			r.Resolution = &store.DecisionResolution{Kind: store.DecisionRequestAnswered, Option: "A"}
			return r
		}(), "merged", store.DecisionViewAnswered},
	}
	for _, c := range cases {
		view := projectDecisionRequest(store.DecisionRequestEntry{Request: c.request, Current: true}, c.taskState, now)
		if view.State != c.want || view.StateLabel == "" {
			t.Errorf("%s: state=%s label=%q want %s", c.name, view.State, view.StateLabel, c.want)
		}
		if view.State != store.DecisionViewDefaultApplied && view.StateLabel == decisionStateLabels[store.DecisionViewDefaultApplied] {
			t.Errorf("%s: reads as applied", c.name)
		}
	}
}
