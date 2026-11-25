package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// dotsChoiceAttachment inserts one attachment row and returns the option
// attachment that names it.
func dotsChoiceAttachment(t *testing.T, s *Store, n int, caption string) DecisionOptionAttachment {
	t.Helper()
	sha := fmt.Sprintf("%064x", n)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO attachments(sha256,size_bytes,mime,original_name,created_by,created_at) VALUES($1,1,'image/png',$2,'operator',now())`, sha, fmt.Sprintf("%d.png", n)); err != nil {
		t.Fatal(err)
	}
	return DecisionOptionAttachment{SHA256: sha, Caption: caption}
}

// dotsChoiceRequest files a declaring request with two options and starts a
// run on it.
func dotsChoiceRequest(t *testing.T, s *Store, body string) (Task, DotsReplyResult) {
	t.Helper()
	first := dotsChoiceAttachment(t, s, 1, "첫 번째 카드")
	second := dotsChoiceAttachment(t, s, 2, "두 번째 카드")
	task, err := s.CreateDotsRequest(t.Context(), "caller", DotsRequestInput{
		Title:         "카드 결제 화면 시안",
		Body:          body,
		RequesterLane: "requester",
		Options: []DecisionOption{
			{Key: "A", Label: "시안 A", Attachments: []DecisionOptionAttachment{first}},
			{Key: "B", Label: "시안 B", Attachments: []DecisionOptionAttachment{second}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	return task, started
}

// Invariant (hk 4789): a declaring request's RESULT records the result as
// today, opens one operator decision with the declared options (and their
// materials), dots's recommendation, its reason, its unverified list and its
// one fix, and leaves the task waiting instead of completing it.
func TestDotsChoiceResultOpensDecision(t *testing.T) {
	s := schemaStore(t, "dots_choice_open")
	ctx := context.Background()
	task, started := dotsChoiceRequest(t, s, "Investigate\n[알릴 레인] lane-a")
	first := task.Refs.DotsChoice
	if first == nil || len(first.Options) != 2 || first.Options[0].Key != "A" || first.Options[1].Attachments[0].Caption != "두 번째 카드" {
		t.Fatalf("declaration not stored: %+v", first)
	}
	result, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{
		TaskID: task.ID, Kind: "RESULT", Body: "두 시안을 만들었습니다", IdempotencyKey: "result", RunToken: started.RunToken,
		Recommended:          "B",
		RecommendationReason: "B가 대비가 좋습니다",
		Unverified:           []string{"모바일 실기기 확인 안 함"},
		OneFix:               "결제 버튼 대비를 한 단계 올리세요",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.State == StateCompleted {
		t.Fatalf("declaring request completed on RESULT: %+v", result)
	}
	current, found, err := s.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if current.State == StateCompleted || !current.AwaitingDecision {
		t.Fatalf("task state=%s awaiting=%v, want open and awaiting", current.State, current.AwaitingDecision)
	}
	request := current.Refs.DecisionRequest
	if request == nil || request.Status != DecisionRequestOpen {
		t.Fatalf("request=%+v", request)
	}
	if request.Doc != result.DocumentKey {
		t.Fatalf("doc=%q want the RESULT document %q", request.Doc, result.DocumentKey)
	}
	if request.RecommendationReason != "B가 대비가 좋습니다" || len(request.Unverified) != 1 || request.OneFix != "결제 버튼 대비를 한 단계 올리세요" {
		t.Fatalf("material=%+v", request)
	}
	options := current.Refs.DecisionOptions
	if options == nil || !options.AllowFree || len(options.Options) != 2 {
		t.Fatalf("options=%+v", options)
	}
	if options.Options[0].Recommended || !options.Options[1].Recommended {
		t.Fatalf("recommended marks the wrong option: %+v", options.Options)
	}
	if options.Options[1].Attachments[0].SHA256 != first.Options[1].Attachments[0].SHA256 {
		t.Fatalf("option material lost: %+v", options.Options[1])
	}
	if history := DecisionRequestHistory(current); len(history) != 1 || history[0].Request.ID != request.ID {
		t.Fatalf("history=%+v", history)
	}
	// The RESULT still reaches the requester and the named lane as today.
	if n := notifyRows(t, s, "requester", OutboxKindDotsReport); n != 1 {
		t.Fatalf("requester RESULT rows=%d", n)
	}
	if n := notifyRows(t, s, "lane-a", OutboxKindDotsReport); n != 1 {
		t.Fatalf("lane-a RESULT rows=%d", n)
	}
}

// dotsChoiceAnsweredRequest runs a declaring request to its RESULT and returns
// the task and the opened decision.
func dotsChoiceAnsweredRequest(t *testing.T, s *Store, prefix string) (Task, DotsReplyResult) {
	t.Helper()
	task, started := dotsChoiceRequest(t, s, "Investigate\n[알릴 레인] lane-a")
	result, err := s.ReplyDots(t.Context(), "operator:test@example.com", "chatgpt", DotsReplyInput{
		TaskID: task.ID, Kind: "RESULT", Body: "두 시안을 만들었습니다", IdempotencyKey: prefix, RunToken: started.RunToken,
		Recommended: "A", RecommendationReason: "A가 더 단순합니다",
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _, err := s.GetTask(t.Context(), task.ID)
	if err != nil || current.Refs.DecisionRequest == nil {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	return current, result
}

// Invariant (hk 4789): the answer to a declaring request's decision is the
// request's result. Each answer path completes the task with the decision and
// the answer as evidence and enqueues exactly one requester notification; a
// replayed answer enqueues none.
func TestDotsChoiceAnswerCompletesAndDelivers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  func(t *testing.T, s *Store, task Task) error
		respond func(t *testing.T, s *Store, task Task) error
	}{
		{
			name: "decision_answer",
			answer: func(t *testing.T, s *Store, task Task) error {
				// The chatgpt channel is how dots records the operator's words.
				_, err := s.ResolveDecision(context.Background(), ResolveDecisionInput{
					RequestID: task.Refs.DecisionRequest.ID, Kind: DecisionRequestAnswered, Option: "B",
					Text: "B로 갑시다", Channel: DecisionChannelDots, Actor: "operator:test@example.com", EventKey: "answer-1",
				})
				return err
			},
			respond: func(t *testing.T, s *Store, task Task) error {
				out, err := s.ResolveDecision(context.Background(), ResolveDecisionInput{
					RequestID: task.Refs.DecisionRequest.ID, Kind: DecisionRequestAnswered, Option: "B",
					Text: "B로 갑시다", Channel: DecisionChannelDots, Actor: "operator:test@example.com", EventKey: "answer-1",
				})
				if err == nil && !out.Duplicate {
					t.Fatalf("replay did not report a duplicate: %+v", out)
				}
				return err
			},
		},
		{
			name: "console",
			answer: func(t *testing.T, s *Store, task Task) error {
				_, err := s.ResolveDecision(context.Background(), ResolveDecisionInput{
					RequestID: task.Refs.DecisionRequest.ID, Kind: DecisionRequestAnswered, Option: "B",
					Text: "B로 갑시다", Channel: DecisionChannelConsole, Actor: "operator:robin@example.com", Verified: true, EventKey: "console-1",
				})
				return err
			},
			respond: func(t *testing.T, s *Store, task Task) error {
				out, err := s.ResolveDecision(context.Background(), ResolveDecisionInput{
					RequestID: task.Refs.DecisionRequest.ID, Kind: DecisionRequestAnswered, Option: "B",
					Text: "B로 갑시다", Channel: DecisionChannelConsole, Actor: "operator:robin@example.com", Verified: true, EventKey: "console-1",
				})
				if err == nil && !out.Duplicate {
					t.Fatalf("replay did not report a duplicate: %+v", out)
				}
				return err
			},
		},
		{
			name: "dots_answer",
			answer: func(t *testing.T, s *Store, task Task) error {
				_, err := s.AnswerDotsQuestion(context.Background(), task.ID, "operator:test@example.com", DotsAnswerInput{Text: "B로 갑시다"})
				return err
			},
			respond: func(t *testing.T, s *Store, task Task) error {
				// A second answer writes nothing: the task is already closed.
				_, err := s.AnswerDotsQuestion(context.Background(), task.ID, "operator:test@example.com", DotsAnswerInput{Text: "B로 갑시다"})
				if !errors.Is(err, ErrTaskTerminal) && !errors.Is(err, ErrDecisionRequestResolved) {
					t.Fatalf("second answer err=%v", err)
				}
				return nil
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := schemaStore(t, "dots_choice_answer")
			task, result := dotsChoiceAnsweredRequest(t, s, "result")
			if err := tc.answer(t, s, task); err != nil {
				t.Fatal(err)
			}
			done, found, err := s.GetTask(context.Background(), task.ID)
			if err != nil || !found {
				t.Fatalf("found=%v err=%v", found, err)
			}
			if done.State != StateCompleted || done.AwaitingDecision {
				t.Fatalf("state=%s awaiting=%v, want completed", done.State, done.AwaitingDecision)
			}
			if done.Refs.Closure == nil || !strings.Contains(done.Refs.Closure.Evidence, done.Refs.DecisionRequest.ID) || !strings.Contains(done.Refs.Closure.Evidence, "B로 갑시다") {
				t.Fatalf("closure=%+v", done.Refs.Closure)
			}
			rows, err := s.pool.Query(context.Background(), `SELECT target_lane,event_id,text FROM notification_outbox WHERE starts_with(event_id,'dots-answer-') ORDER BY id`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			type answerRow struct{ lane, eventID, text string }
			var got []answerRow
			for rows.Next() {
				var row answerRow
				if err := rows.Scan(&row.lane, &row.eventID, &row.text); err != nil {
					t.Fatal(err)
				}
				got = append(got, row)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("answer notifications=%+v, want requester and named lane once each", got)
			}
			if got[0].lane != "requester" || got[1].lane != "lane-a" {
				t.Fatalf("lanes=%+v", got)
			}
			for _, row := range got {
				if !strings.Contains(row.text, done.Refs.DecisionRequest.ID) || !strings.Contains(row.text, "B로 갑시다") || !strings.Contains(row.text, result.DocumentKey) {
					t.Fatalf("answer notification text=%q", row.text)
				}
			}
			// A replayed answer writes nothing.
			if err := tc.respond(t, s, task); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_outbox WHERE starts_with(event_id,'dots-answer-')`).Scan(&n); err != nil || n != 2 {
				t.Fatalf("rows after replay=%d err=%v", n, err)
			}
		})
	}
}

