package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// The batch parser's unexported product constant is intentionally asserted at
// its public behavior boundary here: UI rendering must cap at the same 1000
// cards accepted by one default-toolchain form request.
const p4DecisionBatchRenderLimit = 1000

// p4URLs maps each p4Store to its schema's database URL, for the fixtures
// that write rows directly.
var p4URLs sync.Map

// p4Store opens a store on a throwaway schema (hk 3294). These tests read
// and submit the whole decision page, which lists every open decision in the
// schema: on the shared one, rows other tests left open (tasks, escalations,
// decision requests) render among the cards and take answer slots, and which
// test fails then depends on test order and timing.
func p4Store(t *testing.T) *store.Store {
	t.Helper()
	_, url := testdb.Schema(t, "ui_p4")
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	p4URLs.Store(s, url)
	t.Cleanup(func() { p4URLs.Delete(s) })
	return s
}

// p4URL is the database URL of a store p4Store opened.
func p4URL(t *testing.T, s *store.Store) string {
	t.Helper()
	url, ok := p4URLs.Load(s)
	if !ok {
		t.Fatal("p4URL: store was not opened by p4Store")
	}
	return url.(string)
}

func p4Options(recommended, allowFree bool) map[string]any {
	return map[string]any{"options": []map[string]any{{"key": "A", "label": "노드 로컬 저장", "recommended": recommended}, {"key": "B", "label": "중앙 저장 선행"}}, "allow_free": allowFree}
}

func p4PostJSON(t *testing.T, h *httptest.Server, path string, input any) (int, string) {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer node-token")
	req.Header.Set("Content-Type", "application/json")
	response, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, responseText(t, response)
}

func p4Task(t *testing.T, h *httptest.Server, lane, title, kind string) int64 {
	t.Helper()
	// The fixtures share titles ("hidden ... backlog") in the shared schema,
	// so they answer the duplicate check with new.
	status, body := p4PostJSON(t, h, "/v1/tasks", map[string]any{"lane": lane, "title": title, "kind": kind, "project": "experiment", "new": true})
	if status != http.StatusCreated {
		t.Fatalf("create task status=%d body=%q", status, body)
	}
	var output struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &output); err != nil || output.ID < 1 {
		t.Fatalf("create task body=%q err=%v", body, err)
	}
	return output.ID
}

func p4DecisionTask(t *testing.T, h *httptest.Server, lane, title string, options map[string]any) int64 {
	t.Helper()
	id := p4Task(t, h, lane, title, "implement")
	if status, body := p4PostJSON(t, h, "/v1/tasks/"+strconv.FormatInt(id, 10)+"/claim", map[string]any{"claimed_by": "worker-a", "job_id": "job-p4"}); status != http.StatusOK {
		t.Fatalf("claim status=%d body=%q", status, body)
	}
	input := map[string]any{"to": "needs_decision", "note": "어떤 저장소를 선택할까요?", "refs": map[string]any{"job_id": "job-p4", "decision_options": options}}
	if status, body := p4PostJSON(t, h, "/v1/tasks/"+strconv.FormatInt(id, 10)+"/transition", input); status != http.StatusOK {
		t.Fatalf("decision transition status=%d body=%q", status, body)
	}
	return id
}

type renderedDecisionControl struct {
	name      string
	value     string
	kind      string
	checked   bool
	disabled  bool
	submitter bool
}

type p4OpenDecisionCounts struct {
	Tasks       int
	Escalations int
	Lanes       int
}

type p4OwnedDecisionCounter struct {
	db            *pgx.Conn
	taskIDs       []int64
	escalationIDs []int64
	laneIDs       []int64
}

