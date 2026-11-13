package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestTaskPriorityAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	x, err := st.CreateTask(context.Background(), store.Task{
		Lane:      "d",
		Title:     "prio task",
		Kind:      "implement",
		CreatedBy: "test",
		Project:   projectStr("experiment"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Change priority on non-terminal task
	code, out := projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/priority", x.ID), "tok", map[string]any{
		"priority": 5,
		"note":     "raise priority",
	})
	if code != http.StatusOK || int(out["priority"].(float64)) != 5 {
		t.Fatalf("set priority: code=%d body=%v", code, out)
	}

	// Verify event was recorded
	got, _, err := st.GetTask(context.Background(), x.ID)
	if err != nil || got.Priority != 5 {
		t.Fatalf("task priority=%d err=%v", got.Priority, err)
	}
	events, err := st.ListTaskEvents(context.Background(), x.ID, 0, 100)
	if err != nil || len(events) == 0 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	last := events[len(events)-1]
	if last.Kind != "metadata" || last.Field != "priority" || last.ToValue != "5" {
		t.Fatalf("last event=%+v", last)
	}

	// Invalid priority is rejected with 400
	code, _ = projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/priority", x.ID), "tok", map[string]any{
		"priority": -1,
	})
	if code != http.StatusBadRequest {
		t.Fatalf("negative priority code=%d, want 400", code)
	}

	// Terminal task rejects priority change with 409
	if _, err = st.TransitionTask(context.Background(), x.ID, "completed", "test", "done", nil, ""); err != nil {
		t.Fatal(err)
	}
	code, out = projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/priority", x.ID), "tok", map[string]any{
		"priority": 1,
	})
	if code != http.StatusConflict || out["error"] != "task_terminal" {
		t.Fatalf("terminal task priority change: code=%d body=%v, want 409 task_terminal", code, out)
	}
}
