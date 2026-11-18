package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestTaskCloseRuleAPI(t *testing.T) {
	h, st := relaneAPIServer(t)
	x, err := st.CreateTask(context.Background(), store.Task{
		Lane:      "d",
		Title:     "programme task",
		Kind:      "implement",
		CreatedBy: "test",
		Project:   projectStr("experiment"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if x.CloseRule != store.CloseRuleAuto {
		t.Fatalf("new task close_rule=%q, want auto", x.CloseRule)
	}

	code, out := projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/close-rule", x.ID), "tok", map[string]any{
		"close_rule": "explicit",
		"note":       "programme task",
	})
	if code != http.StatusOK || out["close_rule"] != "explicit" {
		t.Fatalf("set explicit: code=%d body=%v", code, out)
	}

	got, _, err := st.GetTask(context.Background(), x.ID)
	if err != nil || got.CloseRule != store.CloseRuleExplicit {
		t.Fatalf("task close_rule=%q err=%v", got.CloseRule, err)
	}
	events, err := st.ListTaskEvents(context.Background(), x.ID, 0, 100)
	if err != nil || len(events) == 0 {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	last := events[len(events)-1]
	if last.Kind != "metadata" || last.Field != "close_rule" || last.ToValue != store.CloseRuleExplicit {
		t.Fatalf("last event=%+v", last)
	}

	code, out = projectRequest(t, h.URL, http.MethodPost, fmt.Sprintf("/v1/tasks/%d/close-rule", x.ID), "tok", map[string]any{
		"close_rule": "sometimes",
	})
	if code != http.StatusBadRequest || out["error"] != "invalid_close_rule" {
		t.Fatalf("unknown rule: code=%d body=%v, want 400 invalid_close_rule", code, out)
	}
}
