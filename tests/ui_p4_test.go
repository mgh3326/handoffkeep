package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

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
	status, body := p4PostJSON(t, h, "/v1/tasks", map[string]any{"lane": lane, "title": title, "kind": kind})
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
	if status, body := p4PostJSON(t, h, "/v1/tasks/"+strconv.FormatInt(id, 10)+"/claim", map[string]any{"claimed_by": "worker-a"}); status != http.StatusOK {
		t.Fatalf("claim status=%d body=%q", status, body)
	}
	input := map[string]any{"to": "needs_decision", "note": "어떤 저장소를 선택할까요?", "refs": map[string]any{"job_id": "job-p4", "decision_options": options}}
	if status, body := p4PostJSON(t, h, "/v1/tasks/"+strconv.FormatInt(id, 10)+"/transition", input); status != http.StatusOK {
		t.Fatalf("decision transition status=%d body=%q", status, body)
	}
	return id
}

func p4LegacyDecisionTask(t *testing.T, h *httptest.Server, lane, title string) int64 {
	t.Helper()
	id := p4Task(t, h, lane, title, "implement")
	if status, body := p4PostJSON(t, h, "/v1/tasks/"+strconv.FormatInt(id, 10)+"/claim", map[string]any{"claimed_by": "worker-a"}); status != http.StatusOK {
		t.Fatalf("claim status=%d body=%q", status, body)
	}
	input := map[string]any{"to": "needs_decision", "note": "배포를 진행할까요?\noptions: 승인 | 반려", "refs": map[string]any{"job_id": "job-p4"}}
	if status, body := p4PostJSON(t, h, "/v1/tasks/"+strconv.FormatInt(id, 10)+"/transition", input); status != http.StatusOK {
		t.Fatalf("legacy decision transition status=%d body=%q", status, body)
	}
	return id
}

func p4Index(t *testing.T, body, kind string, id int64) int {
	t.Helper()
	pattern := regexp.MustCompile(`items\.([0-9]+)\.type" value="` + regexp.QuoteMeta(kind) + `"><input type="hidden" name="items\.[0-9]+\.id" value="` + strconv.FormatInt(id, 10) + `"`)
	match := pattern.FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("decision item %s/%d absent from body %q", kind, id, body)
	}
	index, err := strconv.Atoi(match[1])
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func p4Item(values url.Values, index int, kind string, id int64, answer string) {
	prefix := "items." + strconv.Itoa(index) + "."
	values.Set(prefix+"type", kind)
	values.Set(prefix+"id", strconv.FormatInt(id, 10))
	values.Set(prefix+"answer", answer)
}

func TestUIP4StructuredDecisionRendering(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "http://127.0.0.1:1", "fixture-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	recommended := p4DecisionTask(t, h, lane, "긴 한국어 결정 제목: 영속 저장 경로를 선택합니다", p4Options(true, false))
	plain := p4DecisionTask(t, h, lane, "권고 없는 결정", p4Options(false, true))

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%q", response.StatusCode, body)
	}
	for _, test := range []struct {
		id              int64
		wantChecked     int
		wantCustom      bool
		wantRecommended bool
	}{
		{recommended, 1, false, true},
		{plain, 0, true, false},
	} {
		start := strings.Index(body, "#"+strconv.FormatInt(test.id, 10))
		end := strings.Index(body[start:], "</article>")
		if start < 0 || end < 0 {
			t.Fatalf("card %d missing", test.id)
		}
		card := body[start : start+end]
		if got := strings.Count(card, "checked"); got != test.wantChecked {
			t.Fatalf("card %d checked=%d want=%d: %q", test.id, got, test.wantChecked, card)
		}
		if test.wantRecommended && !strings.Contains(card, "권고") {
			t.Fatalf("card %d lacks recommended badge: %q", test.id, card)
		}
		if got := strings.Contains(card, "items."+strconv.Itoa(p4Index(t, body, "task", test.id))+".custom"); got != test.wantCustom {
			t.Fatalf("card %d custom=%t want=%t: %q", test.id, got, test.wantCustom, card)
		}
	}
}

