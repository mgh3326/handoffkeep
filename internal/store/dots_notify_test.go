package store

import (
	"testing"
)

// notifyRows counts the outbox rows of one lane and kind; an empty lane or kind
// matches any.
func notifyRows(t *testing.T, s *Store, lane, kind string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM notification_outbox WHERE ($1='' OR target_lane=$1) AND ($2='' OR kind=$2)`, lane, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Invariant: a dots reply notifies the requester lane once, by kind: RESULT is
// ready (dots_report), QUESTION is decision (dots_decision), BLOCKED is blocked
// (dots_blocked). START and PROGRESS send nothing (operator decision 166).
func TestDotsReplyNotifiesRequesterByKind(t *testing.T) {
	for _, tc := range []struct {
		kind, outbox string
	}{
		{"RESULT", OutboxKindDotsReport},
		{"QUESTION", OutboxKindDotsDecision},
		{"BLOCKED", OutboxKindDotsBlocked},
		{"PROGRESS", ""},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			s := schemaStore(t, "dots_notify_kind")
			task, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
			if err != nil {
				t.Fatal(err)
			}
			started, _ := startDotsRun(t, s, task.ID, "start")
			if _, err := s.ReplyDots(t.Context(), "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: tc.kind, Body: "Need an answer", IdempotencyKey: "key", RunToken: started.RunToken}); err != nil {
				t.Fatal(err)
			}
			if tc.outbox == "" {
				if n := notifyRows(t, s, "requester", ""); n != 0 {
					t.Fatalf("%s sent %d lane events", tc.kind, n)
				}
				return
			}
			if n := notifyRows(t, s, "requester", tc.outbox); n != 1 {
				t.Fatalf("%s: %s rows=%d", tc.kind, tc.outbox, n)
			}
			if n := notifyRows(t, s, "requester", ""); n != 1 {
				t.Fatalf("%s: requester lane got %d events, want exactly one", tc.kind, n)
			}
		})
	}
}

// Invariant: a RESULT also notifies each lane a "[알릴 레인]" line of the
// request names, once each, and the requester lane is not repeated.
func TestDotsResultNotifiesNamedLanes(t *testing.T) {
	s := schemaStore(t, "dots_notify_named")
	task, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "Research", Body: "Investigate\n[알릴 레인] lane-a, lane-b, requester", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	if _, err := s.ReplyDots(t.Context(), "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "Done", IdempotencyKey: "key", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	for _, lane := range []string{"requester", "lane-a", "lane-b"} {
		if n := notifyRows(t, s, lane, OutboxKindDotsReport); n != 1 {
			t.Fatalf("%s got %d ready events, want 1", lane, n)
		}
	}
	if n := notifyRows(t, s, "", ""); n != 3 {
		t.Fatalf("lane events=%d, want 3", n)
	}
}

// Invariant: a "[알릴 레인]" line that names an invalid lane refuses the RESULT
// and writes nothing, so no notice is lost to a typo.
func TestDotsResultRefusesInvalidNotifyLane(t *testing.T) {
	s := schemaStore(t, "dots_notify_invalid")
	task, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "Research", Body: "Investigate\n[알릴 레인] Not A Lane", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	if _, err := s.ReplyDots(t.Context(), "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "Done", IdempotencyKey: "key", RunToken: started.RunToken}); err == nil {
		t.Fatal("RESULT with an invalid notify lane was accepted")
	}
	if n := notifyRows(t, s, "", ""); n != 0 {
		t.Fatalf("lane events=%d after a refused reply", n)
	}
}
