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

	// 1. Answer with no open question and no open decision request -> 409 with its reason
	code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/dots/answer", "test-token", map[string]any{"task": task.ID, "text": "answer"})
	if code != http.StatusConflict || out["error"] != "no_open_dots_question" || out["reason"] != "this request has no open dots question or decision request to answer" {
		t.Fatalf("expected 409 no_open_dots_question with reason, got %d %v", code, out)
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

// Invariant (operator decision 364, hk 5000): a plain QUESTION reply is
// answered over the same route by the request's requester lane; the answer
// closes it once, and another lane is refused with the rule.
func TestDotsAnswerPlainQuestionAPI(t *testing.T) {
	_, url := testdb.Schema(t, "dots_answer_plain_api")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Tokens: Tokens{"cli": "test-token"}, Service: Service{Store: st}}.Handler())
	defer h.Close()
	ctx := t.Context()

	task, err := st.CreateDotsRequest(ctx, "cli", store.DotsRequestInput{Title: "Vehicles", Body: "Make six vehicles", RequesterLane: "requesting-lane"})
	if err != nil {
		t.Fatal(err)
	}
	revision := task.UpdatedAt
	start, err := st.ReplyDots(ctx, "operator:test@example.com", "chatgpt", store.DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Starting", IdempotencyKey: "start", ExpectedUpdatedAt: &revision})
	if err != nil {
		t.Fatalf("start=%+v err=%v", start, err)
	}
	if _, err := st.ReplyDots(ctx, "operator:test@example.com", "chatgpt", store.DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: "Which first?", IdempotencyKey: "question", RunToken: start.RunToken}); err != nil {
		t.Fatal(err)
	}

	// Another lane is refused with the rule, not with its own input.
	code, out := projectRequest(t, h.URL, http.MethodPost, "/v1/dots/answer", "test-token", map[string]any{"task": task.ID, "text": "the van", "requester_lane": "intruder-lane"})
	if code != http.StatusConflict || out["error"] != "task_conflict" {
		t.Fatalf("intruder status=%d body=%v", code, out)
	}
	if reason, _ := out["reason"].(string); reason != "only the request's requester lane may answer its question" {
		t.Fatalf("intruder reason=%q (body=%v)", reason, out)
	}

	// The requester answers the plain question.
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/dots/answer", "test-token", map[string]any{"task": task.ID, "text": "the van", "requester_lane": "requesting-lane"})
	if code != http.StatusOK {
		t.Fatalf("answer status=%d body=%v", code, out)
	}
	if _, ok := out["request"]; ok {
		t.Fatalf("plain answer carried a decision request: %v", out)
	}
	question, _ := out["question"].(map[string]any)
	if question == nil || question["comment_id"] == nil {
		t.Fatalf("plain answer did not name the question: %v", out)
	}

	// Answered once: a second answer names what is missing.
	code, out = projectRequest(t, h.URL, http.MethodPost, "/v1/dots/answer", "test-token", map[string]any{"task": task.ID, "text": "again", "requester_lane": "requesting-lane"})
	if code != http.StatusConflict || out["error"] != "no_open_dots_question" || out["reason"] != "this request has no open dots question or decision request to answer" {
		t.Fatalf("second answer status=%d body=%v", code, out)
	}
}