func TestUIP4BatchAnswersAndAudit(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	task := p4DecisionTask(t, h, lane, "일괄 답변 태스크", p4Options(true, true))
	escalation := seedRelay(t, s, lane, "job.escalate", uiLane(t, "batch-escalation"), "", "ESC worker-a(T16 §2,#75): held영속 Q1\n[options] A|노드 로컬 저장;B|중앙 저장 선행;rec=A", "")
	laneEvent := seedRelay(t, s, lane, "lane.event", "", "[decision-needed] lane answer", "", "")
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	values := url.Values{"csrf": {csrf}, "mode": {"selected"}}
	for _, item := range []struct {
		kind string
		id   int64
	}{
		{"task", task}, {"escalation", escalation.ID}, {"lane", laneEvent.ID},
	} {
		index := p4Index(t, body, item.kind, item.id)
		p4Item(values, index, item.kind, item.id, "A: 노드 로컬 저장")
		values.Set("items."+strconv.Itoa(index)+".select", "1")
	}
	var audit bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&audit)
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
	responseBody := responseText(t, response)
	log.SetOutput(oldOutput)
	if response.StatusCode != http.StatusOK || strings.Count(responseBody, `data-item-status="200"`) != 3 {
		t.Fatalf("batch status=%d body=%q", response.StatusCode, responseBody)
	}
	requests := hub.requests()
	if len(requests) != 3 {
		t.Fatalf("hub requests=%d", len(requests))
	}
	for _, request := range requests {
		if !strings.Contains(request.Text, "A: 노드 로컬 저장 (from operator(web) admin@example.com)") {
			t.Fatalf("unexpected answer text %q", request.Text)
		}
	}
	updated, found, err := s.GetTask(t.Context(), task)
	if err != nil || !found || updated.State != "claimed" {
		t.Fatalf("task after batch=%+v found=%t err=%v", updated, found, err)
	}
	lines := strings.Split(strings.TrimSpace(audit.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("audit lines=%q", audit.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, "action=decision-batch") || strings.Contains(line, "노드 로컬 저장") || strings.Contains(line, "hub-test-token") || strings.Contains(line, csrf) {
			t.Fatalf("unsafe batch audit=%q", line)
		}
	}
}

