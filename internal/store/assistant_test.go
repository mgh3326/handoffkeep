package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	return assistantRecordWith(t, s, id, decisionInput())
}

func assistantRecordWith(t *testing.T, s *Store, id int64, in DecisionRequestInput) DecisionRequest {
	t.Helper()
	got, err := s.RecordDecisionRequest(context.Background(), id, "panewire-test", in)
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

// Invariant: an assistant judgement resolution records
// Responder=assistant-via-berry and By=<the caller identity the server was
// given> — caller input cannot carry either, and exactly one deterministic
// outbox row exists per resolution.
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
	if res == nil || res.Kind != DecisionRequestAnswered || res.Option != "B" || res.Responder != DecisionAssistantJudgementResponder || res.By != "berry-mcp" {
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

// Invariant (hk 1459 / MGH-36 PR-4c): a body revision clears the answer slot
// in the same statement that bumps the revision — the revised question is
// answerable again at the new revision, a rev1 answer is stale forever, and
// the rev1 answer row, relation and outbox row all survive as history.
func TestChatQuestionRevisionClearsAnswer(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	qid := fmt.Sprintf("Q-%s-33%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "revise me"}); err != nil {
		t.Fatal(err)
	}
	answer := func(event string, revision int) (ChatMessage, error) {
		t.Helper()
		m, _, err := s.PostChatMessage(ctx, ChatMessagePost{
			Message: ChatMessage{Author: "operator", Body: "answer " + event, SourceChannel: "assistant", OriginEventID: event},
			Answers: []ChatAnswerInput{{QuestionID: qid, Revision: revision}},
		})
		return m, err
	}
	rev1Answer, err := answer("rev-ev-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	q, _, _ := s.GetChatQuestion(ctx, qid)
	if q.AnswerMessageID == nil || *q.AnswerMessageID != rev1Answer.ID || q.Revision != 1 {
		t.Fatalf("rev1 answered question=%+v", q)
	}
	// A real revision: new body bumps the revision and frees the slot.
	revised, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "revised body"})
	if err != nil {
		t.Fatal(err)
	}
	if revised.Revision != 2 || revised.AnswerMessageID != nil {
		t.Fatalf("revised question=%+v", revised)
	}
	// The rev1 answer row and its outbox row survive — history by revision.
	var messages, relations, rev1Outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_messages WHERE id=$1`, rev1Answer.ID).Scan(&messages); err != nil || messages != 1 {
		t.Fatalf("rev1 message left=%d err=%v", messages, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_message_questions WHERE message_id=$1 AND question_id=$2 AND relation_kind='reply'`, rev1Answer.ID, qid).Scan(&relations); err != nil || relations != 1 {
		t.Fatalf("rev1 relation left=%d err=%v", relations, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE event_id=$1`, qid+"-rev1-answered").Scan(&rev1Outbox); err != nil || rev1Outbox != 1 {
		t.Fatalf("rev1 outbox=%d err=%v", rev1Outbox, err)
	}
	// An answer naming the old revision is stale; the new revision answers.
	if _, err := answer("rev-ev-stale", 1); !errors.Is(err, ErrChatQuestionStale) {
		t.Fatalf("rev1 answer after revision err=%v", err)
	}
	rev2Answer, err := answer("rev-ev-2", 2)
	if err != nil {
		t.Fatalf("rev2 answer err=%v", err)
	}
	q, _, _ = s.GetChatQuestion(ctx, qid)
	if q.AnswerMessageID == nil || *q.AnswerMessageID != rev2Answer.ID || q.Revision != 2 {
		t.Fatalf("rev2 answered question=%+v", q)
	}
	var rev2Outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE event_id=$1`, qid+"-rev2-answered").Scan(&rev2Outbox); err != nil || rev2Outbox != 1 {
		t.Fatalf("rev2 outbox=%d err=%v", rev2Outbox, err)
	}
}

// Invariant: a lane change is a revision and clears the slot the same way,
// while an identical re-upsert changes nothing — revision and
// answer_message_id are kept. Kills the mutants "clear on any upsert" and
// "clear without bumping".
func TestChatQuestionLaneRevisionClearsAnswer(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	qid := fmt.Sprintf("Q-%s-34%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "which lane"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PostChatMessage(ctx, ChatMessagePost{
		Message: ChatMessage{Author: "operator", Body: "lane answer", SourceChannel: "assistant", OriginEventID: "lane-ev-1"},
		Answers: []ChatAnswerInput{{QuestionID: qid, Revision: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	// Identical re-upsert: not a revision, the slot stays filled.
	same, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "which lane"})
	if err != nil {
		t.Fatal(err)
	}
	if same.Revision != 1 || same.AnswerMessageID == nil {
		t.Fatalf("identical re-upsert mutated question=%+v", same)
	}
	// A lane change revises and clears exactly like a body change.
	moved, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "other-lane", Body: "which lane"})
	if err != nil {
		t.Fatal(err)
	}
	if moved.Revision != 2 || moved.AnswerMessageID != nil {
		t.Fatalf("lane-revised question=%+v", moved)
	}
	if _, _, err := s.PostChatMessage(ctx, ChatMessagePost{
		Message: ChatMessage{Author: "operator", Body: "lane answer 2", SourceChannel: "assistant", OriginEventID: "lane-ev-2"},
		Answers: []ChatAnswerInput{{QuestionID: qid, Revision: 2}},
	}); err != nil {
		t.Fatalf("rev2 lane answer err=%v", err)
	}
}

