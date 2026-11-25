package store

import (
	"context"
	"testing"
)

// Invariant (hk 6481): a session answer recorded by the task lane's own
// session writes no task-lane notice. The decision event and audit rows are
// still written: the resolution closes the request, only the lane notice is
// skipped, because the recording session saw its own answer.
func TestDecisionAnswerNoNoticeToRecordingTaskLane(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()

	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	in := sessionAnswer(request, "")
	in.RecordingLane = task.Lane
	out, err := s.ResolveDecision(ctx, in)
	if err != nil || out.Result != DecisionResultAnswered {
		t.Fatalf("resolve: out=%+v err=%v", out, err)
	}
	if out.ResolutionID == 0 || out.Request.Status != DecisionRequestAnswered {
		t.Fatalf("answer was not recorded: out=%+v", out)
	}
	if rows := requestNotices(t, s, request.ID); len(rows) != 0 {
		t.Fatalf("session by task lane: notices=%+v, want none", rows)
	}
}

// Invariant (hk 6481): answers given elsewhere keep the task lane's notice:
// console, MCP, a session answer recorded by another lane, and a session
// answer without a recording lane.
func TestDecisionAnswerNoticeKeptForOtherChannels(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   func(DecisionRequest) ResolveDecisionInput
	}{
		{"console", func(r DecisionRequest) ResolveDecisionInput {
			return answerA(r, DecisionChannelConsole, "operator")
		}},
		{"mcp", func(r DecisionRequest) ResolveDecisionInput {
			return answerA(r, DecisionChannelMCP, "bearer:test")
		}},
		{"session by other lane", func(r DecisionRequest) ResolveDecisionInput {
			in := sessionAnswer(r, "")
			in.RecordingLane = "other-lane"
			return in
		}},
		{"session without recording lane", func(r DecisionRequest) ResolveDecisionInput {
			return sessionAnswer(r, "")
		}},
	}
	for _, c := range cases {
		task := assistantTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		out, err := s.ResolveDecision(ctx, c.in(request))
		if err != nil || out.Result != DecisionResultAnswered {
			t.Fatalf("%s: out=%+v err=%v", c.name, out, err)
		}
		rows := requestNotices(t, s, request.ID)
		if len(rows) != 1 {
			t.Fatalf("%s: notices=%+v, want 1", c.name, rows)
		}
		row := rows[0]
		if row.TargetLane != task.Lane || row.EventID != decisionAnswerNoticeEventID(request.ID, out.ResolutionID) {
			t.Fatalf("%s: row=%+v", c.name, row)
		}
	}
}