func TestUIP4BatchConflictAndRecommended(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	live := p4DecisionTask(t, h, lane, "live task", p4Options(true, true))
	closed := p4DecisionTask(t, h, lane, "closed task", p4Options(true, true))
	if status, body := p4PostJSON(t, h, "/v1/tasks/"+strconv.FormatInt(closed, 10)+"/transition", map[string]any{"to": "claimed", "note": "answered"}); status != http.StatusOK {
		t.Fatalf("close task status=%d body=%q", status, body)
	}
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	values := url.Values{"csrf": {csrf}, "mode": {"selected"}}
	p4Item(values, 0, "task", closed, "A: 노드 로컬 저장")
	values.Set("items.0.select", "1")
	p4Item(values, 1, "task", live, "A: 노드 로컬 저장")
	values.Set("items.1.select", "1")
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `data-item-status="409"`) || !strings.Contains(body, "이미 답변됨") || !strings.Contains(body, `data-item-status="200"`) {
		t.Fatalf("partial batch status=%d body=%q", response.StatusCode, body)
	}
	if got := len(hub.requests()); got != 1 {
		t.Fatalf("conflict reached hub: requests=%d", got)
	}

	withRecommendation := p4DecisionTask(t, h, lane, "recommended", p4Options(true, true))
	withoutRecommendation := p4DecisionTask(t, h, lane, "not recommended", p4Options(false, true))
	cookie, csrf = uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	values = url.Values{"csrf": {csrf}, "mode": {"recommended"}}
	p4Item(values, 0, "task", withRecommendation, "B: 중앙 저장 선행")
	p4Item(values, 1, "task", withoutRecommendation, "A: 노드 로컬 저장")
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
	body = responseText(t, response)
	if response.StatusCode != http.StatusOK || strings.Count(body, `data-item-status="200"`) != 1 {
		t.Fatalf("recommended response status=%d body=%q", response.StatusCode, body)
	}
	requests := hub.requests()
	if len(requests) != 2 || !strings.Contains(requests[1].Text, "A: 노드 로컬 저장") {
		t.Fatalf("recommended requests=%+v", requests)
	}
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
	s := uiStore(t)
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
	if updated.State != "claimed" {
		t.Fatalf("resolved task state=%s", updated.State)
	}
	before := uiRowCounts(t).Relay
	response, _ = p4Resolve(t, h, map[string]any{"type": "task", "id": task, "by": "lane-ops", "answer": "again"})
	if response.StatusCode != http.StatusConflict || uiRowCounts(t).Relay != before {
		t.Fatalf("already resolved status=%d relay before=%d after=%d", response.StatusCode, before, uiRowCounts(t).Relay)
	}
	response, _ = p4Resolve(t, h, map[string]any{"type": "task", "id": task + 1000000, "by": "bad lane", "answer": "x"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid by status=%d", response.StatusCode)
	}
	tooLong := p4DecisionTask(t, h, lane, "too long resolve", p4Options(true, true))
	before = uiRowCounts(t).Relay
	response, _ = p4Resolve(t, h, map[string]any{"type": "task", "id": tooLong, "by": "lane-ops", "answer": strings.Repeat("x", 2049)})
	if response.StatusCode != http.StatusBadRequest || uiRowCounts(t).Relay != before {
		t.Fatalf("oversized resolve status=%d relay before=%d after=%d", response.StatusCode, before, uiRowCounts(t).Relay)
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

func TestUIP4QueueOperatorView(t *testing.T) {
	s := uiStore(t)
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
	for _, title := range []string{"hidden implement backlog", "hidden fix backlog", "hidden ops backlog"} {
		if strings.Contains(body, title) {
			t.Fatalf("operator view rendered %q", title)
		}
	}
	if !strings.Contains(body, "shown needs decision") || !strings.Contains(body, "backlog (") {
		t.Fatalf("operator queue missing decision/count: %q", body)
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue?view=all", assertion, "")
	if body = responseText(t, response); !strings.Contains(body, "hidden implement backlog") || !strings.Contains(body, "hidden fix backlog") || !strings.Contains(body, "hidden ops backlog") {
		t.Fatalf("all view omitted backlog: %q", body)
	}
	before := uiRowCounts(t)
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/fragments/queue-backlog?lane="+lane, assertion, "")
	if body = responseText(t, response); response.StatusCode != http.StatusOK || !strings.Contains(body, "hidden implement backlog") || uiRowCounts(t) != before {
		t.Fatalf("backlog fragment status=%d body=%q before=%+v after=%+v", response.StatusCode, body, before, uiRowCounts(t))
	}
}

func TestUIP4BatchRejectsNoFreeCustom(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	task := p4DecisionTask(t, h, uiLane(t, "lane-a"), "closed free", p4Options(true, false))
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	values := url.Values{"csrf": {csrf}, "mode": {"selected"}}
	p4Item(values, 0, "task", task, "A: 노드 로컬 저장")
	values.Set("items.0.custom", "forbidden direct reply")
	values.Set("items.0.select", "1")
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `data-item-status="400"`) || !strings.Contains(body, "직접 답변은 허용되지 않습니다.") || len(hub.requests()) != 0 {
		t.Fatalf("no-free response status=%d body=%q hub=%d", response.StatusCode, body, len(hub.requests()))
	}
}

func TestUIP4BatchSelectionBoundsAndHubFailure(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, url.Values{"csrf": {csrf}, "mode": {"selected"}}, h.URL, false)
	if body := responseText(t, response); response.StatusCode != http.StatusOK || !strings.Contains(body, "선택된 항목이 없습니다.") || len(hub.requests()) != 0 {
		t.Fatalf("empty selection status=%d body=%q hub=%d", response.StatusCode, body, len(hub.requests()))
	}
	tooMany := url.Values{"csrf": {csrf}, "mode": {"selected"}}
	for index := 0; index < 51; index++ {
		p4Item(tooMany, index, "task", int64(index+1), "A: answer")
	}
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, tooMany, h.URL, false)
	if response.StatusCode != http.StatusBadRequest || len(hub.requests()) != 0 {
		t.Fatalf("over-limit status=%d hub=%d body=%q", response.StatusCode, len(hub.requests()), responseText(t, response))
	}
	response.Body.Close()

	task := p4DecisionTask(t, h, uiLane(t, "lane-a"), "hub failure task", p4Options(true, true))
	hub.mu.Lock()
	hub.mode = "failed"
	hub.mu.Unlock()
	values := url.Values{"csrf": {csrf}, "mode": {"selected"}}
	p4Item(values, 0, "task", task, "A: 노드 로컬 저장")
	values.Set("items.0.select", "1")
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `data-item-status="502"`) || !strings.Contains(body, "레인 전송 실패:") {
		t.Fatalf("hub failure status=%d body=%q", response.StatusCode, body)
	}
	updated, _, _ := s.GetTask(t.Context(), task)
	if updated.State != "needs_decision" {
		t.Fatalf("hub failure transitioned task=%s", updated.State)
	}
}

