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
	// PROGRESS is kept as a document and sends no lane event (operator decision 166).
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE target_lane='requester'`).Scan(&count); err != nil || count != 0 {
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

func TestAnswerDotsQuestion(t *testing.T) {
	s := schemaStore(t, "dots_answer_q")
	ctx := t.Context()

	// 1. Task with no open question and no open decision request -> ErrNoOpenDotsQuestion
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "answer"}); !errors.Is(err, ErrNoOpenDotsQuestion) || !strings.Contains(err.Error(), "no open dots question or decision request") {
		t.Fatalf("expected ErrNoOpenDotsQuestion with its reason, got %v", err)
	}

	// 2. Open dots QUESTION
	started, _ := startDotsRun(t, s, task.ID, "start")
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{
		TaskID:         task.ID,
		Kind:           "QUESTION",
		Body:           "Which option?",
		AskOperator:    true,
		IdempotencyKey: "question",
		RunToken:       started.RunToken,
	}); err != nil {
		t.Fatal(err)
	}

	asked, _, _ := s.GetTask(ctx, task.ID)
	if !asked.AwaitingDecision || asked.Refs.DecisionRequest == nil {
		t.Fatalf("question did not flag task: %+v", asked)
	}

	// 3. Plain comment does NOT resolve it
	comment, err := s.CreateTaskComment(ctx, task.ID, "caller", "just a comment")
	if err != nil {
		t.Fatal(err)
	}
	if comment.ID == 0 {
		t.Fatal("comment ID is 0")
	}
	afterComment, _, _ := s.GetTask(ctx, task.ID)
	if !afterComment.AwaitingDecision || afterComment.Refs.DecisionRequest.Status != DecisionRequestOpen {
		t.Fatalf("comment should not resolve decision: awaiting=%v status=%s", afterComment.AwaitingDecision, afterComment.Refs.DecisionRequest.Status)
	}

	// 4. AnswerDotsQuestion resolves request, clears awaiting_decision, records comment
	answerRes, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "proceed with option B"})
	if err != nil {
		t.Fatalf("AnswerDotsQuestion failed: %v", err)
	}
	if answerRes.Task.AwaitingDecision {
		t.Fatalf("awaiting_decision should be false, got true")
	}
	if answerRes.Request.Status != DecisionRequestAnswered {
		t.Fatalf("request status should be answered, got %s", answerRes.Request.Status)
	}
	if answerRes.Request.Resolution == nil || answerRes.Request.Resolution.Text != "proceed with option B" || answerRes.Request.Resolution.Responder != "caller" {
		t.Fatalf("unexpected resolution: %+v", answerRes.Request.Resolution)
	}
	if answerRes.Comment.Body != "proceed with option B" || answerRes.Comment.Author != "caller" {
		t.Fatalf("unexpected comment: %+v", answerRes.Comment)
	}

	answeredTask, _, _ := s.GetTask(ctx, task.ID)
	if answeredTask.AwaitingDecision {
		t.Fatalf("answered task awaiting_decision should be false, got true")
	}
	// The QUESTION the decision came from is answered with it, so the request
	// no longer reports an open question (hk 5000).
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 2 || replies[1].Kind != "QUESTION" || replies[1].Answer == nil || replies[1].Answer.Text != "proceed with option B" {
		t.Fatalf("decision answer did not close the question: replies=%+v err=%v", replies, err)
	}

	// 5. A second answer is refused with a clear code (ErrDecisionRequestResolved)
	if _, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "another answer"}); !errors.Is(err, ErrDecisionRequestResolved) {
		t.Fatalf("second answer expected ErrDecisionRequestResolved, got %v", err)
	}

	// 6. Policy checks: human_only / operator_direct request is refused
	opTask, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Op", Body: "Op Question", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	opStarted, _ := startDotsRun(t, s, opTask.ID, "start-op")
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{
		TaskID:         opTask.ID,
		Kind:           "QUESTION",
		Body:           "Needs operator?",
		AskOperator:    true,
		IdempotencyKey: "question-op",
		RunToken:       opStarted.RunToken,
	}); err != nil {
		t.Fatal(err)
	}
	// Make decision request human-only / operator_direct
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET refs=jsonb_set(refs, '{decision_request,human_only}', 'true') WHERE id=$1`, opTask.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerDotsQuestion(ctx, opTask.ID, "caller", DotsAnswerInput{Text: "unauthorized"}); !errors.Is(err, ErrDecisionRequestHumanOnly) {
		t.Fatalf("expected ErrDecisionRequestHumanOnly, got %v", err)
	}

	// 7. Non-dots request task is refused
	plainTask, err := s.CreateTask(ctx, Task{Lane: "dev", Title: "Plain task", Kind: "implement", CreatedBy: "tester", Project: projectPtr(testProjectName)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerDotsQuestion(ctx, plainTask.ID, "caller", DotsAnswerInput{Text: "answer"}); err == nil || !strings.Contains(err.Error(), "invalid dots request task") {
		t.Fatalf("expected invalid dots request task error, got %v", err)
	}
}

// Invariant (operator decision 364, hk 5000): a plain dots QUESTION — one sent
// without ask_operator, so no decision request opens — is answered by the
// request's requester lane with dots answer. The answer is the task comment,
// its dots_results row marks the question answered, and a question is answered
// once.
func TestAnswerDotsPlainQuestion(t *testing.T) {
	s := schemaStore(t, "dots_answer_plain")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	const question = "Which of the three should I make first?"
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: question, IdempotencyKey: "question", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}

	// Only the request's requester lane may answer, and the refusal names the
	// rule rather than echoing the lane.
	if _, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "the first", RequesterLane: "intruder-lane"}); !errors.Is(err, ErrTaskConflict) || !strings.Contains(err.Error(), "only the request's requester lane may answer its question") {
		t.Fatalf("another lane: %v", err)
	}
	if _, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "the first"}); !errors.Is(err, ErrDotsRequestInvalid) {
		t.Fatalf("missing requester lane: %v", err)
	}
	// A refused answer records nothing.
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 2 || replies[1].Answer != nil {
		t.Fatalf("refused answers changed the question: replies=%+v err=%v", replies, err)
	}

	const answer = "Make the van first; the other two follow the same body."
	res, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: answer, RequesterLane: "requester"})
	if err != nil {
		t.Fatalf("AnswerDotsQuestion: %v", err)
	}
	if res.Request != nil || res.Question == nil || res.Question.ResultID == 0 || res.Question.CommentID == 0 || res.CommentID == 0 {
		t.Fatalf("result=%+v", res)
	}
	if res.Task.State != StateResearching || res.Task.AwaitingDecision {
		t.Fatalf("answering must not move the request: %+v", res.Task)
	}
	if res.Comment.Author != "caller" || res.Comment.Body != answer {
		t.Fatalf("answer comment=%+v", res.Comment)
	}

	// dots reads the answer on the question it closed.
	replies, err = s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 2 || replies[1].Kind != "QUESTION" || replies[1].Answer == nil {
		t.Fatalf("replies=%+v err=%v", replies, err)
	}
	got := replies[1].Answer
	if got.CommentID != res.CommentID || got.Text != answer || got.By != "caller" || got.At.IsZero() {
		t.Fatalf("answer=%+v", got)
	}
	results, err := s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 1 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	if results[0].Kind != "QUESTION" || results[0].AnsweredAt == nil || results[0].AnsweredBy != "caller" || results[0].AnswerCommentID == nil || *results[0].AnswerCommentID != res.CommentID {
		t.Fatalf("question row=%+v", results[0])
	}

	// Answered once: a second answer finds no open question.
	if _, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "again", RequesterLane: "requester"}); !errors.Is(err, ErrNoOpenDotsQuestion) || !strings.Contains(err.Error(), "no open dots question or decision request") {
		t.Fatalf("second answer: %v", err)
	}
}

