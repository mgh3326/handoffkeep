package mcp

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// TestDecisionsPendingSplitsCleanupAndOperatorOnly checks the two lists
// decisions_pending returns: an open request on a live task needs an answer,
// and one left open on a dropped task is cleanup. It also checks that an
// operator-only request refused to a bearer caller keeps its attempt and stays
// open.
func TestDecisionsPendingSplitsCleanupAndOperatorOnly(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	options := store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "first", Recommended: true}, {Key: "B", Label: "second"}}}

	live := newOperatorTask(t, st, "decision live fixture")
	openReq, err := st.RecordDecisionRequest(ctx, live.ID, "desk", store.DecisionRequestInput{
		Question: "Which path?", Options: options, DefaultAction: "hold", HumanOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	dropped := newOperatorTask(t, st, "decision dropped fixture")
	droppedReq, err := st.RecordDecisionRequest(ctx, dropped.ID, "desk", store.DecisionRequestInput{
		Question: "Keep it?", Options: options, DefaultAction: "hold",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionTask(ctx, dropped.ID, "dropped", "tester", "step", nil, ""); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})

	type pendingItem struct {
		RequestID    string `json:"request_id"`
		Reason       string `json:"reason"`
		TaskState    string `json:"task_state"`
		OperatorOnly bool   `json:"operator_only"`
	}
	type pendingOut struct {
		Decisions    []pendingItem `json:"decisions"`
		NeedsCleanup []pendingItem `json:"needs_cleanup"`
	}
	pending := callTool[pendingOut](t, session, "decisions_pending", map[string]any{})
	if len(pending.Decisions) != 1 || pending.Decisions[0].RequestID != openReq.Request.ID {
		t.Fatalf("decisions=%+v", pending.Decisions)
	}
	if got := pending.Decisions[0]; got.Reason != "awaiting_answer" || !got.OperatorOnly {
		t.Fatalf("needs-answer item=%+v", got)
	}
	if len(pending.NeedsCleanup) != 1 || pending.NeedsCleanup[0].RequestID != droppedReq.Request.ID {
		t.Fatalf("needs_cleanup=%+v", pending.NeedsCleanup)
	}
	if got := pending.NeedsCleanup[0]; got.Reason != "task_closed" || got.TaskState != "dropped" || got.OperatorOnly {
		t.Fatalf("cleanup item=%+v", got)
	}

	text, isError := toolText(t, session, "decision_answer", map[string]any{"request_id": openReq.Request.ID, "option": "A"})
	if !isError || !strings.Contains(text, "decision_operator_only") {
		t.Fatalf("operator-only answer over bearer: isError=%v text=%q", isError, text)
	}
	attempts, err := st.ListDecisionAttempts(ctx, live.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Result != store.DecisionResultPendingConfirmation {
		t.Fatalf("attempts=%+v", attempts)
	}
	still := callTool[pendingOut](t, session, "decisions_pending", map[string]any{})
	if len(still.Decisions) != 1 || still.Decisions[0].RequestID != openReq.Request.ID {
		t.Fatalf("refused request left the open list: %+v", still.Decisions)
	}
}

// TestDecisionsHistoryPagesClosedRequests answers three requests over bearer,
// leaves a fourth open, and pages the history two at a time. Each closed item
// carries its recommendation, chosen option, rationale and decider; the open
// request is absent.
func TestDecisionsHistoryPagesClosedRequests(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	options := store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "smaller", Recommended: true}, {Key: "B", Label: "larger"}}}

	var closedIDs []string
	for i := 0; i < 3; i++ {
		task := newOperatorTask(t, st, fmt.Sprintf("decision history fixture %d", i))
		res, err := st.RecordDecisionRequest(ctx, task.ID, "desk", store.DecisionRequestInput{
			Question: fmt.Sprintf("Pick %d?", i), Options: options, DefaultAction: "hold", Reason: "A is the smaller change",
		})
		if err != nil {
			t.Fatal(err)
		}
		closedIDs = append(closedIDs, res.Request.ID)
	}
	openTask := newOperatorTask(t, st, "decision history open fixture")
	openRes, err := st.RecordDecisionRequest(ctx, openTask.ID, "desk", store.DecisionRequestInput{
		Question: "Still open?", Options: options, DefaultAction: "hold",
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})
	for _, id := range closedIDs {
		answer := callTool[answerOut](t, session, "decision_answer", map[string]any{"request_id": id, "option": "A"})
		if answer.Result != store.DecisionResultAnswered {
			t.Fatalf("answer %s=%+v", id, answer)
		}
	}

	type historyItem struct {
		RequestID         string    `json:"request_id"`
		Status            string    `json:"status"`
		OperatorOnly      bool      `json:"operator_only"`
		RecommendedOption string    `json:"recommended_option"`
		ChosenOption      string    `json:"chosen_option"`
		Rationale         string    `json:"rationale"`
		Decider           string    `json:"decider"`
		DecidedAt         time.Time `json:"decided_at"`
		ConsoleURL        string    `json:"console_url"`
	}
	type historyOut struct {
		Decisions  []historyItem `json:"decisions"`
		Truncated  bool          `json:"truncated"`
		NextCursor string        `json:"next_cursor"`
	}
	first := callTool[historyOut](t, session, "decisions_history", map[string]any{"limit": 2})
	if len(first.Decisions) != 2 || !first.Truncated || first.NextCursor == "" {
		t.Fatalf("first page=%+v", first)
	}
	second := callTool[historyOut](t, session, "decisions_history", map[string]any{"limit": 2, "cursor": first.NextCursor})
	if len(second.Decisions) != 1 || second.Truncated || second.NextCursor != "" {
		t.Fatalf("second page=%+v", second)
	}

	seen := map[string]int{}
	for _, item := range append(first.Decisions, second.Decisions...) {
		seen[item.RequestID]++
		if item.Status != store.DecisionRequestAnswered || item.ChosenOption != "A" || item.RecommendedOption != "A" {
			t.Fatalf("history item=%+v", item)
		}
		if item.Rationale != "A is the smaller change" || item.Decider != "tester" || item.DecidedAt.IsZero() || item.ConsoleURL == "" || item.OperatorOnly {
			t.Fatalf("history item=%+v", item)
		}
	}
	for _, id := range closedIDs {
		if seen[id] != 1 {
			t.Fatalf("closed request %s seen %d times in %v", id, seen[id], seen)
		}
	}
	if seen[openRes.Request.ID] != 0 {
		t.Fatalf("open request appeared in history: %v", seen)
	}
}