func TestUIP4BatchSecurityAndConsoleEscalationClosure(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	task := p4DecisionTask(t, h, uiLane(t, "lane-a"), "batch security", p4Options(true, true))
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	values := url.Values{"csrf": {csrf}, "mode": {"selected"}}
	p4Item(values, 0, "task", task, "A: 노드 로컬 저장")
	values.Set("items.0.select", "1")
	before := uiRowCounts(t)
	wrongCSRF := url.Values{}
	for key, value := range values {
		wrongCSRF[key] = append([]string(nil), value...)
	}
	wrongCSRF.Set("csrf", "wrong")
	for _, test := range []struct {
		name   string
		values url.Values
		origin string
	}{
		{"csrf", wrongCSRF, h.URL},
		{"origin", values, "http://elsewhere.invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, test.values, test.origin, false)
			if response.StatusCode != http.StatusForbidden {
				t.Fatalf("%s status=%d body=%q", test.name, response.StatusCode, responseText(t, response))
			}
			response.Body.Close()
		})
	}
	if len(hub.requests()) != 0 || uiRowCounts(t) != before {
		t.Fatalf("rejected batch changed state hub=%d before=%+v after=%+v", len(hub.requests()), before, uiRowCounts(t))
	}

	serviceHub := newFakeIngressHub(t)
	service := newP3UITestServer(t, s, fixture, serviceHub.server.URL, "hub-test-token", []string{"glance-fixture"})
	defer service.Close()
	serviceAssertion := p3ServiceAssertion(t, fixture)
	response := p3Request(t, service.Client(), http.MethodPost, service.URL+"/ui/decisions/answer-batch", serviceAssertion, []byte(values.Encode()), "application/x-www-form-urlencoded")
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("service batch status=%d body=%q", response.StatusCode, responseText(t, response))
	}
	response.Body.Close()
	for _, path := range []string{"/ui/decisions", "/ui/queue"} {
		response = p3Request(t, service.Client(), http.MethodGet, service.URL+path, serviceAssertion, nil, "")
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("service %s status=%d", path, response.StatusCode)
		}
		response.Body.Close()
	}
	response = p3Request(t, service.Client(), http.MethodGet, service.URL+"/ui/api/glance", serviceAssertion, nil, "")
	if response.StatusCode != http.StatusOK || len(serviceHub.requests()) != 0 {
		t.Fatalf("service glance status=%d requests=%d", response.StatusCode, len(serviceHub.requests()))
	}
	response.Body.Close()

	hub.mu.Lock()
	hub.persist = func(request ingressRequest) {
		status, body := p4PostJSON(t, h, "/v1/relay/events", map[string]any{"kind": "lane.event", "owner_lane": request.Lane, "event_id": request.EventID, "text": request.Text})
		if status != http.StatusCreated {
			t.Errorf("persist decision event status=%d body=%q", status, body)
		}
	}
	hub.mu.Unlock()
	escalationText := "P4 console escalation closure"
	escalation := seedRelay(t, s, uiLane(t, "lane-a"), "job.escalate", uiLane(t, "console-close"), "", escalationText, "")
	cookie, csrf = uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("escalation", escalation.ID, "continue", csrf), h.URL, true)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("console escalation status=%d body=%q", response.StatusCode, responseText(t, response))
	}
	response.Body.Close()
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	if strings.Contains(body, escalationText) {
		t.Fatalf("console answered escalation remained in inbox: %q", body)
	}
}

func TestUIP4LegacyDecisionControlsAndAnswer(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	legacy := p4LegacyDecisionTask(t, h, lane, "FT1 legacy options")

	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	index := p4Index(t, body, "task", legacy)
	start := strings.Index(body, "#"+strconv.FormatInt(legacy, 10))
	end := strings.Index(body[start:], "</article>")
	if start < 0 || end < 0 {
		t.Fatalf("legacy card absent: %q", body)
	}
	card := body[start : start+end]
	for _, answer := range []string{"승인", "반려"} {
		if strings.Count(card, `name="items.`+strconv.Itoa(index)+`.answer" value="`+answer+`"`) != 1 {
			t.Fatalf("legacy answer %q missing from card: %q", answer, card)
		}
	}
	if !strings.Contains(card, `name="items.`+strconv.Itoa(index)+`.select"`) || !strings.Contains(card, `name="items.`+strconv.Itoa(index)+`.note"`) || !strings.Contains(card, `name="only" value="`+strconv.Itoa(index)+`"`) || strings.Contains(card, `items.`+strconv.Itoa(index)+`.custom`) {
		t.Fatalf("legacy controls do not follow batch contract: %q", card)
	}
	if regexp.MustCompile(`<[^>]+\sname="answer"`).MatchString(body) {
		t.Fatalf("legacy controls retained an unnamespaced answer element: %q", body)
	}

	values := url.Values{"csrf": {csrf}, "only": {strconv.Itoa(index)}}
	p4Item(values, index, "task", legacy, "승인")
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
	responseBody := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(responseBody, `data-item-status="200"`) {
		t.Fatalf("legacy answer status=%d body=%q", response.StatusCode, responseBody)
	}
	requests := hub.requests()
	if len(requests) != 1 || requests[0].Text != "[decision] #"+strconv.FormatInt(legacy, 10)+": 승인 (from operator(web) admin@example.com)" {
		t.Fatalf("legacy hub requests=%+v", requests)
	}
	updated, found, err := s.GetTask(t.Context(), legacy)
	if err != nil || !found || updated.State != "claimed" {
		t.Fatalf("legacy task=%+v found=%t err=%v", updated, found, err)
	}
}

