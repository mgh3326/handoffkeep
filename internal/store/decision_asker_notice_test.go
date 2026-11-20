package store

import (
	"context"
	"testing"
)

// Invariant: an answer also reaches the lane that asked, only when that lane
// differs from the task's lane, and its outbox id never equals the task
// lane's id, so both rows can exist for one resolution.
func TestDecisionAskerNoticeLane(t *testing.T) {
	cases := []struct {
		name, taskLane, requestLane string
		wantLane                    string
		wantOK                      bool
	}{
		{"asker differs from task lane", "handoffkeep-design", "mac-lane", "mac-lane", true},
		{"asker is the task lane", "handoffkeep-design", "handoffkeep-design", "", false},
		{"no asker recorded", "handoffkeep-design", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lane, ok := decisionAskerNoticeLane(c.taskLane, DecisionRequest{RequestLane: c.requestLane}, ResolveDecisionInput{Channel: DecisionChannelCLI})
			if lane != c.wantLane || ok != c.wantOK {
				t.Fatalf("lane=%q ok=%v, want lane=%q ok=%v", lane, ok, c.wantLane, c.wantOK)
			}
		})
	}
	if decisionAskerAnswerNoticeEventID("dr-1-1", 7) == decisionAnswerNoticeEventID("dr-1-1", 7) {
		t.Fatal("asker event id must differ from the task lane's event id")
	}
}

// Invariant: an answer to a request filed by another lane writes two answered
// notices, one to the task's lane and one to the asker; the same lane writes
// only the task lane's notice.
func TestDecisionAnswerNoticeReachesAsker(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()

	task := assistantTask(t, s, "in_progress")
	in := decisionInput()
	in.RequestLane = "asker-lane"
	request := assistantRecordWith(t, s, task.ID, in)
	out, err := s.ResolveDecision(ctx, answerA(request, DecisionChannelCLI, "operator"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	rows := requestNotices(t, s, request.ID)
	if len(rows) != 2 {
		t.Fatalf("asker request: notices=%+v, want 2", rows)
	}
	lanes := map[string]string{}
	for _, row := range rows {
		lanes[row.TargetLane] = row.EventID
	}
	if lanes[task.Lane] != decisionAnswerNoticeEventID(request.ID, out.ResolutionID) {
		t.Fatalf("task lane notice = %q, lanes=%v", lanes[task.Lane], lanes)
	}
	if lanes["asker-lane"] != decisionAskerAnswerNoticeEventID(request.ID, out.ResolutionID) {
		t.Fatalf("asker lane notice = %q, lanes=%v", lanes["asker-lane"], lanes)
	}

	sameTask := assistantTask(t, s, "in_progress")
	sameIn := decisionInput()
	sameIn.RequestLane = sameTask.Lane
	sameRequest := assistantRecordWith(t, s, sameTask.ID, sameIn)
	if _, err := s.ResolveDecision(ctx, answerA(sameRequest, DecisionChannelCLI, "operator")); err != nil {
		t.Fatalf("resolve same lane: %v", err)
	}
	if rows := requestNotices(t, s, sameRequest.ID); len(rows) != 1 {
		t.Fatalf("same-lane request: notices=%+v, want 1", rows)
	}
}

// Invariant (hk 5527): a session answer recorded by the request's asker lane
// sends that lane no answer notice, since its session wrote the answer; the
// task lane's notice stays. A console answer, and a session answer recorded
// by another lane, still reach the asker.
func TestDecisionAnswerNoticeSkipsRecordingAsker(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	askerRows := func(t *testing.T, requestID string) int {
		n := 0
		for _, row := range requestNotices(t, s, requestID) {
			if row.TargetLane == "asker-lane" {
				n++
			}
		}
		return n
	}
	newRequest := func(t *testing.T) (Task, DecisionRequest) {
		task := assistantTask(t, s, "in_progress")
		in := decisionInput()
		in.RequestLane = "asker-lane"
		return task, assistantRecordWith(t, s, task.ID, in)
	}

	task, request := newRequest(t)
	recorded := sessionAnswer(request, "")
	recorded.RecordingLane = "asker-lane"
	if _, err := s.ResolveDecision(ctx, recorded); err != nil {
		t.Fatalf("session resolve by asker: %v", err)
	}
	if asker := askerRows(t, request.ID); asker != 0 {
		t.Fatalf("session by asker lane: asker notices=%d, want 0", asker)
	}
	if rows := requestNotices(t, s, request.ID); len(rows) != 1 || rows[0].TargetLane != task.Lane {
		t.Fatalf("session by asker lane: notices=%+v, want the task lane's one row", rows)
	}

	_, consoleRequest := newRequest(t)
	if _, err := s.ResolveDecision(ctx, answerA(consoleRequest, DecisionChannelConsole, "operator")); err != nil {
		t.Fatalf("console resolve: %v", err)
	}
	if asker := askerRows(t, consoleRequest.ID); asker != 1 {
		t.Fatalf("console answer: asker notices=%d, want 1", asker)
	}

	_, otherRequest := newRequest(t)
	byOther := sessionAnswer(otherRequest, "")
	byOther.RecordingLane = "other-lane"
	if _, err := s.ResolveDecision(ctx, byOther); err != nil {
		t.Fatalf("session resolve by other lane: %v", err)
	}
	if asker := askerRows(t, otherRequest.ID); asker != 1 {
		t.Fatalf("session by other lane: asker notices=%d, want 1", asker)
	}
}

// Invariant: grounds are at most DecisionRequestMaxGrounds one-line strings,
// and the asker lane is a lane name; a refusal names the rule.
func TestDecisionRequestGroundsAndLaneValidation(t *testing.T) {
	ok := decisionInput()
	ok.Grounds, ok.RequestLane = []string{"CI green on head abc"}, "mac-lane"
	if err := CheckDecisionRequestInput(ok); err != nil {
		t.Fatalf("valid grounds refused: %v", err)
	}
	many := decisionInput()
	many.Grounds = make([]string, DecisionRequestMaxGrounds+1)
	for i := range many.Grounds {
		many.Grounds[i] = "x"
	}
	multiline := decisionInput()
	multiline.Grounds = []string{"first\nsecond"}
	badLane := decisionInput()
	badLane.RequestLane = "Not A Lane"
	for name, in := range map[string]DecisionRequestInput{"too many": many, "multiline": multiline, "bad lane": badLane} {
		if err := CheckDecisionRequestInput(in); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}
