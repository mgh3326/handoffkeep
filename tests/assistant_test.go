package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// MGH-36 PR-1: the /v1/assistant surface. Store-level invariants live in
// internal/store/assistant_test.go; these cover the HTTP contract — status
// codes, auth, and that attribution comes from the server, never the body.

func assistantQuestion(t *testing.T, hURL, token, lane, body string) store.ChatQuestion {
	t.Helper()
	id := chatQuestionID(t, 7)
	status, q := putChatQuestion(t, hURL, token, id, lane, body)
	if status != http.StatusCreated {
		t.Fatalf("put question status=%d", status)
	}
	return q
}

func assistantMessageBody(eventID string, answers []map[string]any) map[string]any {
	return map[string]any{
		"conversation_id":  store.ChatConversationID,
		"author":           "operator",
		"body":             "assistant answer",
		"source_channel":   "assistant",
		"origin_event_id":  eventID,
		"origin_timestamp": time.Now().UTC(),
		"answers":          answers,
	}
}

// AC3: two concurrent assistant answers at one revision — exactly one 201 and
// one 409 chat_question_stale, exactly one answer_message_id, one outbox row.
func TestAssistantChatAnswerConcurrentHTTP(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	q := assistantQuestion(t, h.URL, "node-token", taskLane(t), "race me")
	var wg sync.WaitGroup
	type outcome struct {
		status int
		body   map[string]any
	}
	results := make([]outcome, 2)
	start := make(chan struct{})
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/messages", "node-token",
				assistantMessageBody(fmt.Sprintf("race-%d-%d", i, time.Now().UnixNano()), []map[string]any{{"question_id": q.ID, "revision": q.Revision}}))
			defer resp.Body.Close()
			out := map[string]any{}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			results[i] = outcome{resp.StatusCode, out}
		}(i)
	}
	close(start)
	wg.Wait()
	created, stale := 0, 0
	for _, r := range results {
		switch {
		case r.status == http.StatusCreated:
			created++
		case r.status == http.StatusConflict && r.body["error"] == "chat_question_stale":
			stale++
		default:
			t.Fatalf("unexpected outcome status=%d body=%v", r.status, r.body)
		}
	}
	if created != 1 || stale != 1 {
		t.Fatalf("created=%d stale=%d", created, stale)
	}
	got, found, err := s.GetChatQuestion(t.Context(), q.ID)
	if err != nil || !found || got.AnswerMessageID == nil {
		t.Fatalf("question=%+v found=%t err=%v", got, found, err)
	}
	// The shared DB holds other tests' outbox rows; only this answer's
	// deterministic event id may appear, exactly once.
	unsent, err := s.ListUnsentNotifications(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	mine := 0
	for _, row := range unsent {
		if row.EventID == q.ID+"-rev1-answered" {
			mine++
			if row.Kind != store.OutboxKindChatAnswer || row.TargetLane != q.Lane {
				t.Fatalf("outbox row=%+v", row)
			}
		}
	}
	if mine != 1 {
		t.Fatalf("outbox rows for %s=%d", q.ID, mine)
	}
}

// AC4: an assistant answer at an older revision or against a non-pending
// question is 409 chat_question_stale and writes nothing.
func TestAssistantChatAnswerStaleHTTP(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	q := assistantQuestion(t, h.URL, "node-token", lane, "original")
	if _, updated := putChatQuestion(t, h.URL, "node-token", q.ID, lane, "edited"); updated.Revision != 2 {
		t.Fatalf("edited revision=%d", updated.Revision)
	}
	staleEvent := fmt.Sprintf("stale-%d", time.Now().UnixNano())
	stale := request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/messages", "node-token",
		assistantMessageBody(staleEvent, []map[string]any{{"question_id": q.ID, "revision": 1}}))
	defer stale.Body.Close()
	var staleBody map[string]any
	_ = json.NewDecoder(stale.Body).Decode(&staleBody)
	if stale.StatusCode != http.StatusConflict || staleBody["error"] != "chat_question_stale" {
		t.Fatalf("stale status=%d body=%v", stale.StatusCode, staleBody)
	}
	// A resolved question is stale to the assistant path too.
	resolved := assistantQuestion(t, h.URL, "node-token", lane, "close me")
	tr := request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/questions/"+resolved.ID+"/transition", "node-token", map[string]string{"to": "resolved"})
	tr.Body.Close()
	closedEvent := fmt.Sprintf("closed-%d", time.Now().UnixNano())
	nonPending := request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/messages", "node-token",
		assistantMessageBody(closedEvent, []map[string]any{{"question_id": resolved.ID, "revision": resolved.Revision}}))
	defer nonPending.Body.Close()
	if nonPending.StatusCode != http.StatusConflict {
		t.Fatalf("non-pending status=%d", nonPending.StatusCode)
	}
	// Neither refusal left a message or an outbox row: the shared DB holds
	// other tests' rows, so the proof is per event id.
	p := chatPool(t)
	var leftover int
	for _, ev := range []string{staleEvent, closedEvent} {
		if err := p.QueryRow(t.Context(), `SELECT count(*) FROM chat_messages WHERE origin_event_id=$1`, ev).Scan(&leftover); err != nil || leftover != 0 {
			t.Fatalf("refused answer %s left %d messages err=%v", ev, leftover, err)
		}
	}
	unsent, err := s.ListUnsentNotifications(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range unsent {
		if row.EventID == q.ID+"-rev1-answered" || row.EventID == resolved.ID+"-rev1-answered" {
			t.Fatalf("refused answer left outbox row %+v", row)
		}
	}
}