// TestDecisionsHistoryPagesSessionRecords pages session-settled records two
// at a time: next_cursor carries the session position after the task one is
// exhausted, and the last page ends with an empty cursor (hk 4518).
func TestDecisionsHistoryPagesSessionRecords(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		_, err := st.RecordSessionDecision(ctx, store.SessionDecisionInput{
			Key: fmt.Sprintf("sess-%d", i), Lane: "mcp-lane", By: "tester",
			Reading: fmt.Sprintf("Reading %d", i), Quote: fmt.Sprintf("Quote %d", i),
			DecidedAt: now.Add(-time.Duration(i) * time.Minute),
			Grounds:   []string{"hk:doc decision/2026-11-14/session-page"},
		}, now.Add(time.Duration(i)*time.Second), nil)
		if err != nil {
			t.Fatal(err)
		}
	}

	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, nil))
	defer srv.Close()
	session := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer bearer-secret"})

	type sessionDecision struct {
		Key      string `json:"key"`
		Recorded string `json:"recorded_by"`
	}
	type historyOut struct {
		SessionDecisions []sessionDecision `json:"session_decisions"`
		Truncated        bool              `json:"truncated"`
		NextCursor       string            `json:"next_cursor"`
	}
	first := callTool[historyOut](t, session, "decisions_history", map[string]any{"limit": 2})
	if len(first.SessionDecisions) != 2 || !first.Truncated || first.NextCursor == "" {
		t.Fatalf("first page=%+v", first)
	}
	second := callTool[historyOut](t, session, "decisions_history", map[string]any{"limit": 2, "cursor": first.NextCursor})
	if len(second.SessionDecisions) != 1 || second.Truncated || second.NextCursor != "" {
		t.Fatalf("second page=%+v", second)
	}
	seen := map[string]int{}
	for _, x := range append(first.SessionDecisions, second.SessionDecisions...) {
		seen[x.Key]++
		if x.Recorded != "tester" {
			t.Fatalf("recorded_by=%q", x.Recorded)
		}
	}
	// Newest record first: sess-2, sess-1, then sess-0.
	wantOrder := []string{"mcp-lane:sess-2", "mcp-lane:sess-1", "mcp-lane:sess-0"}
	got := append(first.SessionDecisions, second.SessionDecisions...)
	for i, x := range got {
		if x.Key != wantOrder[i] || seen[x.Key] != 1 {
			t.Fatalf("page order=%+v", got)
		}
	}
}

