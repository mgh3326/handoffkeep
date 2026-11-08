package store

import (
	"errors"
	"testing"
)

// A reopen clears the dots claim. The run it ends must stop posting at once,
// and a fresh START after it must issue a token without meeting the old one.
func TestDotsReopenEndsTheRun(t *testing.T) {
	s := schemaStore(t, "dots_reopen")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := startDotsRun(t, s, task.ID, "start-1")
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "Done", IdempotencyKey: "finish", RunToken: first.RunToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.transitionTask(ctx, task.ID, TransitionInput{To: StateUnstarted, By: "operator:test@example.com", Note: "reopened for a second look"}, true); err != nil {
		t.Fatal(err)
	}
	current, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found || current.ClaimedBy != "" {
		t.Fatalf("after reopen claimed_by=%q found=%v err=%v", current.ClaimedBy, found, err)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "PROGRESS", Body: "Old run", IdempotencyKey: "old-progress", RunToken: first.RunToken}); !errors.Is(err, ErrDotsRunTokenMismatch) {
		t.Fatalf("old run after reopen: %v", err)
	}
	second, _ := startDotsRun(t, s, task.ID, "start-2")
	if second.RunToken == first.RunToken {
		t.Fatal("the new run reused the old token")
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "PROGRESS", Body: "New run", IdempotencyKey: "new-progress", RunToken: second.RunToken}); err != nil {
		t.Fatalf("new run: %v", err)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "PROGRESS", Body: "Old run again", IdempotencyKey: "old-again", RunToken: first.RunToken}); !errors.Is(err, ErrDotsRunTokenMismatch) {
		t.Fatalf("old run after restart: %v", err)
	}
	var active int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM dots_run_tokens WHERE task_id=$1 AND superseded_at IS NULL`, task.ID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active tokens=%d err=%v", active, err)
	}
}