// AC5: the human reply path is unchanged — a reply relates to the question,
// never fills the answer slot or resolves it, and several replies are fine.
func TestOperatorReplyLeavesQuestionPendingHTTP(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	q := assistantQuestion(t, h.URL, "node-token", taskLane(t), "human answers here")
	for i := range 2 {
		status, msg := postChatMessage(t, h.URL, "node-token", map[string]any{
			"conversation_id": store.ChatConversationID, "author": "operator", "body": fmt.Sprintf("human %d", i),
			"source_channel": "web", "origin_event_id": fmt.Sprintf("web-%d-%d", i, time.Now().UnixNano()),
			"question_ids": []string{q.ID},
		})
		if status != http.StatusCreated || len(msg.QuestionRelations) != 1 {
			t.Fatalf("reply %d status=%d msg=%+v", i, status, msg)
		}
	}
	got, _, err := s.GetChatQuestion(t.Context(), q.ID)
	if err != nil || got.State != "pending" || got.AnswerMessageID != nil {
		t.Fatalf("human replies mutated question=%+v err=%v", got, err)
	}
	// The assistant channel keeps author=operator and requires it — a
	// mismatched author is a validation error, not a recorded message.
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/messages", "node-token", map[string]any{
		"conversation_id": store.ChatConversationID, "author": "desk", "body": "posed",
		"source_channel": "assistant", "origin_event_id": fmt.Sprintf("bad-%d", time.Now().UnixNano()),
		"answers": []map[string]any{{"question_id": q.ID, "revision": q.Revision}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("author=desk on assistant channel status=%d", resp.StatusCode)
	}
	// The question is still answerable by the assistant path afterwards.
	status, _ := postChatMessage(t, h.URL, "node-token",
		assistantMessageBody(fmt.Sprintf("ok-%d", time.Now().UnixNano()), []map[string]any{{"question_id": q.ID, "revision": 1}}))
	if status != http.StatusCreated {
		t.Fatalf("answer after human replies status=%d", status)
	}
}

