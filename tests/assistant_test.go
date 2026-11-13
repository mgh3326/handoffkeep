package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// PR-4c: a revised chat question is answerable again over HTTP — the PUT
// bumps revision and clears answer_message_id, the old revision's answer is
// 409 chat_question_stale, and the new revision's answer lands.
func TestAssistantChatAnswerAfterRevisionHTTP(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	q := assistantQuestion(t, h.URL, "node-token", lane, "original body")
	status, _ := postChatMessage(t, h.URL, "node-token",
		assistantMessageBody(fmt.Sprintf("rev1-%d", time.Now().UnixNano()), []map[string]any{{"question_id": q.ID, "revision": 1}}))
	if status != http.StatusCreated {
		t.Fatalf("rev1 answer status=%d", status)
	}
	// A real revision clears the slot over the wire too.
	status, revised := putChatQuestion(t, h.URL, "node-token", q.ID, lane, "revised body")
	if status != http.StatusOK || revised.Revision != 2 || revised.AnswerMessageID != nil {
		t.Fatalf("revised status=%d q=%+v", status, revised)
	}
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/chat/messages", "node-token",
		assistantMessageBody(fmt.Sprintf("stale-%d", time.Now().UnixNano()), []map[string]any{{"question_id": q.ID, "revision": 1}}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("rev1 answer after revision status=%d", resp.StatusCode)
	}
	status, _ = postChatMessage(t, h.URL, "node-token",
		assistantMessageBody(fmt.Sprintf("rev2-%d", time.Now().UnixNano()), []map[string]any{{"question_id": q.ID, "revision": 2}}))
	if status != http.StatusCreated {
		t.Fatalf("rev2 answer status=%d", status)
	}
	unsent, err := s.ListUnsentNotifications(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var rev1Row, rev2Row int
	for _, row := range unsent {
		if row.EventID == q.ID+"-rev1-answered" {
			rev1Row++
		}
		if row.EventID == q.ID+"-rev2-answered" {
			rev2Row++
		}
	}
	if rev1Row != 1 || rev2Row != 1 {
		t.Fatalf("outbox rev1=%d rev2=%d", rev1Row, rev2Row)
	}
}

// The receiving lane can acknowledge an outbox row after the hub accepted it;
// caller-supplied identity is ignored in favor of the bearer token identity.
func TestAssistantOutboxAppliedAPI(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	task := drTask(t, s, taskLane(t), "claimed", "in_progress")
	req := drRecord(t, s, task.ID, drInput("applied 요청"))
	if _, err := s.ResolveDecisionRequestAssistant(t.Context(), task.ID, "berry-mcp", store.DecisionAssistantResolveInput{RequestID: req.ID, Option: "B"}); err != nil {
		t.Fatal(err)
	}
	unsent, err := s.ListUnsentNotifications(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	eventID := ""
	for _, row := range unsent {
		if strings.HasPrefix(row.EventID, "decision-answered-"+req.ID+"-") {
			eventID = row.EventID
			break
		}
	}
	if eventID == "" {
		t.Fatal("resolve queued no notice")
	}
	if _, err := s.MarkNotificationSent(t.Context(), eventID, 4242); err != nil {
		t.Fatal(err)
	}
	applied := func(eventID, by string) (int, map[string]any) {
		t.Helper()
		body := map[string]any{"event_id": eventID}
		if by != "" {
			body["by"] = by
		}
		resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/assistant/outbox/applied", "node-token", body)
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	status, ack := applied(eventID, "spoofed-lane")
	by, _ := ack["applied_by"].(string)
	if status != http.StatusOK || by == "" || by == "spoofed-lane" || ack["applied_at"] == nil || ack["sent_at"] == nil {
		t.Fatalf("applied status=%d body=%v", status, ack)
	}
	if status, again := applied(eventID, ""); status != http.StatusOK || again["applied_at"] != ack["applied_at"] || again["applied_by"] != by {
		t.Fatalf("repeat applied status=%d body=%v", status, again)
	}
	if status, out := applied("dr-999999-1-answered", ""); status != http.StatusNotFound {
		t.Fatalf("applied unknown event status=%d body=%v", status, out)
	}
}

// An ack for a row the hub has not accepted is refused with its own code.
func TestAssistantOutboxAppliedNeedsReceived(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	task := drTask(t, s, taskLane(t), "claimed", "in_progress")
	req := drRecord(t, s, task.ID, drInput("applied 요청"))
	if _, err := s.ResolveDecisionRequestAssistant(t.Context(), task.ID, "berry-mcp", store.DecisionAssistantResolveInput{RequestID: req.ID, Option: "B"}); err != nil {
		t.Fatal(err)
	}
	unsent, err := s.ListUnsentNotifications(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	eventID := ""
	for _, row := range unsent {
		if strings.HasPrefix(row.EventID, "decision-answered-"+req.ID+"-") {
			eventID = row.EventID
			break
		}
	}
	if eventID == "" {
		t.Fatal("resolve queued no notice")
	}
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/assistant/outbox/applied", "node-token", map[string]any{"event_id": eventID})
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusConflict || out["error"] != "notification_outbox_not_received" {
		t.Fatalf("applied before received status=%d body=%v", resp.StatusCode, out)
	}
}
