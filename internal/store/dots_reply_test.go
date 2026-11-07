package store

import (
	"strings"
	"testing"
	"time"
)

func TestDotsReplyAtomicAndIdempotent(t *testing.T) {
	s := schemaStore(t, "dots_reply")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	// A triage task cannot be completed: the refusal happens after the reply
	// and result were tentatively written and must roll them all back.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET state='triage' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	in := DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "The answer", IdempotencyKey: "reply-1"}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in); err == nil {
		t.Fatal("triage completion unexpectedly succeeded")
	}
	comments, err := s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 0 {
		t.Fatalf("comments=%v err=%v", comments, err)
	}
	results, err := s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 0 {
		t.Fatalf("results=%v err=%v", results, err)
	}
	docs, err := s.ListDocumentsByPrefix(ctx, "dots/results/", 10)
	if err != nil || len(docs) != 0 {
		t.Fatalf("documents=%v err=%v", docs, err)
	}
	var notifications int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE target_lane='requester'`).Scan(&notifications); err != nil || notifications != 0 {
		t.Fatalf("notifications=%d err=%v", notifications, err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET state='backlog' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	first, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in)
	if err != nil || first.Duplicate || first.Task.State != StateCompleted || first.Task.Refs.Closure == nil || first.Task.Refs.Closure.Evidence != "hk:doc "+first.DocumentKey {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in)
	if err != nil || !again.Duplicate || again.DocumentKey != first.DocumentKey {
		t.Fatalf("repeat=%+v err=%v", again, err)
	}
	comments, err = s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 1 || comments[0].Author != "operator:test@example.com" {
		t.Fatalf("comments=%v err=%v", comments, err)
	}
	results, err = s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 1 || results[0].Source != "hk" || results[0].RoutedLane != "requester" || !strings.HasPrefix(results[0].DocumentKey, "dots/results/hk-") {
		t.Fatalf("results=%v err=%v", results, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE target_lane='requester'`).Scan(&notifications); err != nil || notifications != 1 {
		t.Fatalf("notifications=%d err=%v", notifications, err)
	}
	in.Body = "Different answer"
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in); err == nil {
		t.Fatal("reused key with different payload accepted")
	}
}

func TestDotsOperatorQuestionRollsBackAndRetries(t *testing.T) {
	s := schemaStore(t, "dots_question")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_dots_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source = 'dots-hk-chatgpt' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `CREATE TRIGGER reject_dots_event BEFORE INSERT ON task_events FOR EACH ROW EXECUTE FUNCTION reject_dots_event()`)
	if err != nil {
		t.Fatal(err)
	}
	in := DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: "Which option?", AskOperator: true, IdempotencyKey: "question-1"}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in); err == nil {
		t.Fatal("injected failure did not fail the reply")
	}
	current, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found || current.AwaitingDecision || current.Refs.DecisionRequest != nil {
		t.Fatalf("task=%+v found=%v err=%v", current, found, err)
	}
	comments, err := s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 0 {
		t.Fatalf("comments=%v err=%v", comments, err)
	}
	results, err := s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 0 {
		t.Fatalf("results=%v err=%v", results, err)
	}
	docs, err := s.ListDocumentsByPrefix(ctx, "dots/results/", 10)
	if err != nil || len(docs) != 0 {
		t.Fatalf("documents=%v err=%v", docs, err)
	}
	var notifications int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&notifications); err != nil || notifications != 0 {
		t.Fatalf("notifications=%d err=%v", notifications, err)
	}
	if _, err := s.pool.Exec(ctx, `DROP TRIGGER reject_dots_event ON task_events`); err != nil {
		t.Fatal(err)
	}
	first, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in)
	if err != nil || first.Duplicate || !first.Task.AwaitingDecision || first.Task.Refs.DecisionRequest == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in)
	if err != nil || !again.Duplicate {
		t.Fatalf("repeat=%+v err=%v", again, err)
	}
	comments, err = s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments=%v err=%v", comments, err)
	}
	results, err = s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 1 {
		t.Fatalf("results=%v err=%v", results, err)
	}
}

func TestDotsReplyMatchesLinearRecord(t *testing.T) {
	s := schemaStore(t, "dots_compare")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	issue := DotsIssue{Identifier: "ZZZ-1", Title: task.Title, URL: "https://linear.example/ZZZ-1", IssueUUID: "0a1b2c3d-0000-4000-8000-0000000000b1", Description: "hk-lane: requester"}
	origin := validOrigin()
	origin.ExternalID = issue.IssueUUID
	if _, _, err := s.AddTaskOrigin(ctx, task.ID, "caller", origin); err != nil {
		t.Fatal(err)
	}
	comment := DotsComment{ID: "0a1b2c3d-1111-4111-8111-111111111111", CreatedAt: time.Now().UTC().Format(time.RFC3339), Body: "[dots] RESULT\nThe answer"}
	linearKey, created, err := s.RecordDotsResult(ctx, "RESULT", issue, comment)
	if err != nil || !created {
		t.Fatalf("linear: key=%s created=%v err=%v", linearKey, created, err)
	}
	hk, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "The answer", IdempotencyKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	results, err := s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	a, b := results[0], results[1]
	if a.Source != "hk" || b.Source != "linear" || a.Kind != b.Kind || a.Verdict != b.Verdict || a.ArtefactSHA256 != b.ArtefactSHA256 || a.RoutedLane != b.RoutedLane {
		t.Fatalf("hk=%+v linear=%+v", a, b)
	}
	for _, key := range []string{linearKey, hk.DocumentKey} {
		doc, found, err := s.GetDocument(ctx, key)
		if err != nil || !found || !strings.HasSuffix(doc.Body, "\n\n"+comment.Body+"\n") {
			t.Fatalf("document=%+v found=%v err=%v", doc, found, err)
		}
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE target_lane='requester' AND kind=$1`, OutboxKindDotsReport).Scan(&count); err != nil || count != 2 {
		t.Fatalf("notifications=%d err=%v", count, err)
	}
}

func TestDotsReplyStates(t *testing.T) {
	for _, tc := range []struct {
		kind, state string
		ask         bool
		results     int
		awaiting    bool
	}{
		{"START", StateResearching, false, 0, false},
		{"BLOCKED", StateIcebox, false, 1, false},
		{"QUESTION", StateBacklog, false, 1, false},
		{"QUESTION", StateBacklog, true, 1, true},
	} {
		t.Run(tc.kind+tc.state+map[bool]string{true: "_operator"}[tc.ask], func(t *testing.T) {
			s := schemaStore(t, "dots_state")
			task, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := s.ReplyDots(t.Context(), "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: tc.kind, Body: "Need an answer", AskOperator: tc.ask, IdempotencyKey: "key"})
			if err != nil || got.Task.State != tc.state || got.Task.AwaitingDecision != tc.awaiting {
				t.Fatalf("reply=%+v err=%v", got, err)
			}
			if tc.kind == "START" && got.Task.ClaimedBy != "dots" {
				t.Fatalf("claimant=%q", got.Task.ClaimedBy)
			}
			if tc.ask && got.Task.Refs.DecisionRequest == nil {
				t.Fatal("missing decision request")
			}
			results, err := s.ListDotsResults(t.Context(), task.ID, "", 10)
			if err != nil || len(results) != tc.results {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}