// Invariant (hk 5000): with more than one QUESTION open, an answer must name
// the one it closes instead of silently leaving another open, and the target
// is the reply id dots_inbox already exposes.
func TestAnswerDotsMultipleOpenQuestions(t *testing.T) {
	s := schemaStore(t, "dots_answer_multi")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	for _, q := range []struct{ key, body string }{{"q1", "Which first?"}, {"q2", "Which second?"}} {
		if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: q.body, IdempotencyKey: q.key, RunToken: started.RunToken}); err != nil {
			t.Fatal(err)
		}
	}
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 3 {
		t.Fatalf("replies=%+v err=%v", replies, err)
	}
	if _, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "no target", RequesterLane: "requester"}); !errors.Is(err, ErrDotsQuestionAmbiguous) || !strings.Contains(err.Error(), "more than one dots question is open") {
		t.Fatalf("without a target: %v", err)
	}
	res, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "the first", RequesterLane: "requester", QuestionID: replies[1].ID})
	if err != nil || res.Question == nil || res.Question.CommentID != replies[1].ID {
		t.Fatalf("targeted answer=%+v err=%v", res, err)
	}
	// One question remains, so no target is needed for it.
	res, err = s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "the second", RequesterLane: "requester"})
	if err != nil || res.Question == nil || res.Question.CommentID != replies[2].ID {
		t.Fatalf("remaining answer=%+v err=%v", res, err)
	}
}

