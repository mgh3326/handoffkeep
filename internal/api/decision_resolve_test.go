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

func decisionHTTPInput(question string, humanOnly bool) store.DecisionRequestInput {
	return store.DecisionRequestInput{
		Question: question,
		Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{
			{Key: "A", Label: "first", Recommended: true},
			{Key: "B", Label: "second"},
		}},
		DefaultAction: "자동 적용 없음",
		HumanOnly:     humanOnly,
	}
}

// TestDecisionResolveHTTP maps each row of the result table to its status,
// error code and result key on both bearer routes (hk 3061).
func TestDecisionResolveHTTP(t *testing.T) {
	ctx := context.Background()
	_, dbURL := testdb.Schema(t, "decision_resolve_api")
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{
		"operator": "op-tok", "operator-desk": "desk-tok", "worker": "worker-tok", "berry": "berry-tok",
	}}.Handler())
	t.Cleanup(h.Close)

	newTask := func(t *testing.T) store.Task {
		t.Helper()
		x, err := st.CreateTask(ctx, store.Task{Lane: fmt.Sprintf("dr-http-%d", time.Now().UnixNano()), Title: "decision http", Kind: "implement", CreatedBy: "dr-http", Project: &apiTestProject})
		if err != nil {
			t.Fatal(err)
		}
		if x, err = st.ClaimTask(ctx, x.ID, "dr-http", "job-1", ""); err != nil {
			t.Fatal(err)
		}
		if _, err = st.TransitionTask(ctx, x.ID, "in_progress", "dr-http", "step", nil, ""); err != nil {
			t.Fatal(err)
		}
		return x
	}
	record := func(t *testing.T, taskID int64, in store.DecisionRequestInput) store.DecisionRequest {
		t.Helper()
		out, err := st.RecordDecisionRequest(ctx, taskID, "dr-http", in)
		if err != nil {
			t.Fatal(err)
		}
		return out.Request
	}
	post := func(t *testing.T, token, path string, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.URL+path, bytes.NewReader(raw))
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
	cli := func(t *testing.T, token string, taskID int64, body map[string]any) (int, map[string]any) {
		return post(t, token, fmt.Sprintf("/v1/tasks/%d/decision-request/resolve", taskID), body)
	}
	assistant := func(t *testing.T, body map[string]any) (int, map[string]any) {
		return post(t, "berry-tok", "/v1/assistant/decisions/resolve", body)
	}
	expect := func(t *testing.T, status int, body map[string]any, wantStatus int, wantCode, wantResult string) {
		t.Helper()
		if status != wantStatus {
			t.Fatalf("status=%d body=%v want %d", status, body, wantStatus)
		}
		if wantCode != "" && body["error"] != wantCode {
			t.Fatalf("body=%v want error %s", body, wantCode)
		}
		if wantResult != "" && body["result"] != wantResult {
			t.Fatalf("body=%v want result %s", body, wantResult)
		}
	}
	status := func(t *testing.T, id int64) string {
		t.Helper()
		x, _, err := st.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return x.Refs.DecisionRequest.Status
	}

	t.Run("answered_and_identical_resend", func(t *testing.T) {
		for _, route := range []string{"cli", "assistant"} {
			task := newTask(t)
			request := record(t, task.ID, decisionHTTPInput("q "+route, false))
			body := map[string]any{"request_id": request.ID, "option": "A", "kind": "answered"}
			call := func() (int, map[string]any) {
				if route == "cli" {
					return cli(t, "worker-tok", task.ID, body)
				}
				return assistant(t, body)
			}
			s1, b1 := call()
			expect(t, s1, b1, http.StatusOK, "", "answered")
			if b1["resolution_id"] == nil || b1["duplicate"] != false {
				t.Fatalf("%s first=%v", route, b1)
			}
			s2, b2 := call()
			expect(t, s2, b2, http.StatusOK, "", "answered")
			if b2["duplicate"] != true || b2["resolution_id"] != b1["resolution_id"] {
				t.Fatalf("%s resend=%v", route, b2)
			}
		}
	})

	t.Run("stale_and_superseded_ids", func(t *testing.T) {
		task := newTask(t)
		first := record(t, task.ID, decisionHTTPInput("first", false))
		next := decisionHTTPInput("second", false)
		next.Supersedes = first.ID
		record(t, task.ID, next)
		for _, route := range []string{"cli", "assistant"} {
			body := map[string]any{"request_id": first.ID, "option": "A", "kind": "answered"}
			var s int
			var b map[string]any
			if route == "cli" {
				s, b = cli(t, "worker-tok", task.ID, body)
			} else {
				s, b = assistant(t, body)
			}
			expect(t, s, b, http.StatusConflict, "decision_request_stale", "stale_revision")
			body["request_id"] = fmt.Sprintf("dr-%d-9", task.ID)
			if route == "cli" {
				s, b = cli(t, "worker-tok", task.ID, body)
			} else {
				s, b = assistant(t, body)
			}
			expect(t, s, b, http.StatusConflict, "decision_request_stale", "superseded")
		}
	})

	t.Run("resolved_rows", func(t *testing.T) {
		conflict := newTask(t)
		cr := record(t, conflict.ID, decisionHTTPInput("conflict", false))
		s, b := cli(t, "worker-tok", conflict.ID, map[string]any{"request_id": cr.ID, "option": "A", "kind": "answered"})
		expect(t, s, b, http.StatusOK, "", "answered")
		s, b = cli(t, "worker-tok", conflict.ID, map[string]any{"request_id": cr.ID, "option": "B", "kind": "answered"})
		expect(t, s, b, http.StatusConflict, "decision_request_resolved", "conflict")
		s, b = assistant(t, map[string]any{"request_id": cr.ID, "option": "B"})
		expect(t, s, b, http.StatusConflict, "decision_request_resolved", "conflict")

		expired := newTask(t)
		er := record(t, expired.ID, decisionHTTPInput("expired", false))
		s, b = cli(t, "worker-tok", expired.ID, map[string]any{"request_id": er.ID, "kind": "default_applied", "receipt": "hk:doc receipt/x"})
		expect(t, s, b, http.StatusOK, "", "answered")
		s, b = cli(t, "worker-tok", expired.ID, map[string]any{"request_id": er.ID, "option": "A", "kind": "answered"})
		expect(t, s, b, http.StatusConflict, "decision_request_resolved", "expired")
		s, b = assistant(t, map[string]any{"request_id": er.ID, "option": "A"})
		expect(t, s, b, http.StatusConflict, "decision_request_resolved", "expired")

		withdrawn := newTask(t)
		wr := record(t, withdrawn.ID, decisionHTTPInput("withdrawn", false))
		s, b = cli(t, "worker-tok", withdrawn.ID, map[string]any{"request_id": wr.ID, "kind": "withdrawn", "text": "not needed"})
		expect(t, s, b, http.StatusOK, "", "answered")
		s, b = cli(t, "worker-tok", withdrawn.ID, map[string]any{"request_id": wr.ID, "option": "A", "kind": "answered"})
		expect(t, s, b, http.StatusConflict, "decision_request_resolved", "superseded")
		s, b = assistant(t, map[string]any{"request_id": wr.ID, "option": "A"})
		expect(t, s, b, http.StatusConflict, "decision_request_resolved", "superseded")
	})

	t.Run("human_only", func(t *testing.T) {
		judged := newTask(t)
		jr := record(t, judged.ID, decisionHTTPInput("judged", true))
		s, b := assistant(t, map[string]any{"request_id": jr.ID, "option": "A"})
		expect(t, s, b, http.StatusConflict, "decision_request_human_only", "")
		if got := status(t, judged.ID); got != store.DecisionRequestOpen {
			t.Fatalf("status=%s", got)
		}

		quoted := newTask(t)
		qr := record(t, quoted.ID, decisionHTTPInput("quoted", true))
		s, b = assistant(t, map[string]any{"request_id": qr.ID, "option": "A", "provenance": "operator_decided", "operator_quote": "A로 해", "decision_ref": qr.ID})
		expect(t, s, b, http.StatusAccepted, "", "pending_confirmation")
		if got := status(t, quoted.ID); got != store.DecisionRequestOpen {
			t.Fatalf("a relay closed the request: %s", got)
		}

		for _, client := range []string{"worker-tok", "desk-tok"} {
			s, b = cli(t, client, quoted.ID, map[string]any{"request_id": qr.ID, "option": "A", "kind": "answered"})
			expect(t, s, b, http.StatusAccepted, "", "pending_confirmation")
		}
		s, b = cli(t, "op-tok", quoted.ID, map[string]any{"request_id": qr.ID, "option": "A", "kind": "answered"})
		expect(t, s, b, http.StatusOK, "", "answered")
		if got := status(t, quoted.ID); got != store.DecisionRequestAnswered {
			t.Fatalf("operator client left %s", got)
		}
	})

	t.Run("validation_and_event_key", func(t *testing.T) {
		task := newTask(t)
		request := record(t, task.ID, decisionHTTPInput("validation", false))
		s, b := cli(t, "worker-tok", task.ID, map[string]any{"request_id": request.ID, "option": "F", "kind": "answered"})
		if s != http.StatusBadRequest {
			t.Fatalf("option not in request: %d %v", s, b)
		}
		s, b = assistant(t, map[string]any{"request_id": request.ID, "option": "A", "provenance": "operator"})
		expect(t, s, b, http.StatusBadRequest, "invalid_provenance", "")
		s, b = cli(t, "worker-tok", task.ID, map[string]any{"request_id": request.ID, "option": "A", "kind": "answered", "event_key": "k-1"})
		expect(t, s, b, http.StatusOK, "", "answered")
		s, b = cli(t, "worker-tok", task.ID, map[string]any{"request_id": request.ID, "option": "A", "kind": "answered", "event_key": "k-1"})
		expect(t, s, b, http.StatusOK, "", "answered")
		if b["duplicate"] != true {
			t.Fatalf("replay=%v", b)
		}
		s, b = cli(t, "worker-tok", task.ID, map[string]any{"request_id": request.ID, "option": "B", "kind": "answered", "event_key": "k-1"})
		expect(t, s, b, http.StatusConflict, "decision_event_key_conflict", "")
	})
}