// AC6+AC8: the assistant resolve API refuses every kind but answered,
// refuses a human_only request and a disposition item, and records
// responder=operator-via-berry / by=<token client> regardless of forged
// body fields.
func TestAssistantDecisionResolveAPI(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	post := func(taskID int64, body any) (int, map[string]any) {
		t.Helper()
		resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/assistant/decisions/resolve", "node-token", body)
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Wrong kinds are refused without touching the request.
	kindTask := drTask(t, s, lane, "claimed", "in_progress")
	kindReq := drRecord(t, s, kindTask.ID, drInput("kind guard"))
	for _, kind := range []string{"default_applied", "withdrawn"} {
		status, body := post(kindTask.ID, map[string]any{"request_id": kindReq.ID, "kind": kind, "text": "x"})
		if status != http.StatusBadRequest || body["error"] != "decision_assistant_kind" {
			t.Fatalf("kind=%s status=%d body=%v", kind, status, body)
		}
	}
	if got, _, _ := s.GetTask(t.Context(), kindTask.ID); got.Refs.DecisionRequest.Status != store.DecisionRequestOpen {
		t.Fatalf("refused kind closed the request: %+v", got.Refs.DecisionRequest)
	}

	// human_only refuses the assistant path with a named 409.
	humanTask := drTask(t, s, lane, "claimed", "in_progress")
	human := drInput("deploy 결정")
	human.HumanOnly = true
	humanReq := drRecord(t, s, humanTask.ID, human)
	status, body := post(humanTask.ID, map[string]any{"request_id": humanReq.ID, "option": "A"})
	if status != http.StatusConflict || body["error"] != "decision_request_human_only" {
		t.Fatalf("human_only status=%d body=%v", status, body)
	}

	// A disposition item refuses the assistant path. RecordDecisionRequest
	// refuses disposition items outright, so the request is recorded first
	// and the disposition marker lands on refs afterwards — the guard reads
	// the same field a real item would carry.
	dispTask := drTask(t, s, lane, "claimed", "in_progress")
	dispReq := drRecord(t, s, dispTask.ID, drInput("disposition 요청"))
	p := chatPool(t)
	if _, err := p.Exec(t.Context(), `UPDATE tasks SET refs=refs||'{"disposition":{"schema":"disposition/v1","facts":{"facts_source":"gh-pr-view","facts_as_of":"2026-10-09T00:00:00Z","install":{"state":"unknown"},"residual_n":0},"recommended":"E"}}'::jsonb WHERE id=$1`, dispTask.ID); err != nil {
		t.Fatal(err)
	}
	status, body = post(dispTask.ID, map[string]any{"request_id": dispReq.ID, "option": "A"})
	if status != http.StatusConflict || body["error"] != "disposition_operator_only" {
		t.Fatalf("disposition status=%d body=%v", status, body)
	}

	// Forged responder/by fields are accepted-but-ignored: the recorded
	// resolution is server-attributed.
	task := drTask(t, s, lane, "claimed", "in_progress")
	req := drRecord(t, s, task.ID, drInput("누가 답했나"))
	status, resolved := post(task.ID, map[string]any{
		"request_id": req.ID, "option": "A", "text": "ship it",
		"responder": "operator", "by": "root",
	})
	if status != http.StatusOK {
		t.Fatalf("resolve status=%d body=%v", status, resolved)
	}
	got, _, err := s.GetTask(t.Context(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	res := got.Refs.DecisionRequest.Resolution
	if res == nil || res.Responder != store.DecisionAssistantResponder || res.By != "node" || res.Kind != store.DecisionRequestAnswered {
		t.Fatalf("attribution=%+v", res)
	}
	// Exactly one deterministic outbox row (the shared DB holds other tests'
	// rows; count only this resolution's event id).
	countOutbox := func() int {
		t.Helper()
		unsent, err := s.ListUnsentNotifications(t.Context(), 0)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, row := range unsent {
			if row.EventID == req.ID+"-answered" {
				n++
				if row.Kind != store.OutboxKindDecisionAnswered || row.TargetLane != task.Lane {
					t.Fatalf("outbox row=%+v", row)
				}
			}
		}
		return n
	}
	if countOutbox() != 1 {
		t.Fatalf("outbox rows for %s=%d", req.ID, countOutbox())
	}
	// The identical re-send is a duplicate, not a second write.
	status, dup := post(task.ID, map[string]any{"request_id": req.ID, "option": "A", "text": "ship it"})
	if status != http.StatusOK || dup["duplicate"] != true {
		t.Fatalf("repeat status=%d body=%v", status, dup)
	}
	if countOutbox() != 1 {
		t.Fatalf("repeat left %d outbox rows", countOutbox())
	}
	// A stale request id is a 409.
	status, body = post(task.ID, map[string]any{"request_id": "dr-" + strconv.FormatInt(task.ID, 10) + "-9", "option": "A"})
	if status != http.StatusConflict {
		t.Fatalf("stale request status=%d body=%v", status, body)
	}
	// Unauthenticated calls are refused.
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/assistant/decisions/resolve", "", map[string]any{"request_id": req.ID, "option": "A"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", resp.StatusCode)
	}
}

// AC7: the pending API lists open requests on live tasks (merged/dropped
// excluded), pending questions with revision, and server_time.
func TestAssistantPendingAPI(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	backlog := drTask(t, s, lane)
	backlogReq := drRecord(t, s, backlog.ID, drInput("backlog 요청"))
	work := drTask(t, s, lane, "claimed", "in_progress")
	workReq := drRecord(t, s, work.ID, drInput("in-progress 요청"))
	merged := drTask(t, s, lane, "claimed", "in_progress")
	drRecord(t, s, merged.ID, drInput("머지 전 요청"))
	for _, to := range []string{"verifying", "merged"} {
		if _, err := s.TransitionTask(t.Context(), merged.ID, to, "dr-test", "step", nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	dropped := drTask(t, s, lane, "claimed", "in_progress")
	drRecord(t, s, dropped.ID, drInput("드롭 전 요청"))
	if _, err := s.TransitionTask(t.Context(), dropped.ID, "dropped", "dr-test", "abandon", nil, ""); err != nil {
		t.Fatal(err)
	}
	q := assistantQuestion(t, h.URL, "node-token", lane, "pending question")

	resp := request(t, h.Client(), http.MethodGet, h.URL+"/v1/assistant/pending", "node-token", nil)
	defer resp.Body.Close()
	var pending store.AssistantPending
	if err := json.NewDecoder(resp.Body).Decode(&pending); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pending status=%d", resp.StatusCode)
	}
	if pending.ServerTime.IsZero() {
		t.Fatalf("server_time missing")
	}
	listed := map[int64]string{}
	for _, d := range pending.DecisionRequests {
		listed[d.TaskID] = d.Request.ID
	}
	if listed[backlog.ID] != backlogReq.ID || listed[work.ID] != workReq.ID {
		t.Fatalf("live requests missing: %+v", listed)
	}
	for _, id := range []int64{merged.ID, dropped.ID} {
		if _, ok := listed[id]; ok {
			t.Fatalf("terminal task %d listed", id)
		}
	}
	found := false
	for _, question := range pending.ChatQuestions {
		if question.ID == q.ID {
			found = true
			if question.Revision != q.Revision {
				t.Fatalf("pending Q revision=%d want %d", question.Revision, q.Revision)
			}
		}
	}
	if !found {
		t.Fatalf("pending question not listed")
	}
	resp = request(t, h.Client(), http.MethodGet, h.URL+"/v1/assistant/pending", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated pending status=%d", resp.StatusCode)
	}
}

// AC9: the outbox API lists unsent rows and marks one sent by event_id,
// idempotently; hub_row_id=0 or a differing row id never marks sent.
func TestAssistantOutboxAPI(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	task := drTask(t, s, taskLane(t), "claimed", "in_progress")
	req := drRecord(t, s, task.ID, drInput("outbox 요청"))
	resolve := request(t, h.Client(), http.MethodPost, h.URL+"/v1/assistant/decisions/resolve", "node-token", map[string]any{"request_id": req.ID, "option": "B"})
	resolve.Body.Close()
	if resolve.StatusCode != http.StatusOK {
		t.Fatalf("resolve status=%d", resolve.StatusCode)
	}
	eventID := req.ID + "-answered"
	// The shared DB holds other tests' unsent rows; look up only this test's
	// event id in the list.
	unsentHas := func() (bool, store.NotificationOutbox) {
		t.Helper()
		resp := request(t, h.Client(), http.MethodGet, h.URL+"/v1/assistant/outbox", "node-token", nil)
		defer resp.Body.Close()
		var listed struct {
			Notifications []store.NotificationOutbox `json:"notifications"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
			t.Fatal(err)
		}
		for _, row := range listed.Notifications {
			if row.EventID == eventID {
				return true, row
			}
		}
		return false, store.NotificationOutbox{}
	}
	present, row := unsentHas()
	if !present || row.SentAt != nil || row.Kind != store.OutboxKindDecisionAnswered {
		t.Fatalf("unsent lookup present=%t row=%+v", present, row)
	}
	mark := func(body any) (int, map[string]any) {
		t.Helper()
		resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/assistant/outbox/sent", "node-token", body)
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if status, out := mark(map[string]any{"event_id": eventID, "hub_row_id": 0}); status != http.StatusBadRequest {
		t.Fatalf("hub_row_id=0 status=%d body=%v", status, out)
	}
	status, first := mark(map[string]any{"event_id": eventID, "hub_row_id": 4242})
	if status != http.StatusOK || first["hub_row_id"].(float64) != 4242 || first["sent_at"] == nil {
		t.Fatalf("mark status=%d body=%v", status, first)
	}
	status, again := mark(map[string]any{"event_id": eventID, "hub_row_id": 4242})
	if status != http.StatusOK || again["sent_at"] != first["sent_at"] {
		t.Fatalf("repeat mark status=%d body=%v", status, again)
	}
	if status, out := mark(map[string]any{"event_id": eventID, "hub_row_id": 7777}); status != http.StatusConflict {
		t.Fatalf("different row id status=%d body=%v", status, out)
	}
	if status, out := mark(map[string]any{"event_id": "dr-999999-1-answered", "hub_row_id": 1}); status != http.StatusNotFound {
		t.Fatalf("unknown event status=%d body=%v", status, out)
	}
	if present, _ := unsentHas(); present {
		t.Fatalf("sent row still listed")
	}
}