func TestDecisionRecordAndOperatorAttentionTools(t *testing.T) {
	st, pool := newOperatorStoreWithPool(t)
	ctx := context.Background()
	fixture := newAccessFixture(t)
	token := "test-" + t.Name()
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": token}, fixture.verifier))
	defer srv.Close()
	bearerSession := connectMCP(t, srv.URL, map[string]string{"Authorization": "Bearer " + token})
	session := connectMCP(t, srv.URL, map[string]string{"Cf-Access-Jwt-Assertion": fixture.token(t, jwt.MapClaims{"email": "operator@example.com"})})

	type sessionDecision struct {
		Reading  string    `json:"reading"`
		Quote    string    `json:"quote"`
		Decided  time.Time `json:"decided_at"`
		Grounds  []string  `json:"grounds"`
		Recorded string    `json:"recorded_by"`
	}
	type historyOut struct {
		SessionDecisions []sessionDecision `json:"session_decisions"`
	}
	at := time.Date(2026, 11, 11, 5, 0, 0, 0, time.UTC)
	args := map[string]any{
		"key": "a27", "lane": "mcp-lane", "reading": "MCP answers count as the operator's own.",
		"quote": "hk web and MCP can answer.", "decided_at": at.Format(time.RFC3339),
		"grounds": []string{"hk:doc decision/2026-11-11/model-routing-escalation"},
	}
	if text, isError := toolText(t, bearerSession, "decision_record", args); !isError || !strings.Contains(text, "operator_required") {
		t.Fatalf("unverified bearer record: isError=%t text=%q", isError, text)
	}
	var refusedRecords, refusedAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM decision_requests WHERE source='session_chat' AND source_ref='mcp-lane:a27'`).Scan(&refusedRecords); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mcp_write_audit WHERE tool='decision_record' AND idempotency_key='a27'`).Scan(&refusedAudits); err != nil || refusedRecords != 0 || refusedAudits != 0 {
		t.Fatalf("unverified bearer wrote records=%d audits=%d err=%v", refusedRecords, refusedAudits, err)
	}
	recorded := callTool[store.SessionDecisionResult](t, session, "decision_record", args)
	if recorded.Duplicate || recorded.Record.Source != store.DecisionSourceSessionChat || recorded.Record.Answer != "hk web and MCP can answer." {
		t.Fatalf("record=%+v", recorded)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM mcp_write_audit WHERE tool='decision_record' AND actor='operator:operator@example.com' AND channel='chatgpt' AND idempotency_key='a27'`).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("decision record audit count=%d err=%v", auditCount, err)
	}
	replay := callTool[store.SessionDecisionResult](t, session, "decision_record", args)
	if !replay.Duplicate || replay.Record.ID != recorded.Record.ID {
		t.Fatalf("replay=%+v", replay)
	}
	changed := map[string]any{}
	for key, value := range args {
		changed[key] = value
	}
	changed["quote"] = "different words"
	if text, isError := toolText(t, session, "decision_record", changed); !isError || !strings.Contains(text, "idempotency_key_reused") {
		t.Fatalf("changed key result=%q isError=%t", text, isError)
	}
	history := callTool[historyOut](t, session, "decisions_history", map[string]any{})
	if len(history.SessionDecisions) != 1 || history.SessionDecisions[0].Quote != "hk web and MCP can answer." || history.SessionDecisions[0].Reading == "" || !history.SessionDecisions[0].Decided.Equal(at) || len(history.SessionDecisions[0].Grounds) != 1 {
		t.Fatalf("session decisions=%+v", history.SessionDecisions)
	}

	task := newOperatorTask(t, st, "operator attention fixture")
	_, err := st.RecordDecisionRequest(ctx, task.ID, "tester", store.DecisionRequestInput{
		Question: "Choose?", Options: store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "A"}}},
		DefaultAction: "hold", Block: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	type attentionPart struct {
		Count int              `json:"count"`
		Items []map[string]any `json:"items"`
	}
	type attentionOut struct {
		Decisions attentionPart `json:"decisions"`
		Tasks     attentionPart `json:"tasks"`
		AsOf      time.Time     `json:"as_of"`
	}
	attention := callTool[attentionOut](t, session, "operator_attention", map[string]any{})
	if attention.Decisions.Count != 1 || len(attention.Decisions.Items) != 1 || attention.Tasks.Count != 0 || attention.AsOf.IsZero() {
		t.Fatalf("attention=%+v", attention)
	}
}