// Invariant (hk 5000, operator decision 364): a QUESTION answered by a decision
// before this release keeps its answered row either way: the migration
// backfills the rows a closed decision request answered, and the plain path
// refuses a second answer to an unbackfilled row whose body matches the
// closed request.
func TestDotsQuestionAnswerBackfill(t *testing.T) {
	s := schemaStore(t, "dots_answer_backfill")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: "Which option?", AskOperator: true, IdempotencyKey: "question", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	asked, _, _ := s.GetTask(ctx, task.ID)
	request := asked.Refs.DecisionRequest
	if request == nil || request.DotsQuestionCommentID == 0 {
		t.Fatalf("decision request did not record its question: %+v", request)
	}
	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "operator:test@example.com", DecisionResolveInput{RequestID: request.ID, Kind: DecisionRequestAnswered, Option: "A", Text: "option B", Responder: "operator"}); err != nil {
		t.Fatal(err)
	}
	// Simulate the previous release's rows: the question has no answer mark and
	// the request carries no link to it.
	if _, err := s.pool.Exec(ctx, `UPDATE dots_results SET answered_at=NULL,answered_by=NULL,answer_comment_id=NULL,ask_operator=NULL WHERE task_id=$1 AND kind='QUESTION'`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET refs=refs #- '{decision_request,dots_question_comment_id}' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}

	// The unbackfilled row must not accept a second answer.
	if _, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "second answer", RequesterLane: "requester"}); !errors.Is(err, ErrDecisionRequestResolved) {
		t.Fatalf("second answer to an answered decision: %v", err)
	}

	// The migration backfills the row from the closed request.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyDotsQuestionAnswerMigration(ctx, tx); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 1 || results[0].Kind != "QUESTION" || results[0].AnsweredAt == nil || results[0].AnswerText != "option B" {
		t.Fatalf("backfill did not close the question: results=%+v err=%v", results, err)
	}
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 2 || replies[1].Answer == nil || replies[1].Answer.Text != "option B" {
		t.Fatalf("backfilled reply=%+v err=%v", replies, err)
	}
}

// Invariant (hk 5000): closing a decision request closes the exact QUESTION it
// came from, even when a later plain question repeats its text; a withdrawn
// decision closes it too.
func TestDotsDecisionClosesItsExactQuestion(t *testing.T) {
	s := schemaStore(t, "dots_answer_link")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	const body = "Which one?"
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: body, AskOperator: true, IdempotencyKey: "asked", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	asked, _, _ := s.GetTask(ctx, task.ID)
	request := asked.Refs.DecisionRequest
	if request == nil || request.DotsQuestionCommentID == 0 {
		t.Fatalf("no question link: %+v", request)
	}
	// A later plain question repeats the same text while the decision is open.
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: body, IdempotencyKey: "repeat", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "operator:test@example.com", DecisionResolveInput{RequestID: request.ID, Kind: DecisionRequestWithdrawn, Text: "not needed", Responder: "operator"}); err != nil {
		t.Fatal(err)
	}
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 3 {
		t.Fatalf("replies=%+v err=%v", replies, err)
	}
	if replies[1].Answer == nil || replies[1].Answer.Text != "not needed" {
		t.Fatalf("the decision's question stayed open or lost its text: %+v", replies[1])
	}
	if replies[2].Answer != nil {
		t.Fatalf("the repeated question was closed by the decision: %+v", replies[2])
	}
	// The repeated question is still answerable on its own.
	res, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "this one", RequesterLane: "requester"})
	if err != nil || res.Question == nil || res.Question.CommentID != replies[2].ID {
		t.Fatalf("plain answer=%+v err=%v", res, err)
	}
}