func (counter p4OwnedDecisionCounter) counts(t *testing.T) p4OpenDecisionCounts {
	t.Helper()
	counts := p4OpenDecisionCounts{}
	err := counter.db.QueryRow(t.Context(), `SELECT
		(SELECT COUNT(*) FROM tasks WHERE id = ANY($1) AND awaiting_decision),
		(SELECT COUNT(*) FROM relay_events e WHERE e.id = ANY($2) AND e.kind='job.escalate' AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.job_id=e.job_id
			AND resolved.kind IN ('job.joined','job.completed','job.lost','job.revoked') AND resolved.id>e.id
		) AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.kind='lane.event'
			AND resolved.owner_lane=e.owner_lane AND resolved.id>e.id
			AND resolved.event_id LIKE '%decision-escalation-' || e.id::text || '-%'
		)),
		(SELECT COUNT(*) FROM relay_events e WHERE e.id = ANY($3) AND e.kind='lane.event'
		AND e.text LIKE '[decision-needed]%' AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.kind='lane.event'
			AND resolved.owner_lane=e.owner_lane AND resolved.id>e.id
			AND resolved.text LIKE '[decision-answered] #' || e.id::text || ':%'
		))`, counter.taskIDs, counter.escalationIDs, counter.laneIDs).Scan(&counts.Tasks, &counts.Escalations, &counts.Lanes)
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func p4DecideTask(t *testing.T, s *store.Store, lane, title, state string) store.Task {
	t.Helper()
	task, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: title, Kind: "decide", CreatedBy: "node", Project: projectRef("experiment")})
	if err != nil {
		t.Fatal(err)
	}
	if state == "backlog" {
		return task
	}
	task, err = s.ClaimTask(t.Context(), task.ID, "worker-a", "job-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if state == "claimed" {
		return task
	}
	if state == "merged" {
		for _, next := range []string{"in_progress", "verifying"} {
			task, err = s.TransitionTask(t.Context(), task.ID, next, "node", "p4 decide state", nil, "")
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	task, err = s.TransitionTask(t.Context(), task.ID, state, "node", "p4 decide state", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func p4Resolve(t *testing.T, h *httptest.Server, input map[string]any) (*http.Response, string) {
	t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.URL+"/v1/decisions/resolve", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer node-token")
	req.Header.Set("Content-Type", "application/json")
	response, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response, responseText(t, response)
}

func TestUIP4ResolveClosesOnlyItsDecision(t *testing.T) {
	s := p4Store(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	lane := uiLane(t, "lane-a")
	escalation := seedRelay(t, s, lane, "job.escalate", uiLane(t, "resolve-escalation"), "", "resolve escalation", "")
	response, body := p4Resolve(t, h, map[string]any{"type": "escalation", "id": escalation.ID, "by": "lane-ops", "answer": "A: continue", "no_inject": true})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resolve escalation status=%d body=%q", response.StatusCode, body)
	}
	var output struct {
		Event struct {
			DeliveredAt *time.Time `json:"delivered_at"`
			EventID     string     `json:"event_id"`
			Text        string     `json:"text"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(body), &output); err != nil || output.Event.DeliveredAt == nil || !strings.HasPrefix(output.Event.EventID, "cli-decision-escalation-") {
		t.Fatalf("resolved event=%+v err=%v", output.Event, err)
	}
	open, err := s.ListOpenEscalations(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range open {
		if event.ID == escalation.ID {
			t.Fatal("resolved escalation remained open")
		}
	}
	// The list above is a global 1000-row window, so absence in it can be
	// vacuous; target this escalation's id directly.
	if relayEscalationOpenAt(t, p4URL(t, s), escalation.ID) {
		t.Fatal("resolved escalation remained open")
	}

	falsePositive := seedRelay(t, s, lane, "job.escalate", uiLane(t, "false-positive"), "", "must remain", "")
	if status, body := p4PostJSON(t, h, "/v1/relay/events", map[string]any{"kind": "lane.event", "owner_lane": lane, "event_id": fmt.Sprintf("web-decision-task-%d-1234abcd", falsePositive.ID), "text": "[decision] unrelated"}); status != http.StatusCreated {
		t.Fatalf("append false-positive status=%d body=%q", status, body)
	}
	open, err = s.ListOpenEscalations(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !p4HasRelay(open, falsePositive.ID) {
		t.Fatal("task answer event incorrectly closed escalation")
	}

	laneEvent := seedRelay(t, s, lane, "lane.event", "", "[decision-needed] lane resolve", "", "")
	response, body = p4Resolve(t, h, map[string]any{"type": "lane", "id": laneEvent.ID, "by": "lane-ops", "answer": "done"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resolve lane status=%d body=%q", response.StatusCode, body)
	}
	openLane, err := s.ListOpenLaneDecisions(t.Context(), 1000)
	if err != nil || p4HasRelay(openLane, laneEvent.ID) {
		t.Fatalf("resolved lane still open=%t err=%v", p4HasRelay(openLane, laneEvent.ID), err)
	}

	task := p4DecisionTask(t, h, lane, "resolve task", p4Options(true, true))
	response, body = p4Resolve(t, h, map[string]any{"type": "task", "id": task, "by": "lane-ops", "answer": "done"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("resolve task status=%d body=%q", response.StatusCode, body)
	}
	updated, _, _ := s.GetTask(t.Context(), task)
	if updated.State != "developing" {
		t.Fatalf("resolved task state=%s", updated.State)
	}
	before := uiLaneRowCountsAt(t, p4URL(t, s), lane).Relay
	response, _ = p4Resolve(t, h, map[string]any{"type": "task", "id": task, "by": "lane-ops", "answer": "again"})
	if after := uiLaneRowCountsAt(t, p4URL(t, s), lane).Relay; response.StatusCode != http.StatusConflict || after != before {
		t.Fatalf("already resolved status=%d relay before=%d after=%d", response.StatusCode, before, after)
	}
	response, _ = p4Resolve(t, h, map[string]any{"type": "task", "id": task + 1000000, "by": "bad lane", "answer": "x"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid by status=%d", response.StatusCode)
	}
	longTextTask := p4DecisionTask(t, h, lane, "long resolve", p4Options(true, true))
	longAnswer := strings.Repeat("x", 2049)
	before = uiLaneRowCountsAt(t, p4URL(t, s), lane).Relay
	response, body = p4Resolve(t, h, map[string]any{"type": "task", "id": longTextTask, "by": "lane-ops", "answer": longAnswer})
	var longOutput struct {
		Event store.RelayEvent `json:"event"`
	}
	if err := json.Unmarshal([]byte(body), &longOutput); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("long resolve status=%d body=%q err=%v", response.StatusCode, body, err)
	}
	if !strings.Contains(longOutput.Event.Text, longAnswer) {
		t.Fatalf("long resolve text lost: event bytes=%d answer bytes=%d", len(longOutput.Event.Text), len(longAnswer))
	}
	if after := uiLaneRowCountsAt(t, p4URL(t, s), lane).Relay; after != before+1 {
		t.Fatalf("long resolve relay before=%d after=%d", before, after)
	}
}

func p4HasRelay(events any, id int64) bool {
	value := reflect.ValueOf(events)
	for index := 0; index < value.Len(); index++ {
		if value.Index(index).FieldByName("ID").Int() == id {
			return true
		}
	}
	return false
}

// The React board replaced the htmx operator view. Server-side, /ui/queue is
// only a mount point and /ui/api/board/tasks must return the complete task
// set — the operator-only narrowing is a client filter covered by the bundle
// tests, never a server projection that could lose data.
func TestUIP4QueueOperatorView(t *testing.T) {
	s := p4Store(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	for _, task := range []struct{ title, kind string }{{"hidden implement backlog", "implement"}, {"hidden fix backlog", "fix"}, {"hidden ops backlog", "ops"}, {"shown decide backlog", "decide"}} {
		_ = p4Task(t, h, lane, task.title, task.kind)
	}
	decision := p4DecisionTask(t, h, lane, "shown needs decision", p4Options(true, true))
	_ = decision
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue", assertion, "")
	body := responseText(t, response)
	for _, title := range []string{"hidden implement backlog", "hidden fix backlog", "hidden ops backlog", "shown decide backlog", "shown needs decision"} {
		if strings.Contains(body, title) {
			t.Fatalf("board mount page rendered task data %q", title)
		}
	}
	if !strings.Contains(body, `id="shell-root"`) {
		t.Fatalf("queue page did not mount the board: %q", body)
	}
	before := uiLaneRowCountsAt(t, p4URL(t, s), lane)
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/board/tasks?lane="+lane, assertion, "")
	if body = responseText(t, response); response.StatusCode != http.StatusOK {
		t.Fatalf("board tasks status=%d body=%q", response.StatusCode, body)
	}
	for _, title := range []string{"hidden implement backlog", "hidden fix backlog", "hidden ops backlog", "shown decide backlog", "shown needs decision"} {
		if !strings.Contains(body, title) {
			t.Fatalf("board API lost task %q: %q", title, body)
		}
	}
	if after := uiLaneRowCountsAt(t, p4URL(t, s), lane); after != before {
		t.Fatalf("read-only board API changed rows: before=%+v after=%+v", before, after)
	}
}

func TestUIP4QueueOperatorViewExcludesInProgressImplement(t *testing.T) {
	s := p4Store(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	implement := createUITask(t, s, lane, "FT6 in-progress implement")
	claimAndTransition(t, s, implement, "in_progress", "working")
	decision := createUITask(t, s, lane, "FT6 needs decision")
	claimAndTransition(t, s, decision, "needs_decision", "choose")

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/board/tasks?lane="+lane, assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, implement.Title) || !strings.Contains(body, decision.Title) {
		t.Fatalf("board API status=%d body=%q", response.StatusCode, body)
	}
	if !strings.Contains(body, `"state":"developing"`) || !strings.Contains(body, `"awaiting_decision":true`) {
		t.Fatalf("board API dropped canonical states: %q", body)
	}
}

func TestUIP4QueueOperatorViewIncludesNonterminalDecide(t *testing.T) {
	s := p4Store(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	inProgress := p4DecideTask(t, s, lane, "SHOULD-3 in-progress decide", "in_progress")
	claimed := p4DecideTask(t, s, lane, "SHOULD-3 claimed decide", "claimed")
	merged := p4DecideTask(t, s, lane, "SHOULD-3 merged decide", "merged")
	dropped := p4DecideTask(t, s, lane, "SHOULD-3 dropped decide", "dropped")
	backlog := p4DecideTask(t, s, lane, "SHOULD-3 backlog decide", "backlog")

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/board/tasks?lane="+lane, assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, inProgress.Title) || !strings.Contains(body, claimed.Title) {
		t.Fatalf("SHOULD-3 board API omitted nonterminal decide task: %q", body)
	}
	for _, task := range []store.Task{merged, dropped, backlog} {
		if !strings.Contains(body, task.Title) {
			t.Fatalf("SHOULD-3 board API lost terminal/backlog decide task %q: %q", task.Title, body)
		}
	}
}

// p4LaneDecisions seeds open lane decisions directly, which is the cheapest way
// to render a form larger than the parse limit.
func p4LaneDecisions(t *testing.T, s *store.Store, lane string, count int) []int64 {
	t.Helper()
	ids := make([]int64, 0, count)
	for index := 0; index < count; index++ {
		text := "[decision-needed] HT lane " + strconv.Itoa(index)
		event := seedRelay(t, s, lane, "lane.event", "", text, "", "")
		ids = append(ids, event.ID)
	}
	// The package shares one database. Left open, these rows would render on
	// every later test's decision page and slow the suite to a timeout. Each
	// question closes only through an exact "[decision-answered] #<id>:" answer,
	// so cleanup writes one per seeded row, which is the same rule the console
	// itself uses. t.Context is already cancelled by the time cleanup runs, so
	// this uses its own context.
	t.Cleanup(func() {
		for index, id := range ids {
			if _, _, err := s.AppendRelayEvent(context.Background(), store.RelayEvent{
				Kind: "lane.event", JobID: "ht-cleanup", OwnerLane: lane,
				Text:    "[decision-answered] #" + strconv.FormatInt(id, 10) + ": HT cleanup",
				EventID: fmt.Sprintf("ht-cleanup-%s-%d", lane, index),
			}); err != nil {
				t.Logf("lane decision cleanup: %v", err)
			}
		}
	})
	return ids
}

// p4Escalations seeds open [decision-needed] escalations under one job id so
// a single completed event closes the whole batch on cleanup.
func p4Escalations(t *testing.T, s *store.Store, lane string, count int, question ...string) []int64 {
	t.Helper()
	jobID := "ht-esc-" + lane
	ids := make([]int64, 0, count)
	for index := 0; index < count; index++ {
		text := "[decision-needed] HT escalation " + strconv.Itoa(index)
		if len(question) > 0 {
			text += "\n" + question[0]
		}
		event := seedRelay(t, s, lane, "job.escalate", jobID, "", text, "")
		ids = append(ids, event.ID)
	}
	t.Cleanup(func() {
		if _, _, err := s.AppendRelayEvent(context.Background(), store.RelayEvent{
			Kind: "job.completed", JobID: jobID, OwnerLane: lane,
			ReportPath: "ht-cleanup.md", Reason: "ht-cleanup",
		}); err != nil {
			t.Logf("escalation cleanup: %v", err)
		}
	})
	return ids
}
