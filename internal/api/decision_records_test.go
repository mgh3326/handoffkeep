package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func decisionAPIServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	_, dbURL := testdb.Schema(t, "decision_api")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"tester": "tok", "operator": "operator-tok"}}.Handler())
	t.Cleanup(h.Close)
	t.Cleanup(st.Close)
	return h, st
}

func decisionAPICall(t *testing.T, h *httptest.Server, method, path string, body any, out any) int {
	return decisionAPICallAs(t, h, "tok", method, path, body, out)
}

func decisionAPICallAs(t *testing.T, h *httptest.Server, token, method, path string, body any, out any) int {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, h.URL+path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

type decisionListBody struct {
	ServerTime       time.Time              `json:"server_time"`
	DecisionRequests []store.DecisionRecord `json:"decision_requests"`
}

func (b decisionListBody) has(id int64) bool {
	for _, x := range b.DecisionRequests {
		if x.ID == id {
			return true
		}
	}
	return false
}

func TestDecisionRequestsListSnoozeAndSourcesAPI(t *testing.T) {
	h, st := decisionAPIServer(t)
	ctx := context.Background()
	for _, id := range []string{"Q-20261105-21", "Q-20261105-22"} {
		if _, _, err := st.UpsertChatQuestion(ctx, store.ChatQuestion{ID: id, Lane: "api-lane", Body: "q " + id}); err != nil {
			t.Fatal(err)
		}
	}
	var list decisionListBody
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/decision-requests?source=chat&lane=api-lane", nil, &list); status != http.StatusOK || len(list.DecisionRequests) != 2 || list.ServerTime.IsZero() {
		t.Fatalf("list: %d %+v", status, list)
	}
	first, second := list.DecisionRequests[0], list.DecisionRequests[1]
	if first.SourceRef != "Q-20261105-21" || second.SourceRef != "Q-20261105-22" || first.Status != store.DecisionStatusOpen {
		t.Fatalf("order or fields: %+v", list.DecisionRequests)
	}

	var snoozed store.DecisionRecord
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if status := decisionAPICall(t, h, http.MethodPost, fmt.Sprintf("/v1/decision-requests/%d/snooze", first.ID), map[string]any{"until": until}, &snoozed); status != http.StatusOK || !snoozed.Snoozed || snoozed.SnoozedUntil == nil || !snoozed.SnoozedUntil.Equal(until) {
		t.Fatalf("snooze: %d %+v", status, snoozed)
	}
	list = decisionListBody{}
	decisionAPICall(t, h, http.MethodGet, "/v1/decision-requests?source=chat", nil, &list)
	if list.has(first.ID) || !list.has(second.ID) {
		t.Fatalf("default list after snooze: %+v", list.DecisionRequests)
	}
	list = decisionListBody{}
	decisionAPICall(t, h, http.MethodGet, "/v1/decision-requests?source=chat&include_snoozed=true", nil, &list)
	if !list.has(first.ID) {
		t.Fatalf("include_snoozed list: %+v", list.DecisionRequests)
	}
	if q, _, err := st.GetChatQuestion(ctx, "Q-20261105-21"); err != nil || q.State != "pending" {
		t.Fatalf("snooze touched the source: %+v %v", q, err)
	}

	if _, err := st.TransitionChatQuestion(ctx, "Q-20261105-22", "resolved", "desk"); err != nil {
		t.Fatal(err)
	}
	list = decisionListBody{}
	decisionAPICall(t, h, http.MethodGet, "/v1/decision-requests?status=answered", nil, &list)
	if !list.has(second.ID) || list.DecisionRequests[0].AnsweredBy != "desk" {
		t.Fatalf("answered list: %+v", list.DecisionRequests)
	}

	var sources store.OpenDecisionSources
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/decision-requests/sources", nil, &sources); status != http.StatusOK || len(sources.Items) != 1 || sources.Items[0].Source != store.DecisionSourceChat || sources.Items[0].SourceRef != "Q-20261105-21" || sources.ServerTime.IsZero() {
		t.Fatalf("sources: %d %+v", status, sources)
	}

	for _, tc := range []struct {
		name, method, path string
		body               any
		status             int
		code               string
	}{
		{"bad status", http.MethodGet, "/v1/decision-requests?status=closed", nil, http.StatusBadRequest, "invalid_context"},
		{"bad source", http.MethodGet, "/v1/decision-requests?source=email", nil, http.StatusBadRequest, "invalid_context"},
		{"bad include_snoozed", http.MethodGet, "/v1/decision-requests?include_snoozed=maybe", nil, http.StatusBadRequest, "invalid_context"},
		{"bad limit", http.MethodGet, "/v1/decision-requests?limit=0", nil, http.StatusBadRequest, "invalid_context"},
		{"bad id", http.MethodPost, "/v1/decision-requests/x/snooze", map[string]any{"until": until}, http.StatusBadRequest, "invalid_context"},
		{"unknown field", http.MethodPost, fmt.Sprintf("/v1/decision-requests/%d/snooze", first.ID), map[string]any{"until": until, "status": "answered"}, http.StatusBadRequest, "invalid_context"},
		{"missing until", http.MethodPost, fmt.Sprintf("/v1/decision-requests/%d/snooze", first.ID), map[string]any{}, http.StatusBadRequest, "invalid_context"},
		{"missing record", http.MethodPost, fmt.Sprintf("/v1/decision-requests/%d/snooze", second.ID+1000), map[string]any{"until": until}, http.StatusNotFound, "not_found"},
		{"answered record", http.MethodPost, fmt.Sprintf("/v1/decision-requests/%d/snooze", second.ID), map[string]any{"until": until}, http.StatusConflict, "decision_request_not_open"},
	} {
		var body map[string]any
		if status := decisionAPICall(t, h, tc.method, tc.path, tc.body, &body); status != tc.status || body["error"] != tc.code {
			t.Errorf("%s: %d %v, want %d %s", tc.name, status, body, tc.status, tc.code)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, h.URL+"/v1/decision-requests", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: %d", resp.StatusCode)
	}
}