// Invariant: an assistant answer racing a revision ends in exactly one of
// two states — the answer committed under rev1 and the revision cleared the
// slot, or the revision won and the answer is stale. The row never settles
// with answer_message_id pointing at a message written for another
// revision: a real revision always frees the slot it bumps.
func TestAssistantChatAnswerRevisionRace(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	qid := fmt.Sprintf("Q-%s-35%06d", time.Now().UTC().Format("20060102"), time.Now().UnixNano()%1000000)
	if _, _, err := s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "race the edit"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([]error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, _, results[0] = s.PostChatMessage(ctx, ChatMessagePost{
			Message: ChatMessage{Author: "operator", Body: "racing answer", SourceChannel: "assistant", OriginEventID: fmt.Sprintf("race-rev-ev-%d", time.Now().UnixNano())},
			Answers: []ChatAnswerInput{{QuestionID: qid, Revision: 1}},
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, _, results[1] = s.UpsertChatQuestion(ctx, ChatQuestion{ID: qid, Lane: "desk-lane", Body: "edited body"})
	}()
	close(start)
	wg.Wait()
	if results[1] != nil {
		t.Fatalf("revision err=%v", results[1])
	}
	switch {
	case results[0] == nil:
		// The answer committed under rev1; the revision then freed the slot.
	case errors.Is(results[0], ErrChatQuestionStale):
		// The revision won the lock first; the rev1 answer is stale.
	default:
		t.Fatalf("answer err=%v", results[0])
	}
	q, _, _ := s.GetChatQuestion(ctx, qid)
	if q.Revision != 2 || q.AnswerMessageID != nil {
		t.Fatalf("post-race question=%+v", q)
	}
}

// Invariant (provenance contract): an operator_decided answer carrying the
// operator's verbatim quote and this request's exact ref closes a human_only
// request — recorded decided_by=operator with the quote and ref, the outbox
// text names the operator quote, and the decision event note carries both.
func TestAssistantResolveOperatorDecidedHumanOnly(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	in := decisionInput()
	in.HumanOnly = true
	request := assistantRecordWith(t, s, task.ID, in)
	got, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{
		RequestID: request.ID, Option: "A", Text: "operator said A",
		Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "옵션 A로 진행해", DecisionRef: request.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := got.Request.Resolution
	if res == nil || res.Provenance != DecisionProvenanceOperatorDecided || res.DecidedBy != DecisionDecidedByOperator ||
		res.OperatorQuote != "옵션 A로 진행해" || res.DecisionRef != request.ID || res.Responder != DecisionAssistantResponder {
		t.Fatalf("resolution=%+v", res)
	}
	rows := outboxRows(t, s)
	if len(rows) != 1 || !strings.Contains(rows[0].Text, "(operator quote)") {
		t.Fatalf("outbox=%+v", rows)
	}
	var note string
	if err := pool.QueryRow(ctx, `SELECT note FROM task_events WHERE task_id=$1 AND kind='decision' ORDER BY id DESC LIMIT 1`, task.ID).Scan(&note); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "(operator quote)") || !strings.Contains(note, "옵션 A로 진행해") {
		t.Fatalf("event note=%q", note)
	}
}

// Invariant: a human_only request refuses every assistant answer that is not
// an operator_decided claim with the verbatim quote — a bare judgement is
// refused outright, and a claim whose quote is blank collapses to a
// judgement and is refused naming no_operator_quote.
func TestAssistantResolveHumanOnlyProvenanceRefusals(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	in := decisionInput()
	in.HumanOnly = true
	// Bare and explicit judgements: plain human_only refusal.
	for _, prov := range []string{"", DecisionProvenanceAssistantJudgement} {
		task := assistantTask(t, s, "in_progress")
		request := assistantRecordWith(t, s, task.ID, in)
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A", Provenance: prov})
		if !errors.Is(err, ErrDecisionRequestHumanOnly) || strings.Contains(err.Error(), "no_operator_quote") {
			t.Fatalf("prov=%q err=%v", prov, err)
		}
	}
	// operator_decided without the quote is a judgement with a reason.
	for _, quote := range []string{"", "   ", "\t\n "} {
		task := assistantTask(t, s, "in_progress")
		request := assistantRecordWith(t, s, task.ID, in)
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{
			RequestID: request.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: quote, DecisionRef: request.ID,
		})
		if !errors.Is(err, ErrDecisionRequestHumanOnly) || !strings.Contains(err.Error(), "no_operator_quote") {
			t.Fatalf("quote=%q err=%v", quote, err)
		}
		if n := len(outboxRows(t, s)); n != 0 {
			t.Fatalf("quote=%q left %d outbox rows", quote, n)
		}
	}
}

// Invariant: an operator_decided answer whose decision_ref is missing or is
// any id but this exact request id — another task's request, another
// revision — is provenance_ref_mismatch and records nothing.
func TestAssistantResolveProvenanceRefMismatch(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	other := assistantTask(t, s, "in_progress")
	otherReq := assistantRecord(t, s, other.ID)
	for _, ref := range []string{"", otherReq.ID, "dr-999999-1", strings.TrimSuffix(request.ID, "-1") + "-9", request.ID + "-answered"} {
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{
			RequestID: request.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "quoted words", DecisionRef: ref,
		})
		if !errors.Is(err, ErrProvenanceRefMismatch) {
			t.Fatalf("ref=%q err=%v", ref, err)
		}
	}
	got, _, _ := s.GetTask(ctx, task.ID)
	if got.Refs.DecisionRequest.Status != DecisionRequestOpen || got.Refs.DecisionRequest.Resolution != nil {
		t.Fatalf("mismatch recorded a resolution: %+v", got.Refs.DecisionRequest)
	}
	if n := len(outboxRows(t, s)); n != 0 {
		t.Fatalf("mismatch left %d outbox rows", n)
	}
}