// Invariant (hk 4789): a request with no declaration behaves exactly as
// before; its RESULT completes the task and opens no decision.
func TestDotsNoDeclarationCompletesOnResult(t *testing.T) {
	s := schemaStore(t, "dots_choice_none")
	ctx := context.Background()
	task, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester"})
	if err != nil {
		t.Fatal(err)
	}
	if task.Refs.DotsChoice != nil {
		t.Fatalf("undeclared request carries a choice: %+v", task.Refs.DotsChoice)
	}
	started, _ := startDotsRun(t, s, task.ID, "start")
	result, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "done", IdempotencyKey: "result", RunToken: started.RunToken})
	if err != nil || result.State != StateCompleted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	current, _, err := s.GetTask(ctx, task.ID)
	if err != nil || current.Refs.DecisionRequest != nil || current.AwaitingDecision {
		t.Fatalf("task=%+v err=%v", current, err)
	}
	if n := notifyRows(t, s, "requester", OutboxKindDotsReport); n != 1 {
		t.Fatalf("requester rows=%d", n)
	}
}

// Invariant (hk 4789): a declaration is validated at creation, so a RESULT is
// never refused for it. An address no attachment row holds, a recommended
// flag, a malformed option and secret-like text are all refused up front.
func TestDotsChoiceRefusedAtCreation(t *testing.T) {
	s := schemaStore(t, "dots_choice_bad")
	ctx := context.Background()
	missing := DecisionOptionAttachment{SHA256: strings.Repeat("a", 64)}
	for _, tc := range []struct {
		name    string
		options []DecisionOption
		want    error
	}{
		{name: "unknown attachment", options: []DecisionOption{{Key: "A", Label: "시안", Attachments: []DecisionOptionAttachment{missing}}}, want: ErrDecisionAttachmentNotFound},
		{name: "declaration recommends", options: []DecisionOption{{Key: "A", Label: "시안", Recommended: true}}},
		{name: "key outside A-F", options: []DecisionOption{{Key: "G", Label: "시안"}}},
		{name: "duplicate key", options: []DecisionOption{{Key: "A", Label: "하나"}, {Key: "A", Label: "둘"}}},
		{name: "too many options", options: []DecisionOption{{Key: "A", Label: "1"}, {Key: "B", Label: "2"}, {Key: "C", Label: "3"}, {Key: "D", Label: "4"}, {Key: "E", Label: "5"}, {Key: "F", Label: "6"}, {Key: "A", Label: "7"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate", RequesterLane: "requester", Options: tc.options})
			if err == nil {
				t.Fatal("bad declaration accepted")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err=%v want %v", err, tc.want)
			}
		})
	}
	var tasks int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE lane='dots'`).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("refused declarations filed %d tasks err=%v", tasks, err)
	}
}

// Invariant (hk 4789): the RESULT annotations belong to RESULT, name a
// declared key, and mean nothing on a request that declares no options.
func TestDotsChoiceAnnotationsRefused(t *testing.T) {
	s := schemaStore(t, "dots_choice_annotations")
	ctx := context.Background()
	task, started := dotsChoiceRequest(t, s, "Investigate")
	for _, tc := range []struct {
		name string
		in   DotsReplyInput
	}{
		{name: "unknown recommended key", in: DotsReplyInput{Kind: "RESULT", Body: "done", Recommended: "C", IdempotencyKey: "unknown"}},
		{name: "recommended on progress", in: DotsReplyInput{Kind: "PROGRESS", Body: "half", Recommended: "A", IdempotencyKey: "progress"}},
		{name: "one fix on question", in: DotsReplyInput{Kind: "QUESTION", Body: "which?", OneFix: "use A", IdempotencyKey: "question"}},
		{name: "unverified on blocked", in: DotsReplyInput{Kind: "BLOCKED", Body: "stuck", Unverified: []string{"x"}, IdempotencyKey: "blocked"}},
		{name: "reason on start", in: DotsReplyInput{Kind: "START", Body: "start", RecommendationReason: "why", IdempotencyKey: "start", ExpectedUpdatedAt: &task.UpdatedAt}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.TaskID = task.ID
			tc.in.RunToken = started.RunToken
			if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", tc.in); err == nil {
				t.Fatal("annotated reply accepted")
			}
		})
	}
	current, _, err := s.GetTask(ctx, task.ID)
	if err != nil || current.State != started.State || current.Refs.DecisionRequest != nil {
		t.Fatalf("refused replies changed the task: %+v err=%v", current, err)
	}
	// A non-declaring request takes no annotations either: nothing would carry
	// them to a decision.
	plain, err := s.CreateDotsRequest(ctx, "caller", DotsRequestInput{Title: "Research", Body: "Investigate"})
	if err != nil {
		t.Fatal(err)
	}
	plainRun, _ := startDotsRun(t, s, plain.ID, "plain")
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: plain.ID, Kind: "RESULT", Body: "done", Recommended: "A", IdempotencyKey: "plain-result", RunToken: plainRun.RunToken}); err == nil {
		t.Fatal("annotation on a request with no declaration accepted")
	}
}

// Invariant (hk 4789, hk 6339): an answer to a declaring request's decision
// on a task that was settled in the meantime is not recorded. Settling the
// task withdrew the request, so the answer is refused as task_terminal, and
// the requester lane hears nothing.
func TestDotsChoiceAnswerOnSettledTaskResolvesOnly(t *testing.T) {
	s := schemaStore(t, "dots_choice_settled")
	ctx := context.Background()
	task, _ := dotsChoiceAnsweredRequest(t, s, "result")
	if _, err := s.TransitionTaskWith(ctx, task.ID, TransitionInput{To: StateDropped, By: "operator:test@example.com", Note: "not needed any more", Evidence: "declined"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ResolveDecision(ctx, ResolveDecisionInput{
		RequestID: task.Refs.DecisionRequest.ID, Kind: DecisionRequestAnswered, Option: "A", Text: "A로 갑시다",
		Channel: DecisionChannelConsole, Actor: "operator:robin@example.com", Verified: true, EventKey: "console-settled",
	})
	if !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("answer on a settled task err=%v, want task_terminal", err)
	}
	done, _, err := s.GetTask(ctx, task.ID)
	if err != nil || done.State != StateDropped {
		t.Fatalf("state=%s err=%v, want the settled state kept", done.State, err)
	}
	if done.Refs.DecisionRequest == nil || done.Refs.DecisionRequest.Status != DecisionRequestWithdrawn {
		t.Fatalf("request=%+v, want it withdrawn by the settle", done.Refs.DecisionRequest)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE starts_with(event_id,'dots-answer-')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("answer notices=%d err=%v", n, err)
	}
}

// Invariant (hk 4789): a withdrawn decision is not the request's result. The
// request closes, the task stays open for a replacement RESULT, and the
// requester lane hears nothing.
func TestDotsChoiceWithdrawnIsNotAResult(t *testing.T) {
	s := schemaStore(t, "dots_choice_withdrawn")
	ctx := context.Background()
	task, _ := dotsChoiceAnsweredRequest(t, s, "result")
	out, err := s.ResolveDecision(ctx, ResolveDecisionInput{
		RequestID: task.Refs.DecisionRequest.ID, Kind: DecisionRequestWithdrawn, Text: "고르지 않겠습니다",
		Channel: DecisionChannelConsole, Actor: "operator:robin@example.com", Verified: true, EventKey: "console-withdrawn",
	})
	if err != nil || out.Result != DecisionResultAnswered {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	done, _, err := s.GetTask(ctx, task.ID)
	if err != nil || done.State != task.State || done.AwaitingDecision {
		t.Fatalf("task=%+v err=%v, want it open and not awaiting", done, err)
	}
	if done.Refs.DecisionRequest == nil || done.Refs.DecisionRequest.Status != DecisionRequestWithdrawn {
		t.Fatalf("request=%+v, want it withdrawn", done.Refs.DecisionRequest)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE starts_with(event_id,'dots-answer-')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("answer notices=%d err=%v", n, err)
	}
}

// Invariant (hk 4789): dots may park a declaring request (BLOCKED) and then
// post its RESULT; the RESULT still opens the decision. The answer resumes the
// parked request and closes it, and the requester hears the result — a
// completion the state graph would otherwise refuse must not roll the answer
// back.
func TestDotsChoiceAnswerClosesParkedRequest(t *testing.T) {
	s := schemaStore(t, "dots_choice_parked")
	ctx := context.Background()
	task, started := dotsChoiceRequest(t, s, "Investigate")
	blocked, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "BLOCKED", Body: "waiting on a tool", IdempotencyKey: "blocked", RunToken: started.RunToken})
	if err != nil || blocked.State != StateIcebox {
		t.Fatalf("blocked=%+v err=%v", blocked, err)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{
		TaskID: task.ID, Kind: "RESULT", Body: "두 시안 완성", IdempotencyKey: "result", RunToken: started.RunToken,
		Recommended: "A",
	}); err != nil {
		t.Fatal(err)
	}
	parked, _, err := s.GetTask(ctx, task.ID)
	if err != nil || parked.State != StateIcebox || parked.Refs.DecisionRequest == nil {
		t.Fatalf("task=%+v err=%v", parked, err)
	}
	out, err := s.ResolveDecision(ctx, ResolveDecisionInput{
		RequestID: parked.Refs.DecisionRequest.ID, Kind: DecisionRequestAnswered, Option: "A", Text: "A로 갑시다",
		Channel: DecisionChannelConsole, Actor: "operator:robin@example.com", Verified: true, EventKey: "console-parked",
	})
	if err != nil || out.Result != DecisionResultAnswered {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	done, _, err := s.GetTask(ctx, task.ID)
	if err != nil || done.State != StateCompleted {
		t.Fatalf("state=%s err=%v, want completed", done.State, err)
	}
	if n := notifyRows(t, s, "requester", OutboxKindDotsReport); n != 2 {
		t.Fatalf("requester rows=%d, want the RESULT and the answer", n)
	}
}

// Invariant (hk 4789): while another blocking decision keeps the task open, an
// answer to the RESULT decision resumes nothing and notifies nobody; when that
// other decision closes, the reconcile finishes the request exactly once.
func TestDotsChoiceAnswerWaitsForAnotherDecision(t *testing.T) {
	s := schemaStore(t, "dots_choice_second")
	ctx := context.Background()
	task, started := dotsChoiceRequest(t, s, "Investigate")
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "BLOCKED", Body: "waiting", IdempotencyKey: "blocked", RunToken: started.RunToken}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReplyDots(ctx, "operator:test@example.com", "chatgpt", DotsReplyInput{TaskID: task.ID, Kind: "RESULT", Body: "두 시안 완성", IdempotencyKey: "result", RunToken: started.RunToken, Recommended: "A"}); err != nil {
		t.Fatal(err)
	}
	parked, _, err := s.GetTask(ctx, task.ID)
	if err != nil || parked.State != StateIcebox || parked.Refs.DecisionRequest == nil {
		t.Fatalf("task=%+v err=%v", parked, err)
	}
	// A second blocking decision stays open on the task (a flag row left by
	// another producer; it is not the RESULT decision).
	if _, err := s.pool.Exec(ctx, `INSERT INTO decision_requests(source,source_ref,task_id,lane,question,status,created_at,updated_at) VALUES('task_flag','flag-1',$1,'dots','another question','open',now(),now())`, task.ID); err != nil {
		t.Fatal(err)
	}
	out, err := s.ResolveDecision(ctx, ResolveDecisionInput{
		RequestID: parked.Refs.DecisionRequest.ID, Kind: DecisionRequestAnswered, Option: "A", Text: "A로 갑시다",
		Channel: DecisionChannelConsole, Actor: "operator:robin@example.com", Verified: true, EventKey: "console-blocked",
	})
	if err != nil || out.Result != DecisionResultAnswered {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	held, _, err := s.GetTask(ctx, task.ID)
	if err != nil || held.State != StateIcebox || !held.AwaitingDecision {
		t.Fatalf("task=%+v err=%v, want it parked and still awaiting", held, err)
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE starts_with(event_id,'dots-answer-')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("answer notices=%d err=%v, want none while another decision blocks", n, err)
	}
	// The other decision closes. Its resolve clears the flag and re-runs the
	// reconcile; here the close is simulated, and the reconcile runs as it does
	// at the end of every resolving transaction.
	if _, err := s.pool.Exec(ctx, `UPDATE decision_requests SET status='superseded' WHERE source='task_flag' AND source_ref='flag-1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE tasks SET awaiting_decision=false WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	closed, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.reconcileDotsChoiceTx(ctx, pgTransaction{tx}, closed, time.Now().UTC())
	if err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if done.State != StateCompleted {
		t.Fatalf("state=%s, want completed after the other decision closed", done.State)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE starts_with(event_id,'dots-answer-')`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("answer notices=%d err=%v, want exactly one", n, err)
	}
	// A second reconcile sends nothing: the outbox id names the decision and the
	// task is settled.
	tx, err = s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.reconcileDotsChoiceTx(ctx, pgTransaction{tx}, done, time.Now().UTC()); err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE starts_with(event_id,'dots-answer-')`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("answer notices after a second reconcile=%d err=%v", n, err)
	}
}
