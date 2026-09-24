package ui

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// A relane event's to is a lane name, not a state: if it entered dwell, the
// open segment would be credited to a lane and the real state's time lost.
func TestTaskDwellSkipsRelaneEvents(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	task := store.Task{
		CreatedAt: t0,
		Events: []store.TaskEvent{
			{Kind: store.TaskEventTransition, From: "backlog", To: "claimed", At: t0.Add(time.Hour)},
			{Kind: store.TaskEventRelane, From: "admiral-1", To: "director-1", At: t0.Add(2 * time.Hour)},
		},
	}
	dwell := taskDwell(task, t0.Add(4*time.Hour))
	var backlog, claimed *dwellSegment
	for i := range dwell {
		switch dwell[i].State {
		case "backlog":
			backlog = &dwell[i]
		case "claimed":
			claimed = &dwell[i]
		default:
			t.Fatalf("lane leaked into dwell states: %+v", dwell[i])
		}
	}
	if backlog == nil || backlog.Seconds != 3600 || backlog.Open {
		t.Fatalf("backlog segment=%+v", backlog)
	}
	if claimed == nil || claimed.Seconds != 3*3600 || !claimed.Open {
		t.Fatalf("claimed segment=%+v (want 3h open — the relane must not close it)", claimed)
	}
}
