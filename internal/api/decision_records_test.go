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

func decisionAPIServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	_, dbURL := testdb.Schema(t, "decision_api")
	st, err := store.Open(context.Background(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"tester": "tok"}}.Handler())
	t.Cleanup(h.Close)
	t.Cleanup(st.Close)
	return h, st
}

func decisionAPICall(t *testing.T, h *httptest.Server, method, path string, body any, out any) int {
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
	req.Header.Set("Authorization", "Bearer tok")
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