// Invariant (hk 5000): a targeted answer closes the plain question it names
// even while a decision request is open; the decision is not resolved by it.
func TestAnswerDotsTargetsPlainQuestionBesideDecision(t *testing.T) {
	s := schemaStore(t, "dots_answer_beside")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: "Decision question?", AskOperator: true, IdempotencyKey: "asked", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: "Plain question?", IdempotencyKey: "plain", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 3 {
		t.Fatalf("replies=%+v err=%v", replies, err)
	}

	// The target names the plain question, not the decision's.
	res, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "the plain one", RequesterLane: "requester", QuestionID: replies[2].ID})
	if err != nil || res.Question == nil || res.Question.CommentID != replies[2].ID {
		t.Fatalf("targeted plain answer=%+v err=%v", res, err)
	}
	still, _, _ := s.GetTask(ctx, task.ID)
	if still.Refs.DecisionRequest == nil || still.Refs.DecisionRequest.Status != DecisionRequestOpen || !still.AwaitingDecision {
		t.Fatalf("the decision must stay open: %+v", still.Refs.DecisionRequest)
	}
	replies, err = s.ListDotsReplies(ctx, task.ID)
	if err != nil || replies[1].Answer != nil || replies[2].Answer == nil {
		t.Fatalf("wrong question closed: replies=%+v err=%v", replies, err)
	}

	// An untargeted answer still resolves the decision.
	res, err = s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "the decision one"})
	if err != nil || res.Request == nil || res.Request.Status != DecisionRequestAnswered {
		t.Fatalf("decision answer=%+v err=%v", res, err)
	}
	replies, err = s.ListDotsReplies(ctx, task.ID)
	if err != nil || replies[1].Answer == nil || replies[2].Answer == nil {
		t.Fatalf("decision question stayed open: replies=%+v err=%v", replies, err)
	}
}

// Invariant (hk 5000): the migration's body fallback closes the question the
// closed decision came from, never a later repeat of the same text.
func TestDotsQuestionAnswerBackfillKeepsLaterRepeat(t *testing.T) {
	s := schemaStore(t, "dots_answer_backfill_repeat")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	const body = "Same question?"
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: body, AskOperator: true, IdempotencyKey: "asked", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	asked, _, _ := s.GetTask(ctx, task.ID)
	request := asked.Refs.DecisionRequest
	if _, err := s.ResolveDecisionRequest(ctx, task.ID, "operator:test@example.com", DecisionResolveInput{RequestID: request.ID, Kind: DecisionRequestAnswered, Option: "A", Text: "option B", Responder: "operator"}); err != nil {
		t.Fatal(err)
	}
	// Simulate the previous release: no mark and no link on the answered question.
	if _, err := s.pool.Exec(ctx, `UPDATE dots_results SET answered_at=NULL,answered_by=NULL,answer_comment_id=NULL,answer_text=NULL,ask_operator=NULL WHERE task_id=$1 AND kind='QUESTION'`, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET refs=refs #- '{decision_request,dots_question_comment_id}' WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	// A later plain question repeats the text after the decision was requested.
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: body, IdempotencyKey: "repeat", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyDotsQuestionAnswerMigration(ctx, tx); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 3 {
		t.Fatalf("replies=%+v err=%v", replies, err)
	}
	if replies[1].Answer == nil || replies[1].Answer.Text != "option B" {
		t.Fatalf("the decision's question was not backfilled: %+v", replies[1])
	}
	if replies[2].Answer != nil {
		t.Fatalf("the later repeat was closed by the backfill: %+v", replies[2])
	}
	// The later repeat is still answerable.
	res, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "the repeat", RequesterLane: "requester"})
	if err != nil || res.Question == nil || res.Question.CommentID != replies[2].ID {
		t.Fatalf("later repeat answer=%+v err=%v", res, err)
	}
}

