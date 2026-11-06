package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// TestDecisionResolveSessionHTTP covers the channel field of the bearer
// resolve route (hk 3064): session answers as a relay with the claim, an
// unknown channel is invalid_context, and an omitted channel is today's cli.
func TestDecisionResolveSessionHTTP(t *testing.T) {
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "decision_session_api")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{
		"operator": "op-tok", "operator-desk": "desk-tok",
	}}.Handler())
	t.Cleanup(h.Close)

	seed := func(t *testing.T, policy string, humanOnly bool) (store.Task, store.DecisionRequest) {
		t.Helper()
		x, err := st.CreateTask(ctx, store.Task{Lane: fmt.Sprintf("dr-session-%d", time.Now().UnixNano()), Title: "decision session", Kind: "implement", CreatedBy: "dr-http", Project: &apiTestProject})
		if err != nil {
			t.Fatal(err)
		}
		if x, err = st.ClaimTask(ctx, x.ID, "dr-http", "job-1", ""); err != nil {
			t.Fatal(err)
		}
		if _, err = st.TransitionTask(ctx, x.ID, "in_progress", "dr-http", "step", nil, ""); err != nil {
			t.Fatal(err)
		}
		in := decisionHTTPInput("session?", humanOnly)
		in.AnswerPolicy = policy
		out, err := st.RecordDecisionRequest(ctx, x.ID, "dr-http", in)
		if err != nil {
			t.Fatal(err)
		}
		return x, out.Request
	}
	post := func(t *testing.T, token string, taskID int64, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1/tasks/%d/decision-request/resolve", h.URL, taskID), bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	session := func(request store.DecisionRequest) map[string]any {
		return map[string]any{
			"request_id": request.ID, "kind": "answered", "option": "A", "channel": "session",
			"provenance": "operator_decided", "operator_quote": "A로 가자", "decision_ref": request.ID,
		}
	}

	t.Run("session quote answers", func(t *testing.T) {
		x, request := seed(t, "", false)
		status, body := post(t, "op-tok", x.ID, session(request))
		if status != http.StatusOK || body["result"] != store.DecisionResultAnswered {
			t.Fatalf("status=%d body=%v", status, body)
		}
		resolution := body["request"].(map[string]any)["resolution"].(map[string]any)
		if resolution["operator_quote"] != "A로 가자" {
			t.Fatalf("resolution=%v", resolution)
		}
	})
	t.Run("session on a direct policy waits", func(t *testing.T) {
		// Even the operator token: the session channel is never verified.
		x, request := seed(t, store.DecisionPolicyOperatorDirect, false)
		status, body := post(t, "op-tok", x.ID, session(request))
		if status != http.StatusAccepted || body["result"] != store.DecisionResultPendingConfirmation {
			t.Fatalf("status=%d body=%v", status, body)
		}
	})
	t.Run("unknown channel", func(t *testing.T) {
		x, request := seed(t, "", false)
		status, body := post(t, "desk-tok", x.ID, map[string]any{"request_id": request.ID, "kind": "answered", "option": "A", "channel": "x"})
		if status != http.StatusBadRequest || body["error"] != "invalid_context" {
			t.Fatalf("status=%d body=%v", status, body)
		}
	})
	t.Run("unknown policy", func(t *testing.T) {
		x, _ := seed(t, "", false)
		in := decisionHTTPInput("other?", false)
		in.AnswerPolicy = "anyone"
		raw, _ := json.Marshal(in)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1/tasks/%d/decision-request", h.URL, x.ID), bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer desk-tok")
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body := map[string]any{}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if resp.StatusCode != http.StatusBadRequest || body["error"] != "invalid_decision_request: answer_policy must be operator_direct or operator_relay_allowed" {
			t.Fatalf("status=%d body=%v", resp.StatusCode, body)
		}
	})
	t.Run("omitted channel is cli", func(t *testing.T) {
		for _, c := range []struct {
			token     string
			humanOnly bool
			status    int
			result    string
		}{
			{"op-tok", true, http.StatusOK, store.DecisionResultAnswered},
			{"desk-tok", true, http.StatusAccepted, store.DecisionResultPendingConfirmation},
			{"desk-tok", false, http.StatusOK, store.DecisionResultAnswered},
		} {
			x, request := seed(t, "", c.humanOnly)
			status, body := post(t, c.token, x.ID, map[string]any{"request_id": request.ID, "kind": "answered", "option": "A"})
			if status != c.status || body["result"] != c.result {
				t.Fatalf("%+v: status=%d body=%v", c, status, body)
			}
		}
	})
}
