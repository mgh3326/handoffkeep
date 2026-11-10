package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
	started, _ := startDotsRun(t, s, task.ID, "start")
	// A triage task cannot be completed: the refusal happens after the reply
	// and result were tentatively written and must roll them all back.
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET state='triage' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	in := DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "The answer", IdempotencyKey: "reply-1", RunToken: started.RunToken}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in); err == nil {
		t.Fatal("triage completion unexpectedly succeeded")
	}
	comments, err := s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 1 {
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
	closed, found, getErr := s.GetTask(ctx, task.ID)
	if err != nil || first.Duplicate || first.State != StateCompleted || getErr != nil || !found || closed.Refs.Closure == nil || closed.Refs.Closure.Evidence != "hk:doc "+first.DocumentKey {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in)
	if err != nil || !again.Duplicate || again.DocumentKey != first.DocumentKey {
		t.Fatalf("repeat=%+v err=%v", again, err)
	}
	comments, err = s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 2 || comments[1].Author != "operator:test@example.com" {
		t.Fatalf("comments=%v err=%v", comments, err)
	}
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 2 || replies[1].Kind != "RESULT" || replies[1].Channel != "chatgpt" || replies[1].Body != in.Body {
		t.Fatalf("replies=%+v err=%v", replies, err)
	}
	results, err = s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 1 || results[0].Source != "hk" || results[0].RoutedLane != "requester" || !strings.HasPrefix(results[0].DocumentKey, "dots/results/hk-") {
		t.Fatalf("results=%v err=%v", results, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE target_lane='requester'`).Scan(&notifications); err != nil || notifications != 1 {
		t.Fatalf("notifications=%d err=%v", notifications, err)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: "Too late?", IdempotencyKey: "new-question", RunToken: started.RunToken}); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("question on completed task: %v", err)
	}
	comments, err = s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 2 {
		t.Fatalf("comments after refusal=%v err=%v", comments, err)
	}
	in.Body = "Different answer"
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in); !errors.Is(err, ErrDotsReplyKeyConflict) {
		t.Fatalf("changed payload error=%v", err)
	}
}

func TestDotsReplyRejectsDocumentCollision(t *testing.T) {
	s := schemaStore(t, "dots_doc_collision")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	key := "collision"
	id := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", task.ID, key)))
	for _, date := range []time.Time{time.Now().UTC(), time.Now().UTC().Add(24 * time.Hour)} {
		docKey := fmt.Sprintf("dots/results/hk-%d/%s-%s", task.ID, date.Format("2006-01-02"), hex.EncodeToString(id[:])[:8])
		if _, _, err := s.PutDocument(ctx, Document{Key: docKey, Kind: "note", Body: "Existing partial result", CreatedBy: "tester"}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "A different result", IdempotencyKey: key, RunToken: started.RunToken})
	if err == nil || !strings.Contains(err.Error(), "document key collision") {
		t.Fatalf("collision error=%v", err)
	}
	current, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found || current.State != StateResearching {
		t.Fatalf("task=%+v found=%v err=%v", current, found, err)
	}
	comments, err := s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments=%+v err=%v", comments, err)
	}
}

func TestDotsStartConcurrentClaim(t *testing.T) {
	s := schemaStore(t, "dots_claim")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		input  DotsReplyInput
		result DotsReplyResult
		err    error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		in := DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Starting", IdempotencyKey: fmt.Sprintf("key-%d", i), ExpectedUpdatedAt: &task.UpdatedAt}
		go func() {
			<-start
			result, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in)
			results <- outcome{in, result, err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil {
		a, b = b, a
	}
	if a.err != nil || !errors.Is(b.err, ErrTaskConflict) || a.result.State != StateResearching || a.result.RunToken == "" {
		t.Fatalf("first=%+v second=%+v", a, b)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "Done", IdempotencyKey: "finish", RunToken: a.result.RunToken}); err != nil {
		t.Fatal(err)
	}
	replay, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", a.input)
	if err != nil || !replay.Duplicate || replay.State != a.result.State || replay.RunToken != a.result.RunToken {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	comments, err := s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 2 {
		t.Fatalf("comments=%+v err=%v", comments, err)
	}
}

func TestDotsRequestNeverEnqueuesLinearMirror(t *testing.T) {
	s := schemaStore(t, "dots_single_channel")
	s.EnableLinearBoard()
	s.EnableLinearSync()
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	started, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "START", Body: "Starting", IdempotencyKey: "start", ExpectedUpdatedAt: &task.UpdatedAt})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: "Which option?", AskOperator: true, IdempotencyKey: "question", RunToken: started.RunToken})
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := s.ListLinearOutbox(ctx, task.ID)
	if err != nil || len(outbox) != 0 {
		t.Fatalf("linear outbox=%+v err=%v", outbox, err)
	}
}

func TestDotsOperatorQuestionRollsBackAndRetries(t *testing.T) {
	s := schemaStore(t, "dots_question")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	// START writes a dots_reply event, which the trigger below rejects, so the
	// run starts before the trigger exists.
	started, _ := startDotsRun(t, s, task.ID, "start")
	_, err = s.pool.Exec(ctx, `CREATE FUNCTION reject_dots_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source = 'dots-hk-chatgpt' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.pool.Exec(ctx, `CREATE TRIGGER reject_dots_event BEFORE INSERT ON task_events FOR EACH ROW EXECUTE FUNCTION reject_dots_event()`)
	if err != nil {
		t.Fatal(err)
	}
	in := DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: "Which option?", AskOperator: true, IdempotencyKey: "question-1", RunToken: started.RunToken}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in); err == nil {
		t.Fatal("injected failure did not fail the reply")
	}
	current, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found || current.AwaitingDecision || current.Refs.DecisionRequest != nil {
		t.Fatalf("task=%+v found=%v err=%v", current, found, err)
	}
	comments, err := s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 1 {
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
	current, found, getErr := s.GetTask(ctx, task.ID)
	if err != nil || first.Duplicate || getErr != nil || !found || !current.AwaitingDecision || current.Refs.DecisionRequest == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	again, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in)
	if err != nil || !again.Duplicate {
		t.Fatalf("repeat=%+v err=%v", again, err)
	}
	comments, err = s.ListTaskComments(ctx, task.ID, 0, 10)
	if err != nil || len(comments) != 2 {
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
	started, _ := startDotsRun(t, s, task.ID, "start")
	comment := DotsComment{ID: "0a1b2c3d-1111-4111-8111-111111111111", CreatedAt: time.Now().UTC().Format(time.RFC3339), Body: "[dots] RESULT\nThe answer"}
	linearKey, created, err := s.RecordDotsResult(ctx, "RESULT", issue, comment)
	if err != nil || !created {
		t.Fatalf("linear: key=%s created=%v err=%v", linearKey, created, err)
	}
	hk, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "The answer", IdempotencyKey: "key", RunToken: started.RunToken})
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
		kind string
		// state is the state after the reply; "" means it stays the state
		// START left the request in.
		state    string
		ask      bool
		results  int
		awaiting bool
	}{
		{"PROGRESS", "", false, 0, false},
		{"BLOCKED", StateIcebox, false, 1, false},
		{"QUESTION", "", false, 1, false},
		{"QUESTION", "", true, 1, true},
		{"RESULT", StateCompleted, false, 1, false},
	} {
		t.Run(tc.kind+map[bool]string{true: "_operator"}[tc.ask], func(t *testing.T) {
			s := schemaStore(t, "dots_state")
			task, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
			if err != nil {
				t.Fatal(err)
			}
			started, _ := startDotsRun(t, s, task.ID, "start")
			want := tc.state
			if want == "" {
				want = started.State
			}
			got, err := s.ReplyDots(t.Context(), "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: tc.kind, Body: "Need an answer", AskOperator: tc.ask, IdempotencyKey: "key", RunToken: started.RunToken})
			current, found, getErr := s.GetTask(t.Context(), task.ID)
			if err != nil || got.State != want || getErr != nil || !found || current.AwaitingDecision != tc.awaiting {
				t.Fatalf("reply=%+v err=%v", got, err)
			}
			if current.ClaimedBy != "dots" {
				t.Fatalf("claimant=%q", current.ClaimedBy)
			}
			if tc.ask && current.Refs.DecisionRequest == nil {
				t.Fatal("missing decision request")
			}
			results, err := s.ListDotsResults(t.Context(), task.ID, "", 10)
			if err != nil || len(results) != tc.results {
				t.Fatalf("results=%+v err=%v", results, err)
			}
		})
	}
}

