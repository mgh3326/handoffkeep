package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestUIDecisions(t *testing.T) {
	_, url := testdb.Schema(t, "ui_decisions")
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	pool, err := pgxpool.New(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	project := fmt.Sprintf("decisions-%d", time.Now().UnixNano())
	if _, err = s.AddTaskProject(t.Context(), project, "fixture"); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(t.Context(), store.Task{Lane: "fixture", Title: "decision fixture", Kind: "implement", CreatedBy: "fixture", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	req, err := s.RecordDecisionRequest(t.Context(), task.ID, "fixture", store.DecisionRequestInput{Question: "Choose?", Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{{Key: "A", Label: "yes"}}}, DefaultAction: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err = pool.QueryRow(t.Context(), `SELECT id FROM decision_requests WHERE source='task_request' AND source_ref=$1`, req.Request.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	token := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	service := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer service.Close()
	serviceResponse := p3Request(t, service.Client(), http.MethodGet, service.URL+"/ui/api/decisions?tab=open", p3ServiceAssertion(t, fixture), nil, "")
	if body := responseText(t, serviceResponse); serviceResponse.StatusCode != http.StatusForbidden || !strings.Contains(body, `"forbidden"`) {
		t.Fatalf("service status=%d body=%s", serviceResponse.StatusCode, body)
	}
	for path, want := range map[string]int{"/ui/decisions": 200, fmt.Sprintf("/ui/decisions/%d", id): 200, "/ui/decisions/abc": 400, "/ui/decisions/legacy": 200, "/ui/api/decisions/999999999": 404, "/ui/api/decisions?tab=x": 400} {
		r := uiRequest(t, h.Client(), http.MethodGet, h.URL+path, token, "")
		body := responseText(t, r)
		if r.StatusCode != want {
			t.Fatalf("%s status=%d body=%s", path, r.StatusCode, body)
		}
		if (path == "/ui/decisions" || path == fmt.Sprintf("/ui/decisions/%d", id)) && (!strings.Contains(body, "shell-root") || !strings.Contains(body, "hk-csrf")) {
			t.Fatalf("shell missing from %s", path)
		}
		if path == "/ui/api/decisions?tab=x" && !strings.Contains(body, "invalid_tab") {
			t.Fatal(body)
		}
	}
	path := fmt.Sprintf("%s/ui/api/decisions/%d", h.URL, id)
	var detail map[string]any
	if status := boardJSON(t, h.Client(), path, token, &detail); status != 200 {
		t.Fatal(status)
	}
	for _, key := range []string{"generated_at", "record", "task", "request", "history", "attempts", "answerable", "disabled_reason"} {
		if _, ok := detail[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if len(detail) != 8 || detail["answerable"] != true || detail["disabled_reason"] != "" {
		t.Fatalf("detail=%v", detail)
	}
	var attention struct {
		Decisions struct {
			Count   int `json:"count"`
			Overdue int `json:"overdue"`
		} `json:"decisions"`
	}
	if status := boardJSON(t, h.Client(), h.URL+"/ui/api/attention", token, &attention); status != http.StatusOK || attention.Decisions.Overdue > attention.Decisions.Count {
		t.Fatalf("attention=%+v status=%d", attention, status)
	}
	var flagID int64
	if err = pool.QueryRow(t.Context(), `INSERT INTO decision_requests(source,source_ref,task_id,question,created_at,updated_at) VALUES('task_flag','fixture',$1,'legacy',now(),now()) RETURNING id`, task.ID).Scan(&flagID); err != nil {
		t.Fatal(err)
	}
	flagPath := fmt.Sprintf("%s/ui/api/decisions/%d", h.URL, flagID)
	if status := boardJSON(t, h.Client(), flagPath, token, &detail); status != 200 || detail["disabled_reason"] != "source_elsewhere" {
		t.Fatalf("flag=%v status=%d", detail, status)
	}
	next, err := s.RecordDecisionRequest(t.Context(), task.ID, "fixture", store.DecisionRequestInput{Question: "Choose again?", Options: store.DecisionOptions{AllowFree: true, Options: []store.DecisionOption{{Key: "A", Label: "yes"}}}, DefaultAction: "wait", Supersedes: req.Request.ID})
	if err != nil {
		t.Fatal(err)
	}
	if status := boardJSON(t, h.Client(), path, token, &detail); status != 200 || detail["disabled_reason"] != "superseded" {
		t.Fatalf("superseded=%v status=%d", detail, status)
	}
	if _, err = s.ResolveDecisionRequest(t.Context(), task.ID, "fixture", store.DecisionResolveInput{RequestID: next.Request.ID, Kind: store.DecisionRequestAnswered, Option: "A"}); err != nil {
		t.Fatal(err)
	}
	if err = pool.QueryRow(t.Context(), `SELECT id FROM decision_requests WHERE source='task_request' AND source_ref=$1`, next.Request.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	path = fmt.Sprintf("%s/ui/api/decisions/%d", h.URL, id)
	if status := boardJSON(t, h.Client(), path, token, &detail); status != 200 || detail["disabled_reason"] != "closed" {
		t.Fatalf("closed=%v status=%d", detail, status)
	}
	if _, err = pool.Exec(t.Context(), `ALTER TABLE decision_requests RENAME TO decisions_unavailable`); err != nil {
		t.Fatal(err)
	}
	r := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/decisions?tab=open", token, "")
	defer r.Body.Close()
	var failure map[string]any
	_ = json.NewDecoder(r.Body).Decode(&failure)
	if r.StatusCode != 500 || failure["error"] != "internal_error" {
		t.Fatalf("failure=%v status=%d", failure, r.StatusCode)
	}
}