func TestUIP4LegacyDecisionOnlyAndWhitelist(t *testing.T) {
	t.Run("only", func(t *testing.T) {
		s := uiStore(t)
		fixture := newUIJWTFixture(t)
		hub := newFakeIngressHub(t)
		h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
		defer h.Close()
		assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
		lane := uiLane(t, "lane-a")
		legacy := p4LegacyDecisionTask(t, h, lane, "FT3 legacy only")
		other := p4DecisionTask(t, h, lane, "FT3 other decision", p4Options(true, true))
		cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
		body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
		index := p4Index(t, body, "task", legacy)
		values := url.Values{"csrf": {csrf}, "only": {strconv.Itoa(index)}}
		p4Item(values, index, "task", legacy, "승인")
		response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
		if response.StatusCode != http.StatusOK || !strings.Contains(responseText(t, response), `data-item-status="200"`) {
			t.Fatalf("legacy only status=%d", response.StatusCode)
		}
		updated, found, err := s.GetTask(t.Context(), other)
		if err != nil || !found || updated.State != "needs_decision" {
			t.Fatalf("only submission changed other task=%+v found=%t err=%v", updated, found, err)
		}
		requests := hub.requests()
		if len(requests) != 1 || strings.Contains(requests[0].Text, "#"+strconv.FormatInt(other, 10)+":") {
			t.Fatalf("only submission sent another decision: %+v", requests)
		}
	})

	t.Run("whitelist", func(t *testing.T) {
		s := uiStore(t)
		fixture := newUIJWTFixture(t)
		hub := newFakeIngressHub(t)
		h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
		defer h.Close()
		assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
		legacy := p4LegacyDecisionTask(t, h, uiLane(t, "lane-a"), "FT4 legacy whitelist")
		cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
		body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
		index := p4Index(t, body, "task", legacy)
		values := url.Values{"csrf": {csrf}, "only": {strconv.Itoa(index)}}
		p4Item(values, index, "task", legacy, "마음대로")
		response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
		responseBody := responseText(t, response)
		if response.StatusCode != http.StatusOK || !strings.Contains(responseBody, `data-item-status="400"`) || len(hub.requests()) != 0 {
			t.Fatalf("whitelist status=%d body=%q hub=%d", response.StatusCode, responseBody, len(hub.requests()))
		}
		updated, found, err := s.GetTask(t.Context(), legacy)
		if err != nil || !found || updated.State != "needs_decision" {
			t.Fatalf("whitelist changed task=%+v found=%t err=%v", updated, found, err)
		}
	})
}

func TestUIP4DecisionInboxDoesNotHideEventKindsAfterFiftyTasks(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	for index := 0; index < 51; index++ {
		task := createUITask(t, s, lane, "FT5 task decision "+strconv.Itoa(index))
		claimAndTransition(t, s, task, "needs_decision", "FT5 task question")
	}
	escalationQuestion := "FT5 escalation remains visible"
	seedRelay(t, s, lane, "job.escalate", uiLane(t, "ft5-escalation"), "", escalationQuestion, "")
	laneText := "[decision-needed] FT5 lane decision remains visible"
	seedRelay(t, s, lane, "lane.event", "", laneText, "", "")

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, escalationQuestion) || !strings.Contains(body, "FT5 lane decision remains visible") {
		t.Fatalf("decision kinds hidden after fifty tasks status=%d body=%q", response.StatusCode, body)
	}
}

func TestUIP4QueueOperatorViewExcludesInProgressImplement(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	implement := createUITask(t, s, lane, "FT6 in-progress implement")
	claimAndTransition(t, s, implement, "in_progress", "working")
	decision := createUITask(t, s, lane, "FT6 needs decision")
	claimAndTransition(t, s, decision, "needs_decision", "choose")

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue", assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || strings.Contains(body, implement.Title) || !strings.Contains(body, decision.Title) {
		t.Fatalf("operator queue status=%d body=%q", response.StatusCode, body)
	}
}
