package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestTaskJSONLastActivity(t *testing.T) {
	h, s := decisionAPIServer(t)
	x, err := s.CreateTask(t.Context(), store.Task{Lane: "activity", Title: "activity", Kind: "implement", CreatedBy: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if status := decisionAPICall(t, h, http.MethodGet, fmt.Sprintf("/v1/tasks/%d", x.ID), nil, &body); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	value, ok := body["last_activity_at"].(string)
	if !ok {
		t.Fatalf("last_activity_at=%v", body["last_activity_at"])
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		t.Fatal(err)
	}
}