// Invariant (hk 5000): the migration classifies every legacy QUESTION by the
// reply's own record, so a question answered by an earlier decision cycle is
// closed even when the task's current request has since been replaced.
func TestDotsQuestionAnswerBackfillEarlierCycle(t *testing.T) {
	s := schemaStore(t, "dots_answer_backfill_cycles")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	for i, q := range []string{"first decision?", "second decision?"} {
		key := fmt.Sprintf("asked-%d", i)
		if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: q, AskOperator: true, IdempotencyKey: key, RunToken: started.RunToken}); err != nil {
			t.Fatal(err)
		}
		current, _, _ := s.GetTask(ctx, task.ID)
		request := current.Refs.DecisionRequest
		if _, err := s.ResolveDecisionRequest(ctx, task.ID, "operator:test@example.com", DecisionResolveInput{RequestID: request.ID, Kind: DecisionRequestAnswered, Option: "A", Text: "option B", Responder: "operator"}); err != nil {
			t.Fatal(err)
		}
	}
	// The previous release left every QUESTION without an owner or answer mark.
	if _, err := s.pool.Exec(ctx, `UPDATE dots_results SET answered_at=NULL,answered_by=NULL,answer_comment_id=NULL,answer_text=NULL,ask_operator=NULL WHERE task_id=$1 AND kind='QUESTION'`, task.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyDotsQuestionAnswerMigration(ctx, tx); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 3 || replies[1].Answer == nil || replies[2].Answer == nil {
		t.Fatalf("earlier cycle stayed open: replies=%+v err=%v", replies, err)
	}
	// Nothing plain is open and the last question went to the operator, so a
	// repeated answer is the decision-path refusal.
	if _, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "again", RequesterLane: "requester"}); !errors.Is(err, ErrDecisionRequestResolved) {
		t.Fatalf("repeated answer: %v", err)
	}
}

// Invariant (hk 5000): legacy rows are classified from the reply payload, so
// two same-text questions keep their owners; the plain one stays answerable
// while the decision's own question stays open in dots_inbox.
func TestDotsQuestionAnswerBackfillClassifiesOwners(t *testing.T) {
	s := schemaStore(t, "dots_answer_backfill_owners")
	ctx := t.Context()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	const body = "Same question?"
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: body, AskOperator: true, IdempotencyKey: "asked", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "QUESTION", Body: body, IdempotencyKey: "plain", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE dots_results SET answered_at=NULL,answered_by=NULL,answer_comment_id=NULL,answer_text=NULL,ask_operator=NULL WHERE task_id=$1 AND kind='QUESTION'`, task.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyDotsQuestionAnswerMigration(ctx, tx); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := s.ListDotsResults(ctx, task.ID, "", 10)
	if err != nil || len(results) != 2 {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	// Results are newest first: the plain repeat then the decision's question.
	if results[0].AskOperator == nil || *results[0].AskOperator || results[1].AskOperator == nil || !*results[1].AskOperator {
		t.Fatalf("owners=%+v / %+v", results[0].AskOperator, results[1].AskOperator)
	}
	replies, err := s.ListDotsReplies(ctx, task.ID)
	if err != nil || len(replies) != 3 {
		t.Fatalf("replies=%+v err=%v", replies, err)
	}
	// The plain repeat is answerable by id; the decision stays open.
	res, err := s.AnswerDotsQuestion(ctx, task.ID, "caller", DotsAnswerInput{Text: "the plain one", RequesterLane: "requester", QuestionID: replies[2].ID})
	if err != nil || res.Question == nil || res.Question.CommentID != replies[2].ID {
		t.Fatalf("plain answer=%+v err=%v", res, err)
	}
	still, _, _ := s.GetTask(ctx, task.ID)
	if still.Refs.DecisionRequest == nil || still.Refs.DecisionRequest.Status != DecisionRequestOpen {
		t.Fatalf("decision closed by a plain answer: %+v", still.Refs.DecisionRequest)
	}
}
