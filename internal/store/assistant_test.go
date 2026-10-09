package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"
)

// MGH-36 PR-1 assistant-path tests. Each invariant below is the sentence a
// mutant must break.

func assistantTask(t *testing.T, s *Store, state string) Task {
	t.Helper()
	return decisionTask(t, s, state)
}

func assistantRecord(t *testing.T, s *Store, id int64) DecisionRequest {
	t.Helper()
	got, err := s.RecordDecisionRequest(context.Background(), id, "panewire-test", decisionInput())
	if err != nil {
		t.Fatal(err)
	}
	return got.Request
}

func outboxRows(t *testing.T, s *Store) []NotificationOutbox {
	t.Helper()
	rows, err := s.ListUnsentNotifications(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Invariant: the assistant path resolves with kind answered only; every other
// kind is a named refusal that writes nothing.
func TestAssistantResolveKindRefused(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	for _, kind := range []string{DecisionRequestDefaultApplied, DecisionRequestWithdrawn, "bogus"} {
		task := assistantTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Kind: kind, Option: "A", Text: "x"})
		if !errors.Is(err, ErrDecisionAssistantKind) {
			t.Fatalf("kind=%s err=%v", kind, err)
		}
		got, _, _ := s.GetTask(ctx, task.ID)
		if got.Refs.DecisionRequest.Status != DecisionRequestOpen || got.Refs.DecisionRequest.Resolution != nil {
			t.Fatalf("kind=%s wrote a resolution: %+v", kind, got.Refs.DecisionRequest)
		}
		if n := len(outboxRows(t, s)); n != 0 {
			t.Fatalf("kind=%s left %d outbox rows", kind, n)
		}
	}
}

// Invariant: an open request on a disposition item is refused by the
// assistant path even though the generic resolve path allows it.
func TestAssistantResolveRefusesDisposition(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	// A disposition item keeps its task row; refs.disposition is what the
	// guard reads. Injecting the shape directly keeps the request open on a
	// task that is indistinguishable from a real disposition item.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET refs=refs||'{"disposition":{"schema":"disposition/v1","facts":{"facts_source":"gh-pr-view","facts_as_of":"2026-10-09T00:00:00Z","install":{"state":"unknown"},"residual_n":0},"recommended":"E"}}'::jsonb WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A"})
	if !errors.Is(err, ErrDispositionOperatorOnly) {
		t.Fatalf("disposition assistant resolve err=%v", err)
	}
	got, _, _ := s.GetTask(ctx, task.ID)
	if got.Refs.DecisionRequest.Status != DecisionRequestOpen {
		t.Fatalf("disposition request closed: %+v", got.Refs.DecisionRequest)
	}
	if n := len(outboxRows(t, s)); n != 0 {
		t.Fatalf("disposition left %d outbox rows", n)
	}
}

// Invariant: the assistant path refuses an open request on a merged or
// dropped task — the pending list already calls that uncleaned state, not
// answerable work. The refusal writes nothing; the operator path keeps its
// cleanup meaning and still closes the request.
func TestAssistantResolveRefusesTerminalTask(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	for _, state := range []string{"merged", "dropped"} {
		task := assistantTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		if state == "merged" {
			if _, err := s.TransitionTask(ctx, task.ID, "verifying", "dr-test", "step", nil, ""); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.TransitionTask(ctx, task.ID, state, "dr-test", "finish", nil, ""); err != nil {
			t.Fatal(err)
		}
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A"})
		if !errors.Is(err, ErrTaskTerminal) {
			t.Fatalf("state=%s assistant resolve err=%v", state, err)
		}
		got, _, _ := s.GetTask(ctx, task.ID)
		if got.Refs.DecisionRequest.Status != DecisionRequestOpen || got.Refs.DecisionRequest.Resolution != nil {
			t.Fatalf("state=%s request mutated: %+v", state, got.Refs.DecisionRequest)
		}
		closed, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", DecisionResolveInput{RequestID: request.ID, Kind: DecisionRequestAnswered, Option: "A", Responder: "operator"})
		if err != nil || closed.Request.Status != DecisionRequestAnswered {
			t.Fatalf("state=%s operator cleanup resolve=%+v err=%v", state, closed.Request.Status, err)
		}
	}
	if n := len(outboxRows(t, s)); n != 0 {
		t.Fatalf("terminal resolves left %d outbox rows", n)
	}
}

// Invariant: a request its producer marked human_only is refused by the
// assistant path; the human path still closes it.
func TestAssistantResolveRefusesHumanOnly(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	in := decisionInput()
	in.HumanOnly = true
	got, err := s.RecordDecisionRequest(ctx, task.ID, "panewire-test", in)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Request.HumanOnly {
		t.Fatalf("human_only not recorded: %+v", got.Request)
	}
	_, err = s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: got.Request.ID, Option: "A"})
	if !errors.Is(err, ErrDecisionRequestHumanOnly) {
		t.Fatalf("human_only assistant resolve err=%v", err)
	}
	if n := len(outboxRows(t, s)); n != 0 {
		t.Fatalf("human_only left %d outbox rows", n)
	}
	// The same request answers fine through the operator path.
	closed, err := s.ResolveDecisionRequest(ctx, task.ID, "director-1", DecisionResolveInput{RequestID: got.Request.ID, Kind: DecisionRequestAnswered, Option: "A", Responder: "operator"})
	if err != nil || closed.Request.Status != DecisionRequestAnswered {
		t.Fatalf("operator resolve=%+v err=%v", closed.Request.Status, err)
	}
}

