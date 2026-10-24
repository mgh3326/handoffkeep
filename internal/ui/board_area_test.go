package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The board row always carries the area key: the area name when tagged,
// null when not. The console reads it as a read-only label.
func TestBoardTaskCarriesArea(t *testing.T) {
	area := "go-backend"
	tagged, err := json.Marshal(projectBoardTask(store.Task{ID: 1, Area: &area}))
	if err != nil || !strings.Contains(string(tagged), `"area":"go-backend"`) {
		t.Fatalf("tagged board task=%s err=%v", tagged, err)
	}
	untagged, err := json.Marshal(projectBoardTask(store.Task{ID: 2}))
	if err != nil || !strings.Contains(string(untagged), `"area":null`) {
		t.Fatalf("untagged board task=%s err=%v", untagged, err)
	}
}

// An area event's to is an area name, not a state: if it entered dwell, the
// open segment would be credited to an area and the real state's time lost.
func TestTaskDwellSkipsAreaEvents(t *testing.T) {
	t0 := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	task := store.Task{
		CreatedAt: t0,
		Events: []store.TaskEvent{
			{Kind: store.TaskEventTransition, From: "backlog", To: "claimed", At: t0.Add(time.Hour)},
			{Kind: store.TaskEventArea, From: "", To: "blender", At: t0.Add(2 * time.Hour)},
		},
	}
	for _, segment := range taskDwell(task, t0.Add(4*time.Hour)) {
		if segment.State == "blender" {
			t.Fatalf("area leaked into dwell states: %+v", segment)
		}
		if segment.State == "claimed" && segment.Seconds != int64(3*time.Hour/time.Second) {
			t.Fatalf("claimed dwell=%d, want 3h unbroken by the area event", segment.Seconds)
		}
	}
}