func TestDotsProgressRecordsPartialResult(t *testing.T) {
	s := schemaStore(t, "dots_progress")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	in := DotsReplyInput{TaskID: task.ID, Kind: "PROGRESS", Body: "Found a lead", IdempotencyKey: "progress", RunToken: started.RunToken}
	first, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in)
	if err != nil || first.State != started.State || first.DocumentKey == "" {
		t.Fatalf("reply=%+v err=%v", first, err)
	}
	// A PROGRESS replay is judged by its key, not by a revision it no longer
	// needs to send.
	newRevision := task.UpdatedAt.Add(time.Second)
	in.ExpectedUpdatedAt = &newRevision
	replay, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", in)
	if err != nil || !replay.Duplicate || replay.DocumentKey != first.DocumentKey {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	doc, found, err := s.GetDocument(ctx, first.DocumentKey)
	if err != nil || !found || !strings.Contains(doc.Body, "[dots] PROGRESS") || !strings.Contains(doc.Body, in.Body) {
		t.Fatalf("document=%+v found=%v err=%v", doc, found, err)
	}
	results, err := s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 0 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 2 || replies[1].Kind != "PROGRESS" {
		t.Fatalf("replies=%+v err=%v", replies, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE target_lane='requester' AND kind=$1`, OutboxKindDotsReport).Scan(&count); err != nil || count != 1 {
		t.Fatalf("notifications=%d err=%v", count, err)
	}
	if DotsKindRecorded("PROGRESS") {
		t.Fatal("Linear webhook must continue ignoring PROGRESS")
	}
}

// An operator answer to a dots question clears the flag its Block request set,
// so the run can still record its RESULT.
func TestDotsOperatorAnswerLetsResultComplete(t *testing.T) {
	s := schemaStore(t, "dots_answer_result")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: "Which option?", AskOperator: true, IdempotencyKey: "question", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	asked, _, _ := s.GetTask(ctx, task.ID)
	if !asked.AwaitingDecision || asked.Refs.DecisionRequest == nil {
		t.Fatalf("question did not flag the task: %+v", asked)
	}
	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "operator:test@example.com", DecisionResolveInput{RequestID: asked.Refs.DecisionRequest.ID, Kind: DecisionRequestAnswered, Option: "A", Text: "go ahead", Responder: "operator"}); err != nil {
		t.Fatal(err)
	}
	answered, _, _ := s.GetTask(ctx, task.ID)
	if answered.AwaitingDecision || answered.State != asked.State {
		t.Fatalf("after answer: awaiting=%v state=%s, want cleared and %s", answered.AwaitingDecision, answered.State, asked.State)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "done", IdempotencyKey: "result", RunToken: started.RunToken}); err != nil {
		t.Fatalf("RESULT after the answer: %v", err)
	}
	done, _, _ := s.GetTask(ctx, task.ID)
	if done.State != StateCompleted {
		t.Fatalf("state=%s, want %s", done.State, StateCompleted)
	}
}
