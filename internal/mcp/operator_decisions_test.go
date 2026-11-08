package mcp

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