// Invariant: an assistant resolution records Responder=operator-via-berry and
// By=<the caller identity the server was given> — caller input cannot carry
// either, and exactly one deterministic outbox row exists per resolution.
func TestAssistantResolveAttributionAndOutbox(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	got, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "B", Text: "B fits"})
	if err != nil {
		t.Fatal(err)
	}
	res := got.Request.Resolution
	if res == nil || res.Kind != DecisionRequestAnswered || res.Option != "B" || res.Responder != DecisionAssistantResponder || res.By != "berry-mcp" {
		t.Fatalf("resolution=%+v", res)
	}
	eventID := "dr-" + strconv.FormatInt(task.ID, 10) + "-1-answered"
	rows := outboxRows(t, s)
	if len(rows) != 1 || rows[0].EventID != eventID || rows[0].Kind != OutboxKindDecisionAnswered || rows[0].TargetLane != task.Lane || rows[0].SentAt != nil {
		t.Fatalf("outbox=%+v", rows)
	}
	// Repeating the same resolution is a duplicate and writes no second row.
	dup, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "B", Text: "B fits"})
	if err != nil || !dup.Duplicate {
		t.Fatalf("repeat=%+v err=%v", dup, err)
	}
	if rows := outboxRows(t, s); len(rows) != 1 {
		t.Fatalf("repeat left %d outbox rows", len(rows))
	}
	// A different answer is a conflict, also without a second row.
	_, err = s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "C"})
	if !errors.Is(err, ErrDecisionRequestResolved) {
		t.Fatalf("different answer err=%v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE event_id=$1`, eventID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("outbox count=%d err=%v", count, err)
	}
}

