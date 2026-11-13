package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func decisionHTTPInput(question string, humanOnly bool) store.DecisionRequestInput {
	return store.DecisionRequestInput{
		Question: question,
		Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{
			{Key: "A", Label: "first", Recommended: true},
			{Key: "B", Label: "second"},
		}},
		DefaultAction: "자동 적용 없음",
		HumanOnly:     humanOnly,
	}
}

// TestDecisionResolveHTTP covers the task decision resolve bearer route.
func TestDecisionResolveHTTP(t *testing.T) {
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "decision_resolve_api")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"worker": "worker-tok"}}.Handler())
	t.Cleanup(h.Close)

	task, err := st.CreateTask(ctx, store.Task{Lane: fmt.Sprintf("dr-http-%d", time.Now().UnixNano()), Title: "decision http", Kind: "implement", CreatedBy: "dr-http", Project: &apiTestProject})
	if err != nil {
		t.Fatal(err)
	}
	if task, err = st.ClaimTask(ctx, task.ID, "dr-http", "job-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionTask(ctx, task.ID, "in_progress", "dr-http", "step", nil, ""); err != nil {
		t.Fatal(err)
	}
	request, err := st.RecordDecisionRequest(ctx, task.ID, "dr-http", decisionHTTPInput("decision", false))
	if err != nil {
		t.Fatal(err)
	}
	call := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1/tasks/%d/decision-request/resolve", h.URL, task.ID), bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer worker-tok")
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}
	body := map[string]any{"request_id": request.Request.ID, "option": "A", "kind": "answered"}
	status, result := call(body)
	if status != http.StatusOK || result["result"] != "answered" || result["duplicate"] != false {
		t.Fatalf("first resolve status=%d body=%v", status, result)
	}
	status, result = call(body)
	if status != http.StatusOK || result["result"] != "answered" || result["duplicate"] != true {
		t.Fatalf("repeat resolve status=%d body=%v", status, result)
	}
	status, result = call(map[string]any{"request_id": request.Request.ID, "option": "not-an-option", "kind": "answered"})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid option status=%d body=%v", status, result)
	}
}
