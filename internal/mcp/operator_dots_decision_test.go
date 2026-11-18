package mcp

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// Invariant (operator decision 399, hk 5132): on the dots channel an
// operator-only request refuses an answer without the operator's words and
// keeps the refusal, and closes when the answer names operator_decided with
// the verbatim quote and this exact request id. The resolution records
// channel dots and the words, so a reader tells it from a console answer.
func TestDotsDecisionAnswerNamesOperator(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "operator-only dots fixture")
	request, err := st.RecordDecisionRequest(ctx, task.ID, "desk", store.DecisionRequestInput{
		Question:      "Ship it?",
		Options:       store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "yes"}, {Key: "B", Label: "no"}}, AllowFree: true},
		DefaultAction: "hold",
		HumanOnly:     true,
		Block:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newAccessFixture(t)
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, fixture.verifier))
	defer srv.Close()
	assertion := fixture.token(t, jwt.MapClaims{"email": "operator@example.com"})
	session := connectMCP(t, srv.URL, map[string]string{"Cf-Access-Jwt-Assertion": assertion})

	// Dots' own judgement: refused, and the refusal stays on the task.
	text, isError := toolText(t, session, "decision_answer", map[string]any{"request_id": request.Request.ID, "option": "A"})
	if !isError || !strings.Contains(text, "operator_only_decision") {
		t.Fatalf("judgement over dots: isError=%v text=%q", isError, text)
	}
	attempts, err := st.ListDecisionAttempts(ctx, task.ID, 10)
	if err != nil || len(attempts) != 1 || attempts[0].Result != "refused" || attempts[0].Channel != store.DecisionChannelDots {
		t.Fatalf("attempts after the refusal=%+v err=%v", attempts, err)
	}

	// The operator's words, bound to this request, close it.
	const words = "A로 진행해"
	answer := callTool[answerOut](t, session, "decision_answer", map[string]any{
		"request_id": request.Request.ID, "option": "A",
		"provenance": store.DecisionProvenanceOperatorDecided, "operator_quote": words, "decision_ref": request.Request.ID,
	})
	if answer.Result != store.DecisionResultAnswered || answer.Channel != ChannelChatGPT {
		t.Fatalf("answer=%+v", answer)
	}
	closed, found, err := st.GetTask(ctx, task.ID)
	if err != nil || !found {
		t.Fatalf("task read: found=%v err=%v", found, err)
	}
	res := closed.Refs.DecisionRequest.Resolution
	if res == nil || res.Provenance != store.DecisionProvenanceOperatorDecided || res.OperatorQuote != words || res.DecisionRef != request.Request.ID {
		t.Fatalf("resolution=%+v", res)
	}
	attempts, err = st.ListDecisionAttempts(ctx, task.ID, 10)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("attempts after the answer=%+v err=%v", attempts, err)
	}
	if last := attempts[0]; last.Result != store.DecisionResultAnswered || last.Channel != store.DecisionChannelDots || last.Provenance != store.DecisionProvenanceOperatorRelay {
		t.Fatalf("answered attempt=%+v", last)
	}
}

// Invariant: a quote bound to another request is refused with the mismatch
// code and the request stays open.
func TestDotsDecisionAnswerRefMismatch(t *testing.T) {
	st := newOperatorStore(t)
	ctx := context.Background()
	task := newOperatorTask(t, st, "dots ref mismatch fixture")
	request, err := st.RecordDecisionRequest(ctx, task.ID, "desk", store.DecisionRequestInput{
		Question:      "Ship it?",
		Options:       store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "yes"}, {Key: "B", Label: "no"}}, AllowFree: true},
		DefaultAction: "hold",
		HumanOnly:     true,
		Block:         true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newAccessFixture(t)
	srv := httptest.NewServer(HTTPHandler(api.Service{Store: st}, api.Tokens{"tester": "bearer-secret"}, fixture.verifier))
	defer srv.Close()
	assertion := fixture.token(t, jwt.MapClaims{"email": "operator@example.com"})
	session := connectMCP(t, srv.URL, map[string]string{"Cf-Access-Jwt-Assertion": assertion})

	text, isError := toolText(t, session, "decision_answer", map[string]any{
		"request_id": request.Request.ID, "option": "A",
		"provenance": store.DecisionProvenanceOperatorDecided, "operator_quote": "A로 진행해", "decision_ref": "dr-999999-1",
	})
	if !isError || !strings.Contains(text, "provenance_ref_mismatch") {
		t.Fatalf("ref mismatch: isError=%v text=%q", isError, text)
	}
	got, _, err := st.GetTask(ctx, task.ID)
	if err != nil || got.Refs.DecisionRequest.Status != store.DecisionRequestOpen {
		t.Fatalf("request after a mismatch=%+v err=%v", got.Refs.DecisionRequest, err)
	}
}