// Invariant: a quote never bypasses the disposition refusal — the
// disposition check stands ahead of provenance, so even a well-formed
// operator_decided answer is refused on a disposition item.
func TestAssistantResolveDispositionRefusesQuote(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	if _, err := pool.Exec(ctx, `UPDATE tasks SET refs=refs||'{"disposition":{"schema":"disposition/v1","facts":{"facts_source":"gh-pr-view","facts_as_of":"2026-10-09T00:00:00Z","install":{"state":"unknown"},"residual_n":0},"recommended":"E"}}'::jsonb WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{
		RequestID: request.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "operator said so", DecisionRef: request.ID,
	})
	if !errors.Is(err, ErrDispositionOperatorOnly) {
		t.Fatalf("disposition + quote err=%v", err)
	}
	if n := len(outboxRows(t, s)); n != 0 {
		t.Fatalf("disposition + quote left %d outbox rows", n)
	}
}

// Invariant: a non-human_only request answers under either provenance — a
// judgement records decided_by=assistant, a responder that never names the
// operator, and the (assistant judgement) marker on the outbox text; an
// operator_decided answer on the same kind of request records the operator.
func TestAssistantResolveProvenanceAttribution(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	judge := assistantTask(t, s, "in_progress")
	judgeReq := assistantRecord(t, s, judge.ID)
	got, err := s.ResolveDecisionRequestAssistant(ctx, judge.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: judgeReq.ID, Option: "B", Text: "my call"})
	if err != nil {
		t.Fatal(err)
	}
	res := got.Request.Resolution
	if res == nil || res.Provenance != DecisionProvenanceAssistantJudgement || res.DecidedBy != DecisionDecidedByAssistant ||
		res.Responder != DecisionAssistantJudgementResponder || strings.Contains(res.Responder, "operator") ||
		res.OperatorQuote != "" || res.DecisionRef != "" {
		t.Fatalf("judgement resolution=%+v", res)
	}
	decided := assistantTask(t, s, "in_progress")
	decidedReq := assistantRecord(t, s, decided.ID)
	got, err = s.ResolveDecisionRequestAssistant(ctx, decided.ID, "berry-mcp", DecisionAssistantResolveInput{
		RequestID: decidedReq.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "pick A", DecisionRef: decidedReq.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	res = got.Request.Resolution
	if res == nil || res.DecidedBy != DecisionDecidedByOperator || res.Responder != DecisionAssistantResponder {
		t.Fatalf("operator_decided resolution=%+v", res)
	}
	rows := outboxRows(t, s)
	var judgeText, decidedText string
	for _, row := range rows {
		switch row.EventID {
		case judgeReq.ID + "-answered":
			judgeText = row.Text
		case decidedReq.ID + "-answered":
			decidedText = row.Text
		}
	}
	if !strings.Contains(judgeText, "(assistant judgement)") || !strings.Contains(decidedText, "(operator quote)") {
		t.Fatalf("outbox texts judge=%q decided=%q", judgeText, decidedText)
	}
}

// Invariant: provenance is part of the resolution's identity — an identical
// re-send is a duplicate, but the same answer sent with a different quote or
// a different claim is resolved-differently, never silently absorbed.
func TestAssistantResolveProvenanceResend(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	answer := DecisionAssistantResolveInput{
		RequestID: request.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "merge it", DecisionRef: request.ID,
	}
	if _, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", answer); err != nil {
		t.Fatal(err)
	}
	dup, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", answer)
	if err != nil || !dup.Duplicate {
		t.Fatalf("identical re-send=%+v err=%v", dup, err)
	}
	other := answer
	other.OperatorQuote = "ship it instead"
	if _, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", other); !errors.Is(err, ErrDecisionRequestResolved) {
		t.Fatalf("different quote err=%v", err)
	}
	judgement := DecisionAssistantResolveInput{RequestID: request.ID, Option: "A"}
	if _, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", judgement); !errors.Is(err, ErrDecisionRequestResolved) {
		t.Fatalf("different provenance err=%v", err)
	}
}

// Invariant: a provenance value outside the vocabulary is invalid_provenance
// and writes nothing; a supplied quote obeys the answer-text rules.
func TestAssistantResolveInvalidProvenance(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	for _, prov := range []string{"operator", "OPERATOR_DECIDED", "human", " operator_decided "} {
		task := assistantTask(t, s, "in_progress")
		request := assistantRecord(t, s, task.ID)
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A", Provenance: prov})
		if !errors.Is(err, ErrInvalidProvenance) {
			t.Fatalf("prov=%q err=%v", prov, err)
		}
	}
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	for _, quote := range []string{"has\x00nul", strings.Repeat("q", DecisionOperatorQuoteMaxBytes+1)} {
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{
			RequestID: request.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: quote, DecisionRef: request.ID,
		})
		if !errors.Is(err, ErrInvalidDecisionRequest) {
			t.Fatalf("bad quote err=%v", err)
		}
	}
	if n := len(outboxRows(t, s)); n != 0 {
		t.Fatalf("invalid provenance left %d outbox rows", n)
	}
}

// Invariant: a resolution recorded before the provenance fields existed
// reads back unchanged, and an identical re-send of that old-shape answer is
// still a duplicate — the fields normalize to assistant_judgement.
func TestAssistantResolvePreProvenanceDuplicate(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	if _, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A"}); err != nil {
		t.Fatal(err)
	}
	// Re-write the stored resolution in the pre-provenance shape: no
	// provenance fields and the old responder.
	if _, err := pool.Exec(ctx, `UPDATE tasks SET refs=jsonb_set(refs,'{decision_request,resolution}',((refs->'decision_request'->'resolution') - 'provenance' - 'operator_quote' - 'decision_ref' - 'decided_by') || '{"responder":"operator-via-berry"}'::jsonb) WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	res := got.Refs.DecisionRequest.Resolution
	if res == nil || res.Provenance != "" || res.DecidedBy != "" || res.Responder != DecisionAssistantResponder {
		t.Fatalf("old-shape resolution=%+v", res)
	}
	dup, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A"})
	if err != nil || !dup.Duplicate {
		t.Fatalf("old-shape re-send=%+v err=%v", dup, err)
	}
}

// Invariant (Amendment 1, A1): a quote made only of invisible runes —
// whitespace, Cc, Cf, default-ignorable code points, variation selectors —
// is no quote at all: it collapses an operator_decided claim to a
// judgement, so a human_only request refuses it naming no_operator_quote.
// A real quote wrapped in invisible runes still counts, and the recorded
// quote keeps the caller's original text. Kills the mutant "normalization
// dropped" (each invisible-only quote would close the human_only request).
func TestAssistantResolveInvisibleQuote(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	in := decisionInput()
	in.HumanOnly = true
	for _, quote := range []string{
		"\u200b",          // zero-width space
		"\ufeff",          // byte order mark
		"\u2060",          // word joiner
		"\u00ad",          // soft hyphen
		"\x01",            // Cc
		"\x1b",            // Cc
		"\ufe0f",          // variation selector
		" \u200b\u00ad\t", // invisible mix
	} {
		task := assistantTask(t, s, "in_progress")
		request := assistantRecordWith(t, s, task.ID, in)
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{
			RequestID: request.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: quote, DecisionRef: request.ID,
		})
		if !errors.Is(err, ErrDecisionRequestHumanOnly) || !strings.Contains(err.Error(), "no_operator_quote") {
			t.Fatalf("quote=%q err=%v", quote, err)
		}
	}
	// A real quote wrapped in invisible runes is still the operator's
	// words — recorded verbatim, wrapper and all.
	task := assistantTask(t, s, "in_progress")
	request := assistantRecordWith(t, s, task.ID, in)
	wrapped := "\u200b\u00ad ship it \ufe0f\u200b"
	got, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{
		RequestID: request.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: wrapped, DecisionRef: request.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	res := got.Request.Resolution
	if res == nil || res.Provenance != DecisionProvenanceOperatorDecided || res.DecidedBy != DecisionDecidedByOperator || res.OperatorQuote != wrapped {
		t.Fatalf("wrapped resolution=%+v", res)
	}
	// On an ordinary request the invisible-only claim is a judgement —
	// no quote and no ref recorded.
	task = assistantTask(t, s, "in_progress")
	request = assistantRecord(t, s, task.ID)
	got, err = s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{
		RequestID: request.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "\u200b\x1b", DecisionRef: request.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	res = got.Request.Resolution
	if res == nil || res.Provenance != DecisionProvenanceAssistantJudgement || res.DecidedBy != DecisionDecidedByAssistant || res.OperatorQuote != "" || res.DecisionRef != "" {
		t.Fatalf("invisible judgement resolution=%+v", res)
	}
}

// Invariant (Amendment 1, A2): provenance is judged before the
// resolved/duplicate branch — an invalid value or an operator_decided ref
// bound to another request on an already-resolved request is a 400/409,
// never a duplicate and never resolved-differently. Kills the mutant
// "validation moved back after the duplicate branch".
func TestAssistantResolveProvenanceOnResolved(t *testing.T) {
	s, _ := searchTestStore(t)
	ctx := context.Background()
	task := assistantTask(t, s, "in_progress")
	request := assistantRecord(t, s, task.ID)
	if _, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A"}); err != nil {
		t.Fatal(err)
	}
	for _, prov := range []string{"bogus", "Operator_Decided", "operator"} {
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{RequestID: request.ID, Option: "A", Provenance: prov})
		if !errors.Is(err, ErrInvalidProvenance) {
			t.Fatalf("resolved + prov=%q err=%v", prov, err)
		}
	}
	for _, ref := range []string{"", "dr-999999-1", request.ID + "-answered", request.ID + " "} {
		_, err := s.ResolveDecisionRequestAssistant(ctx, task.ID, "berry-mcp", DecisionAssistantResolveInput{
			RequestID: request.ID, Option: "A", Provenance: DecisionProvenanceOperatorDecided, OperatorQuote: "pick A", DecisionRef: ref,
		})
		if !errors.Is(err, ErrProvenanceRefMismatch) {
			t.Fatalf("resolved + ref=%q err=%v", ref, err)
		}
	}
	got, _, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	res := got.Refs.DecisionRequest.Resolution
	if res == nil || res.Provenance != DecisionProvenanceAssistantJudgement || res.DecidedBy != DecisionDecidedByAssistant {
		t.Fatalf("resolution after refused resends=%+v", res)
	}
	if n := len(outboxRows(t, s)); n != 1 {
		t.Fatalf("outbox rows=%d", n)
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