func TestRecordSessionDecisionAPIIsIdempotentAndListed(t *testing.T) {
	h, _ := decisionAPIServer(t)
	in := store.SessionDecisionInput{
		Key: "chat-a27", Lane: "design-lane", Reading: "Use the hk web and MCP paths.",
		Quote: "Let's answer from hk web or MCP.", DecidedAt: time.Date(2026, 11, 11, 5, 0, 0, 0, time.UTC),
		Grounds: []string{"hk:doc decision/2026-11-11/model-routing-escalation"},
	}
	var refusal map[string]string
	if status := decisionAPICall(t, h, http.MethodPost, "/v1/decisions/record", in, &refusal); status != http.StatusForbidden || refusal["error"] != "operator_required" {
		t.Fatalf("unverified bearer write: %d %+v", status, refusal)
	}
	var listed decisionListBody
	if status := decisionAPICallAs(t, h, "operator-tok", http.MethodGet, "/v1/decision-requests?status=answered&source=session_chat", nil, &listed); status != http.StatusOK || len(listed.DecisionRequests) != 0 {
		t.Fatalf("unverified bearer left a record: %d %+v", status, listed)
	}
	var first store.SessionDecisionResult
	if status := decisionAPICallAs(t, h, "operator-tok", http.MethodPost, "/v1/decisions/record", in, &first); status != http.StatusCreated || first.Duplicate || first.Record.Answer != in.Quote || first.Record.AnsweredBy != "operator" {
		t.Fatalf("first record: %d %+v", status, first)
	}
	var replay store.SessionDecisionResult
	if status := decisionAPICallAs(t, h, "operator-tok", http.MethodPost, "/v1/decisions/record", in, &replay); status != http.StatusOK || !replay.Duplicate || replay.Record.ID != first.Record.ID {
		t.Fatalf("replay: %d %+v", status, replay)
	}
	in.Quote = "changed"
	if status := decisionAPICallAs(t, h, "operator-tok", http.MethodPost, "/v1/decisions/record", in, nil); status != http.StatusConflict {
		t.Fatalf("key reuse status=%d, want conflict", status)
	}
	listed = decisionListBody{}
	if status := decisionAPICall(t, h, http.MethodGet, "/v1/decision-requests?status=answered&source=session_chat", nil, &listed); status != http.StatusOK || len(listed.DecisionRequests) != 1 || listed.DecisionRequests[0].Reading != "Use the hk web and MCP paths." || len(listed.DecisionRequests[0].Grounds) != 1 {
		t.Fatalf("listed record: %d %+v", status, listed)
	}
}

// hk 4518: a maximal session decision — ten 500-byte grounds — is larger
// than bodySmall; the route takes bodyLarge.
func TestRecordSessionDecisionAPILargeGrounds(t *testing.T) {
	h, _ := decisionAPIServer(t)
	grounds := make([]string, 10)
	for i := range grounds {
		grounds[i] = fmt.Sprintf("hk:doc decision/2026-11-13/ground-%02d-%s", i, strings.Repeat("g", 470))
	}
	in := store.SessionDecisionInput{
		Key: "chat-big", Lane: "design-lane", Reading: "Use every ground.",
		Quote: "All of them.", DecidedAt: time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC),
		Grounds: grounds,
	}
	var out store.SessionDecisionResult
	if status := decisionAPICallAs(t, h, "operator-tok", http.MethodPost, "/v1/decisions/record", in, &out); status != http.StatusCreated || out.Duplicate || len(out.Record.Grounds) != 10 {
		t.Fatalf("record: %d %+v", status, out)
	}
}
