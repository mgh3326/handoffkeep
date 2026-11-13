package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestDotsAnswerAPI(t *testing.T) {
	_, url := testdb.Schema(t, "dots_answer_api")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Tokens: Tokens{"caller": "test-token"}, Service: Service{Store: st}}.Handler())
	defer h.Close()

	// Create task
	task, err := st.CreateDotsRequest(t.Context(), "caller", store.DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Answer with no open request -> 409 no_open_decision_request
	code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/dots/answer", "test-token", map[string]any{"task": task.ID, "text": "answer"})
	if code != http.StatusConflict || out["error"] != "no_open_decision_request" {
		t.Fatalf("expected 409 no_open_decision_request, got %d %v", code, out)
	}

	// Open dots QUESTION
	started, err := st.ReplyDots(t.Context(), "dots", "chatgpt", store.DotsReplyInput{
		TaskID:            task.ID,
		Kind:              "START",
		Body:              "Starting",
		ExpectedUpdatedAt: &task.UpdatedAt,
		IdempotencyKey:    "start",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplyDots(t.Context(), "dots", "chatgpt", store.DotsReplyInput{
		TaskID:         task.ID,
		Kind:           "QUESTION",
		Body:           "Which option?",
		AskOperator:    true,
		IdempotencyKey: "question",
		RunToken:       started.RunToken,
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Answer open question -> 200 OK
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/dots/answer", "test-token", map[string]any{"task": task.ID, "text": "Option B fits"})
	if code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d %v", code, out)
	}
	taskMap, _ := out["task"].(map[string]any)
	if taskMap["awaiting_decision"] != false {
		t.Fatalf("expected awaiting_decision false, got %v", taskMap["awaiting_decision"])
	}
	reqMap, _ := out["request"].(map[string]any)
	if reqMap["status"] != "answered" {
		t.Fatalf("expected request status answered, got %v", reqMap["status"])
	}

	// 3. Second answer -> 409 decision_request_resolved
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/dots/answer", "test-token", map[string]any{"task": task.ID, "text": "Another answer"})
	if code != http.StatusConflict || out["error"] != "decision_request_resolved" {
		t.Fatalf("expected 409 decision_request_resolved, got %d %v", code, out)
	}
}
