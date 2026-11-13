package store

import (
	"context"
	"testing"
)

// Invariant: an MCP answer is recorded with the channel it came through, and
// its mcp_write_audit row is written in the answer's transaction. A keyed
// replay of the same answer writes no second row.
func TestDecisionAnswerMCPChannelAndAudit(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	write := MCPWrite{Tool: "decision_answer", Actor: "bearer:test", Channel: "bearer", Author: "bearer:test", Key: "answer-1"}
	in := answerA(request, DecisionChannelMCP, "bearer:test")
	in.EventKey, in.Audit = MCPDecisionEventKey(write.Actor, write.Key), &write

	first, err := s.ResolveDecision(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	var channel string
	if err := s.pool.QueryRow(ctx, `SELECT channel FROM decision_answer_attempts WHERE id=$1`, first.AttemptID).Scan(&channel); err != nil || channel != DecisionChannelMCP {
		t.Fatalf("attempt channel=%q err=%v", channel, err)
	}
	count := func() int {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM mcp_write_audit WHERE tool=$1 AND actor=$2 AND idempotency_key=$3`, write.Tool, write.Actor, write.Key).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("audit rows=%d after the answer, want 1", n)
	}
	replay, err := s.ResolveDecision(ctx, in)
	if err != nil || replay.Result != first.Result {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if n := count(); n != 1 {
		t.Fatalf("audit rows=%d after a replay, want 1", n)
	}
}

// Invariant: the answer channels mcp and dots are accepted; an unknown channel
// is still refused before any attempt is written.
func TestDecisionAnswerChannelWhitelist(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	if _, err := s.ResolveDecision(ctx, answerA(request, "linear-webhook", "bearer:test")); err == nil {
		t.Fatal("unknown channel accepted")
	}
	if _, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelDots, "operator:test")); err != nil {
		t.Fatalf("dots channel refused: %v", err)
	}
}