// Invariant: the pending list is open requests on live tasks (backlog and
// in_progress included, merged and dropped excluded), plus pending chat
// questions with their revision, plus the server clock.
func TestAssistantPendingList(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	backlog := assistantTask(t, s, "backlog")
	backlogReq := assistantRecord(t, s, backlog.ID)
	work := assistantTask(t, s, "in_progress")
	workReq := assistantRecord(t, s, work.ID)
	merged := assistantTask(t, s, "in_progress")
	assistantRecord(t, s, merged.ID)
	for _, to := range []string{"verifying", "merged"} {
		if _, err := s.TransitionTask(ctx, merged.ID, to, "dr-test", "step", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	dropped := assistantTask(t, s, "in_progress")
	assistantRecord(t, s, dropped.ID)
	if _, err := s.TransitionTask(ctx, dropped.ID, "dropped", "dr-test", "abandon", nil, ""); err != nil {
		t.Fatal(err)
	}
	// An answered request is not pending work.
	answered := assistantTask(t, s, "in_progress")
	answeredReq := assistantRecord(t, s, answered.ID)
	if _, err := s.ResolveDecisionRequestAssistant(ctx, answered.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: answeredReq.ID, Option: "A"}); err != nil {
		t.Fatal(err)
	}
	qid := fmt.Sprintf("Q-%s-99%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	question, created, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "b618-pending", Body: "which lane?"})
	if err != nil || !created {
		t.Fatalf("seed question=%+v created=%t err=%v", question, created, err)
	}
	pending, err := s.ListAssistantPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending.ServerTime.IsZero() {
		t.Fatalf("server_time unset")
	}
	seen := map[int64]string{}
	for _, d := range pending.DecisionRequests {
		seen[d.TaskID] = d.Request.ID
		if d.Request.Status != DecisionRequestOpen {
			t.Fatalf("non-open request listed: %+v", d)
		}
	}
	if seen[backlog.ID] != backlogReq.ID || seen[work.ID] != workReq.ID {
		t.Fatalf("live requests missing: %+v", seen)
	}
	for _, id := range []int64{merged.ID, dropped.ID, answered.ID} {
		if _, listed := seen[id]; listed {
			t.Fatalf("task %d listed but is not pending work", id)
		}
	}
	found := false
	for _, q := range pending.ChatQuestions {
		if q.ID == qid {
			found = true
			if q.Revision != 1 {
				t.Fatalf("pending question revision=%d", q.Revision)
			}
		}
	}
	if !found {
		t.Fatalf("pending question %s not listed", qid)
	}
}

// Invariant: mark-sent is idempotent per event_id, records hub_row_id once,
// and a differing row id is a conflict — never a silent overwrite.
func TestNotificationOutboxMarkSent(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	if _, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A"}); err != nil {
		t.Fatal(err)
	}
	eventID := request.ID + "-answered"
	if _, err := s.MarkNotificationSent(ctx, eventID, 0); err == nil {
		t.Fatalf("hub_row_id 0 accepted")
	}
	if rows := outboxRows(t, s); len(rows) != 1 {
		t.Fatalf("id-0 mark left sent rows=%d", len(rows))
	}
	sent, err := s.MarkNotificationSent(ctx, eventID, 4242)
	if err != nil || sent.SentAt == nil || sent.HubRowID == nil || *sent.HubRowID != 4242 {
		t.Fatalf("mark=%+v err=%v", sent, err)
	}
	if rows := outboxRows(t, s); len(rows) != 0 {
		t.Fatalf("sent row still unsent: %+v", rows)
	}
	again, err := s.MarkNotificationSent(ctx, eventID, 4242)
	if err != nil || !again.SentAt.Equal(*sent.SentAt) {
		t.Fatalf("repeat mark=%+v err=%v", again, err)
	}
	if _, err := s.MarkNotificationSent(ctx, eventID, 7777); !errors.Is(err, ErrOutboxConflict) {
		t.Fatalf("different row id err=%v", err)
	}
	if _, err := s.MarkNotificationSent(ctx, "dr-999999-1-answered", 1); !errors.Is(err, ErrOutboxEventNotFound) {
		t.Fatalf("unknown event err=%v", err)
	}
}

// Invariant: the upsert raises revision by exactly one only when body or lane
// actually changes; a same-content re-send and a transition leave it alone.
func TestChatQuestionRevisionUpsert(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	qid := fmt.Sprintf("Q-%s-88%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	upsert := func(lane, body string) ChatQuestion {
		t.Helper()
		q, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: lane, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	if q := upsert("lane-a", "first"); q.Revision != 1 {
		t.Fatalf("insert revision=%d", q.Revision)
	}
	if q := upsert("lane-a", "first"); q.Revision != 1 {
		t.Fatalf("same content revision=%d", q.Revision)
	}
	if q := upsert("lane-a", "second"); q.Revision != 2 {
		t.Fatalf("body change revision=%d", q.Revision)
	}
	if q := upsert("lane-b", "second"); q.Revision != 3 {
		t.Fatalf("lane change revision=%d", q.Revision)
	}
	if q := upsert("lane-b", "second"); q.Revision != 3 {
		t.Fatalf("repeat revision=%d", q.Revision)
	}
}

// Invariant: an assistant answer takes the question's one slot inside the
// message transaction — pending + expected revision + empty slot — and writes
// one deterministic outbox row with it. Anything else rolls everything back.
func TestAssistantChatAnswerCAS(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	qid := fmt.Sprintf("Q-%s-77%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "pick one"}); err != nil {
		t.Fatal(err)
	}
	post := func(event string, revision int) error {
		t.Helper()
		_, _, err := s.PostChatMessage(ctx, ChatMessagePost{
			Message: ChatMessage{Author: "operator", Body: "answer " + event, SourceChannel: "assistant", OriginEventID: event},
			Answers: []ChatAnswerInput{{QuestionID: qid, Revision: revision}},
		})
		return err
	}
	if err := post("assistant-ev-1", 1); err != nil {
		t.Fatal(err)
	}
	q, _, _ := s.GetChatQuestion(ctx, qid)
	if q.AnswerMessageID == nil || q.State != "pending" {
		t.Fatalf("answered question=%+v", q)
	}
	eventID := qid + "-rev1-answered"
	var outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE event_id=$1 AND kind='chat_answer'`, eventID).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("chat outbox=%d err=%v", outbox, err)
	}
	// The slot is taken: a second answer at the same revision is stale.
	if err := post("assistant-ev-2", 1); !errors.Is(err, ErrChatQuestionStale) {
		t.Fatalf("second answer err=%v", err)
	}
	var messages int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_messages WHERE origin_event_id='assistant-ev-2'`).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("rolled-back message left=%d err=%v", messages, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("outbox rows=%d err=%v", outbox, err)
	}
}

// Invariant: a stale revision or a non-pending question refuses the whole
// post — no message row, no relation, no outbox row survives the rollback.
func TestAssistantChatAnswerStaleWritesNothing(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	stale := fmt.Sprintf("Q-%s-66%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	resolved := fmt.Sprintf("Q-%s-66%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000+1)
	withdrawn := fmt.Sprintf("Q-%s-66%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000+2)
	for i, id := range []string{stale, resolved, withdrawn} {
		if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: id, Lane: "desk-lane", Body: fmt.Sprintf("q%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	// Bump the stale question's revision so revision=1 is behind.
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: stale, Lane: "desk-lane", Body: "edited"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionChatQuestion(ctx, resolved, "resolved"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TransitionChatQuestion(ctx, withdrawn, "withdrawn"); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		id       string
		revision int
	}{{stale, 1}, {resolved, 1}, {withdrawn, 1}} {
		event := fmt.Sprintf("stale-ev-%d", i)
		_, _, err := s.PostChatMessage(ctx, ChatMessagePost{
			Message: ChatMessage{Author: "operator", Body: "stale", SourceChannel: "assistant", OriginEventID: event},
			Answers: []ChatAnswerInput{{QuestionID: tc.id, Revision: tc.revision}},
		})
		if !errors.Is(err, ErrChatQuestionStale) {
			t.Fatalf("case %d err=%v", i, err)
		}
		var messages int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_messages WHERE origin_event_id=$1`, event).Scan(&messages); err != nil || messages != 0 {
			t.Fatalf("case %d left message rows=%d err=%v", i, messages, err)
		}
	}
	var outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&outbox); err != nil || outbox != 0 {
		t.Fatalf("outbox rows=%d err=%v", outbox, err)
	}
}

// Invariant: two concurrent assistant answers at one revision produce exactly
// one accepted message, one answer_message_id and one outbox row — the loser's
// transaction contributes nothing.
func TestAssistantChatAnswerConcurrent(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	qid := fmt.Sprintf("Q-%s-55%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "race me"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, results[i] = s.PostChatMessage(ctx, ChatMessagePost{
				Message: ChatMessage{Author: "operator", Body: fmt.Sprintf("answer %d", i), SourceChannel: "assistant", OriginEventID: fmt.Sprintf("race-ev-%d", i)},
				Answers: []ChatAnswerInput{{QuestionID: qid, Revision: 1}},
			})
		}(i)
	}
	close(start)
	wg.Wait()
	success, stale := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrChatQuestionStale):
			stale++
		default:
			t.Fatalf("unexpected err=%v", err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("success=%d stale=%d", success, stale)
	}
	q, _, _ := s.GetChatQuestion(ctx, qid)
	if q.AnswerMessageID == nil {
		t.Fatalf("no answer recorded")
	}
	var messages, outbox, relations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_messages WHERE source_channel='assistant'`).Scan(&messages); err != nil || messages != 1 {
		t.Fatalf("assistant messages=%d err=%v", messages, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_message_questions WHERE question_id=$1 AND relation_kind='reply'`, qid).Scan(&relations); err != nil || relations != 1 {
		t.Fatalf("answer relations=%d err=%v", relations, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE event_id=$1`, qid+"-rev1-answered").Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("outbox=%d err=%v", outbox, err)
	}
}

// Invariant: a human operator reply relates to the question but never fills
// the answer slot, resolves it, or writes an outbox row — several replies
// stay allowed.
func TestOperatorReplyDoesNotResolveOrFillSlot(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	qid := fmt.Sprintf("Q-%s-44%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "human answers here"}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		_, created, err := s.PostChatMessage(ctx, ChatMessagePost{
			Message:     ChatMessage{Author: "operator", Body: fmt.Sprintf("human reply %d", i), SourceChannel: "web", OriginEventID: fmt.Sprintf("web-ev-%d-%d", time.Now().UnixNano(), i)},
			QuestionIDs: []string{qid},
		})
		if err != nil || !created {
			t.Fatalf("reply %d created=%t err=%v", i, created, err)
		}
	}
	q, _, _ := s.GetChatQuestion(ctx, qid)
	if q.State != "pending" || q.AnswerMessageID != nil {
		t.Fatalf("human reply mutated question=%+v", q)
	}
	var outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&outbox); err != nil || outbox != 0 {
		t.Fatalf("human reply left outbox=%d err=%v", outbox, err)
	}
}

// Invariant: if the transaction fails after the outbox insert, neither the
// resolution nor the outbox row exists — the outbox is inside the writer's
// transaction, never after the commit.
func TestAssistantResolveOutboxRollback(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	// Force a failure strictly after the outbox insert: the decision event
	// write (kind='decision', this task) violates a constraint added for the
	// attempt, so the whole transaction — refs update, outbox row, event —
	// must roll back together. NOT VALID keeps the record's own event row
	// from failing the add; the constraint still checks every new write.
	if _, err := pool.Exec(ctx, fmt.Sprintf(`ALTER TABLE task_events ADD CONSTRAINT assistant_rollback_test CHECK (kind<>'decision' OR task_id<>%d) NOT VALID`, task.ID)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE task_events DROP CONSTRAINT IF EXISTS assistant_rollback_test`)
	}()
	_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A"})
	if err == nil {
		t.Fatalf("forced-failure resolve unexpectedly succeeded")
	}
	got, _, _ := s.GetTask(ctx, task.ID)
	if got.Refs.DecisionRequest.Status != DecisionRequestOpen || got.Refs.DecisionRequest.Resolution != nil {
		t.Fatalf("rolled-back resolution recorded: %+v", got.Refs.DecisionRequest)
	}
	var outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&outbox); err != nil || outbox != 0 {
		t.Fatalf("rolled-back outbox=%d err=%v", outbox, err)
	}
}

// Invariant: adding the Answers hash field must not change what a
// non-assistant payload hashes to — a message stored by pre-v17 code
// replays byte-identically and returns the stored row, never a conflict.
func TestPreV17ChatReplayReturnsStored(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	// v16 hashed exactly this field set after the same id normalization the
	// post path still applies; the Answers key did not exist.
	type qPayload struct{ ID, Lane, Body string }
	v16Hash := func(m ChatMessage, post ChatMessagePost) string {
		post.QuestionIDs = uniqueSorted(post.QuestionIDs)
		post.ProcessedQuestionIDs = uniqueSorted(post.ProcessedQuestionIDs)
		qs := make([]qPayload, 0, len(post.Questions))
		for _, q := range post.Questions {
			qs = append(qs, qPayload{q.ID, q.Lane, q.Body})
		}
		payload, _ := json.Marshal(struct {
			Author, Body                      string
			Questions                         []qPayload
			QuestionIDs, ProcessedQuestionIDs []string
		}{m.Author, m.Body, qs, post.QuestionIDs, post.ProcessedQuestionIDs})
		return fmt.Sprintf("%x", sha256.Sum256(payload))
	}
	seed := func(m ChatMessage, post ChatMessagePost) int64 {
		t.Helper()
		m.ConversationID = ChatConversationID
		if m.Author == "desk" {
			m.RelayState = "not_sent"
		} else {
			m.RelayState = "stored"
		}
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO chat_messages(conversation_id,author,body,source_channel,origin_event_id,origin_timestamp,semantic_hash,relay_state,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
			m.ConversationID, m.Author, m.Body, m.SourceChannel, m.OriginEventID, m.OriginTimestamp, v16Hash(m, post), m.RelayState, time.Now().UTC()).Scan(&id); err != nil {
			t.Fatal(err)
		}
		for _, qid := range post.QuestionIDs {
			var body string
			if err := pool.QueryRow(ctx, `SELECT body FROM chat_questions WHERE id=$1`, qid).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO chat_message_questions(message_id,question_id,relation_kind,question_text) VALUES($1,$2,'reply',$3)`, id, qid, body); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	// A web operator reply to a question, as v16 wrote it.
	qid := fmt.Sprintf("Q-%s-22%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "before v17?"}); err != nil {
		t.Fatal(err)
	}
	web := ChatMessage{Author: "operator", Body: "human reply before v17", SourceChannel: "web", OriginEventID: "v16-replay-web-1"}
	webID := seed(web, ChatMessagePost{Message: web, QuestionIDs: []string{qid}})
	got, created, err := s.PostChatMessage(ctx, ChatMessagePost{Message: web, QuestionIDs: []string{qid}})
	if err != nil || created || got.ID != webID {
		t.Fatalf("web replay id=%d created=%t err=%v", got.ID, created, err)
	}
	if len(got.QuestionRelations) != 1 || got.QuestionRelations[0].QuestionID != qid {
		t.Fatalf("web replay relations=%+v", got.QuestionRelations)
	}
	// A desk claude_stop message, as v16 wrote it.
	stop := ChatMessage{Author: "desk", Body: "stop notice before v17", SourceChannel: "claude_stop", OriginEventID: "v16-replay-stop-1"}
	stopID := seed(stop, ChatMessagePost{Message: stop})
	got, created, err = s.PostChatMessage(ctx, ChatMessagePost{Message: stop})
	if err != nil || created || got.ID != stopID {
		t.Fatalf("claude_stop replay id=%d created=%t err=%v", got.ID, created, err)
	}
}

// Invariant: the chat-answer outbox row lives in the message transaction —
// when one answer in a multi-answer post is stale, the earlier answers'
// outbox rows roll back with the message instead of lingering unsent.
func TestAssistantChatAnswerOutboxRollsBackWithMessage(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	suffix := time.Now().UnixNano() % 1000000
	fresh := fmt.Sprintf("Q-%s-11%06d", time.Now().UTC().Format("20060102"), suffix)
	stale := fmt.Sprintf("Q-%s-12%06d", time.Now().UTC().Format("20060102"), suffix)
	for i, id := range []string{fresh, stale} {
		if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: id, Lane: "desk-lane", Body: fmt.Sprintf("q%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	// Bump the second question's revision so the caller's rev 1 is stale.
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: stale, Lane: "desk-lane", Body: "edited"}); err != nil {
		t.Fatal(err)
	}
	// fresh sorts first, so its answer and outbox insert run before the
	// stale answer fails the whole post.
	_, _, err := s.PostChatMessage(ctx, ChatMessagePost{
		Message: ChatMessage{Author: "operator", Body: "two answers", SourceChannel: "assistant", OriginEventID: "multi-ev-1"},
		Answers: []ChatAnswerInput{{QuestionID: fresh, Revision: 1}, {QuestionID: stale, Revision: 1}},
	})
	if !errors.Is(err, ErrChatQuestionStale) {
		t.Fatalf("multi-answer err=%v", err)
	}
	var messages int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_messages WHERE origin_event_id='multi-ev-1'`).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("rolled-back message left=%d err=%v", messages, err)
	}
	q, _, _ := s.GetChatQuestion(ctx, fresh)
	if q.AnswerMessageID != nil {
		t.Fatalf("rolled-back answer slot set: %+v", q)
	}
	var relations int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_message_questions WHERE question_id=$1`, fresh).Scan(&relations); err != nil || relations != 0 {
		t.Fatalf("rolled-back relations=%d err=%v", relations, err)
	}
	var outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox`).Scan(&outbox); err != nil || outbox != 0 {
		t.Fatalf("rolled-back outbox=%d err=%v", outbox, err)
	}
}
