package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/store"
	"golang.org/x/net/html"
)

// The batch parser's unexported product constant is intentionally asserted at
// its public behavior boundary here: UI rendering must cap at the same 1000
// cards accepted by one default-toolchain form request.
const p4DecisionBatchRenderLimit = 1000

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

type renderedDecisionControl struct {
	name      string
	value     string
	kind      string
	checked   bool
	disabled  bool
	submitter bool
}

func p4HTMLAttribute(node *html.Node, name string) (string, bool) {
	for _, attribute := range node.Attr {
		if strings.EqualFold(attribute.Key, name) {
			return attribute.Val, true
		}
	}
	return "", false
}

func p4DecisionFormControls(t *testing.T, body string) []renderedDecisionControl {
	t.Helper()
	root, err := html.Parse(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var form *html.Node
	var findForm func(*html.Node)
	findForm = func(node *html.Node) {
		if form != nil {
			return
		}
		if node.Type == html.ElementNode && node.Data == "form" {
			if action, ok := p4HTMLAttribute(node, "action"); ok && action == "/ui/decisions/answer-batch" {
				form = node
				return
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			findForm(child)
		}
	}
	findForm(root)
	if form == nil {
		t.Fatalf("rendered decision form absent from body: %q", body)
	}
	controls := []renderedDecisionControl{}
	var walk func(*html.Node, bool)
	walk = func(node *html.Node, fieldsetDisabled bool) {
		if node.Type != html.ElementNode {
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				walk(child, fieldsetDisabled)
			}
			return
		}
		disabled := fieldsetDisabled
		if _, ok := p4HTMLAttribute(node, "disabled"); ok {
			disabled = true
		}
		if node.Data == "input" || node.Data == "button" {
			name, named := p4HTMLAttribute(node, "name")
			if named && name != "" {
				kind := "submit"
				if node.Data == "input" {
					kind, _ = p4HTMLAttribute(node, "type")
					kind = strings.ToLower(kind)
					if kind == "" {
						kind = "text"
					}
				}
				value, hasValue := p4HTMLAttribute(node, "value")
				if !hasValue && (kind == "checkbox" || kind == "radio") {
					value = "on"
				}
				_, checked := p4HTMLAttribute(node, "checked")
				controls = append(controls, renderedDecisionControl{
					name: name, value: value, kind: kind, checked: checked, disabled: disabled,
					submitter: node.Data == "button" || kind == "submit" || kind == "image",
				})
			}
		}
		childDisabled := disabled
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, childDisabled)
		}
	}
	for child := form.FirstChild; child != nil; child = child.NextSibling {
		walk(child, false)
	}
	return controls
}

func p4RenderedDecisionIndexes(t *testing.T, controls []renderedDecisionControl) []int {
	t.Helper()
	seen := map[int]map[string]bool{}
	for _, control := range controls {
		match := regexp.MustCompile(`^items\.([0-9]+)\.(type|id)$`).FindStringSubmatch(control.name)
		if match == nil || control.disabled {
			continue
		}
		index, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatal(err)
		}
		if seen[index] == nil {
			seen[index] = map[string]bool{}
		}
		if seen[index][match[2]] {
			t.Fatalf("duplicate rendered decision field items.%d.%s", index, match[2])
		}
		seen[index][match[2]] = true
	}
	if len(seen) == 0 {
		t.Fatal("rendered decision form contained no enabled item hidden fields")
	}
	indexes := make([]int, 0, len(seen))
	for index, fields := range seen {
		if !fields["type"] || !fields["id"] {
			t.Fatalf("rendered decision form omitted hidden fields for items.%d: %+v", index, fields)
		}
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}

// p4SerializedDecisionForm mirrors browser successful-controls serialization
// over the rendered decision form. extraFields models user edits and exactly
// one clicked submitter; disabled controls and unselected radios/checkboxes
// remain absent.
func p4SerializedDecisionForm(t *testing.T, body string, extraFields url.Values) (url.Values, []int) {
	t.Helper()
	controls := p4DecisionFormControls(t, body)
	indexes := p4RenderedDecisionIndexes(t, controls)
	values := url.Values{}
	byName := map[string][]renderedDecisionControl{}
	for _, control := range controls {
		if control.disabled {
			continue
		}
		byName[control.name] = append(byName[control.name], control)
		if control.submitter || ((control.kind == "checkbox" || control.kind == "radio") && !control.checked) {
			continue
		}
		values.Add(control.name, control.value)
	}
	for name, fieldValues := range extraFields {
		candidates := byName[name]
		if len(candidates) == 0 {
			t.Fatalf("extra field %q is not an enabled rendered control", name)
		}
		isSubmitter := name == "only" || name == "mode"
		values.Del(name)
		for _, fieldValue := range fieldValues {
			matched := false
			for _, candidate := range candidates {
				if isSubmitter {
					matched = candidate.submitter && candidate.value == fieldValue
				} else if candidate.kind == "checkbox" || candidate.kind == "radio" {
					matched = candidate.value == fieldValue
				} else {
					matched = !candidate.submitter
				}
				if matched {
					break
				}
			}
			if !matched {
				t.Fatalf("extra field %s=%q is not a successful rendered control", name, fieldValue)
			}
			values.Add(name, fieldValue)
		}
	}
	return values, indexes
}

func submitRenderedDecisionForm(t *testing.T, h *httptest.Server, assertion string, cookie *http.Cookie, body string, extraFields url.Values) (*http.Response, int) {
	t.Helper()
	values, indexes := p4SerializedDecisionForm(t, body, extraFields)
	return uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false), len(indexes)
}

func p4RecommendedDecisionTasks(t *testing.T, h *httptest.Server, lane, prefix string, count int) []int64 {
	t.Helper()
	tasks := make([]int64, 0, count)
	for index := 0; index < count; index++ {
		tasks = append(tasks, p4DecisionTask(t, h, lane, prefix+" "+strconv.Itoa(index), p4Options(true, false)))
	}
	return tasks
}

func p4StructuredDecisionOptions() store.DecisionOptions {
	return store.DecisionOptions{Options: []store.DecisionOption{
		{Key: "A", Label: "노드 로컬 저장", Recommended: true},
		{Key: "B", Label: "중앙 저장 선행"},
	}, AllowFree: true}
}

func p4StructuredDecisionQuestion() string {
	options := p4StructuredDecisionOptions()
	return "어떤 저장소를 선택할까요?\n" + store.FormatDecisionOptions(options)
}

// p4StructuredDecisionTasks avoids API setup cost in the maximum-backlog
// cases while preserving the same task and structured-option state the UI
// renders. Cleanup removes every still-open card and terminalizes already
// answered work. It also moves just these test rows behind ordinary work:
// ListTasks applies its 1000-row limit before a caller can ignore terminal
// states, so otherwise a heavy test would hide later tests' own task rows.
func p4StructuredDecisionTasks(t *testing.T, _ *store.Store, lane, prefix string, count int) []int64 {
	t.Helper()
	options := p4StructuredDecisionOptions()
	refs, err := json.Marshal(store.TaskRefs{JobID: "job-p4", DecisionOptions: &options})
	if err != nil {
		t.Fatal(err)
	}
	titles := make([]string, 0, count)
	for index := 0; index < count; index++ {
		titles = append(titles, prefix+" "+strconv.Itoa(index))
	}
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	tx, err := db.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	now := time.Now().UTC()
	rows, err := tx.Query(t.Context(), `INSERT INTO tasks(lane,title,kind,state,refs,claimed_by,created_by,created_at,updated_at)
		SELECT $1,title,'implement','needs_decision',$2::jsonb,'worker-a','worker-a',$3,$3
		FROM unnest($4::text[]) AS title RETURNING id`, lane, string(refs), now, titles)
	if err != nil {
		t.Fatal(err)
	}
	tasks := make([]int64, 0, count)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tasks = append(tasks, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if _, err := tx.Exec(t.Context(), `INSERT INTO task_events(task_id,"from","to","by",note,refs,at)
		SELECT id,'claimed','needs_decision','worker-a',$1,$2::jsonb,$3 FROM tasks WHERE id = ANY($4)`, "어떤 저장소를 선택할까요?", string(refs), now, tasks); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db, err := pgx.Connect(context.Background(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
		if err != nil {
			t.Logf("decision task cleanup connection: %v", err)
			return
		}
		defer db.Close(context.Background())
		if _, err := db.Exec(context.Background(), `UPDATE tasks SET state='dropped', priority=-2147483648 WHERE id = ANY($1)`, tasks); err != nil {
			t.Logf("decision task cleanup: %v", err)
		}
	})
	return tasks
}

type p4OpenDecisionCounts struct {
	Tasks       int
	Escalations int
	Lanes       int
}

func (counts p4OpenDecisionCounts) total() int {
	return counts.Tasks + counts.Escalations + counts.Lanes
}

type p4OwnedDecisionCounter struct {
	db            *pgx.Conn
	taskIDs       []int64
	escalationIDs []int64
	laneIDs       []int64
}

func newP4OwnedDecisionCounter(t *testing.T, taskIDs, escalationIDs, laneIDs []int64) p4OwnedDecisionCounter {
	t.Helper()
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(context.Background()) })
	return p4OwnedDecisionCounter{db: db, taskIDs: taskIDs, escalationIDs: escalationIDs, laneIDs: laneIDs}
}

func (counter p4OwnedDecisionCounter) counts(t *testing.T) p4OpenDecisionCounts {
	t.Helper()
	counts := p4OpenDecisionCounts{}
	err := counter.db.QueryRow(t.Context(), `SELECT
		(SELECT COUNT(*) FROM tasks WHERE id = ANY($1) AND state='needs_decision'),
		(SELECT COUNT(*) FROM relay_events e WHERE e.id = ANY($2) AND e.kind='job.escalate' AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.job_id=e.job_id
			AND resolved.kind IN ('job.joined','job.completed') AND resolved.id>e.id
		) AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.kind='lane.event'
			AND resolved.owner_lane=e.owner_lane AND resolved.id>e.id
			AND resolved.event_id LIKE '%decision-escalation-' || e.id::text || '-%'
		)),
		(SELECT COUNT(*) FROM relay_events e WHERE e.id = ANY($3) AND e.kind='lane.event'
		AND e.text LIKE '[decision-needed]%' AND NOT EXISTS (
			SELECT 1 FROM relay_events resolved WHERE resolved.kind='lane.event'
			AND resolved.owner_lane=e.owner_lane AND resolved.text LIKE '[decision-answered]%' AND resolved.id>e.id
		))`, counter.taskIDs, counter.escalationIDs, counter.laneIDs).Scan(&counts.Tasks, &counts.Escalations, &counts.Lanes)
	if err != nil {
		t.Fatal(err)
	}
	return counts
}

func p4TasksByRenderedIndex(t *testing.T, body string, tasks []int64) []int64 {
	t.Helper()
	ordered := append([]int64(nil), tasks...)
	sort.Slice(ordered, func(left, right int) bool {
		return p4Index(t, body, "task", ordered[left]) < p4Index(t, body, "task", ordered[right])
	})
	return ordered
}

func p4DecideTask(t *testing.T, s *store.Store, lane, title, state string) store.Task {
	t.Helper()
	task, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: title, Kind: "decide", CreatedBy: "node"})
	if err != nil {
		t.Fatal(err)
	}
	if state == "backlog" {
		return task
	}
	task, err = s.ClaimTask(t.Context(), task.ID, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if state == "claimed" {
		return task
	}
	if state == "merged" {
		for _, next := range []string{"in_progress", "verifying"} {
			task, err = s.TransitionTask(t.Context(), task.ID, next, "node", "p4 decide state", nil)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	task, err = s.TransitionTask(t.Context(), task.ID, state, "node", "p4 decide state", nil)
	if err != nil {
		t.Fatal(err)
	}
	return task
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
	for index := 0; index < 1001; index++ {
		p4Item(tooMany, index, "task", int64(index+1), "A: answer")
	}
	// Over the parse limit the request is truncated and reported, not rejected.
	// Rejecting locked the console shut once the backlog passed the limit, with
	// no in-console way back under it.
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, tooMany, h.URL, false)
	overBody := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(overBody, "절단됨 1000/1001") || len(hub.requests()) != 0 {
		t.Fatalf("over-limit status=%d hub=%d body=%q", response.StatusCode, len(hub.requests()), overBody)
	}

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
		otherIndex := p4Index(t, body, "task", other)
		values := url.Values{"csrf": {csrf}, "only": {strconv.Itoa(index)}}
		p4Item(values, index, "task", legacy, "승인")
		p4Item(values, otherIndex, "task", other, "A: 노드 로컬 저장")
		values.Set("items."+strconv.Itoa(otherIndex)+".select", "1")
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

func TestUIP4RenderedDecisionFormOnlyOverFifty(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	tasks := p4RecommendedDecisionTasks(t, h, uiLane(t, "lane-a"), "GT1 rendered form", 56)
	target := tasks[len(tasks)-1]
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	index := p4Index(t, body, "task", target)
	extra := url.Values{"csrf": {csrf}, "only": {strconv.Itoa(index)}}
	extra.Set("items."+strconv.Itoa(index)+".answer", "A: 노드 로컬 저장")
	response, rendered := submitRenderedDecisionForm(t, h, assertion, cookie, body, extra)
	responseBody := responseText(t, response)
	if rendered < len(tasks) || response.StatusCode != http.StatusOK || strings.Count(responseBody, `data-item-status="200"`) != 1 || len(hub.requests()) != 1 {
		t.Fatalf("GT1 rendered=%d status=%d results=%d hub=%d body=%q", rendered, response.StatusCode, strings.Count(responseBody, `data-item-status="200"`), len(hub.requests()), responseBody)
	}
	for _, id := range tasks {
		updated, found, err := s.GetTask(t.Context(), id)
		want := "needs_decision"
		if id == target {
			want = "claimed"
		}
		if err != nil || !found || updated.State != want {
			t.Fatalf("GT1 task=%d state=%q want=%q found=%t err=%v", id, updated.State, want, found, err)
		}
	}
}

func TestUIP4RenderedDecisionFormOnlyThreshold(t *testing.T) {
	for _, count := range []int{50, 51} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			s := uiStore(t)
			fixture := newUIJWTFixture(t)
			hub := newFakeIngressHub(t)
			h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
			defer h.Close()
			assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
			tasks := p4RecommendedDecisionTasks(t, h, uiLane(t, "lane-a"), "GT2 rendered form", count)
			target := tasks[len(tasks)-1]
			cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
			body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
			index := p4Index(t, body, "task", target)
			extra := url.Values{"csrf": {csrf}, "only": {strconv.Itoa(index)}}
			extra.Set("items."+strconv.Itoa(index)+".answer", "A: 노드 로컬 저장")
			response, rendered := submitRenderedDecisionForm(t, h, assertion, cookie, body, extra)
			responseBody := responseText(t, response)
			updated, found, err := s.GetTask(t.Context(), target)
			if rendered < count || response.StatusCode != http.StatusOK || strings.Count(responseBody, `data-item-status="200"`) != 1 || len(hub.requests()) != 1 || err != nil || !found || updated.State != "claimed" {
				t.Fatalf("GT2 count=%d rendered=%d status=%d results=%d hub=%d task=%+v found=%t err=%v body=%q", count, rendered, response.StatusCode, strings.Count(responseBody, `data-item-status="200"`), len(hub.requests()), updated, found, err, responseBody)
			}
		})
	}
}

func TestUIP4SelectedDecisionBatchAdvancesOverFifty(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	tasks := p4RecommendedDecisionTasks(t, h, uiLane(t, "lane-a"), "GT3 selected", 51)
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	ordered := p4TasksByRenderedIndex(t, body, tasks)
	extra := url.Values{"csrf": {csrf}, "mode": {"selected"}}
	for _, id := range tasks {
		index := p4Index(t, body, "task", id)
		extra.Set("items."+strconv.Itoa(index)+".select", "1")
		extra.Set("items."+strconv.Itoa(index)+".answer", "A: 노드 로컬 저장")
	}
	response, rendered := submitRenderedDecisionForm(t, h, assertion, cookie, body, extra)
	responseBody := responseText(t, response)
	if rendered < len(tasks) || response.StatusCode != http.StatusOK || strings.Count(responseBody, `data-item-status="200"`) != 50 || !strings.Contains(responseBody, "1건이 남았습니다. 다시 제출하면 이어서 처리됩니다.") || len(hub.requests()) != 50 {
		t.Fatalf("GT3 first submit rendered=%d status=%d results=%d hub=%d body=%q", rendered, response.StatusCode, strings.Count(responseBody, `data-item-status="200"`), len(hub.requests()), responseBody)
	}
	for position, id := range ordered {
		updated, found, err := s.GetTask(t.Context(), id)
		want := "needs_decision"
		if position < 50 {
			want = "claimed"
		}
		if err != nil || !found || updated.State != want {
			t.Fatalf("GT3 first submit task=%d position=%d state=%q want=%q found=%t err=%v", id, position, updated.State, want, found, err)
		}
	}

	remaining := ordered[50]
	extra = url.Values{"csrf": {csrf}, "mode": {"selected"}}
	remainingIndex := p4Index(t, responseBody, "task", remaining)
	extra.Set("items."+strconv.Itoa(remainingIndex)+".select", "1")
	extra.Set("items."+strconv.Itoa(remainingIndex)+".answer", "A: 노드 로컬 저장")
	response, _ = submitRenderedDecisionForm(t, h, assertion, cookie, responseBody, extra)
	responseBody = responseText(t, response)
	updated, found, err := s.GetTask(t.Context(), remaining)
	if response.StatusCode != http.StatusOK || strings.Count(responseBody, `data-item-status="200"`) != 1 || len(hub.requests()) != 51 || err != nil || !found || updated.State != "claimed" {
		t.Fatalf("GT3 retry status=%d results=%d hub=%d task=%+v found=%t err=%v body=%q", response.StatusCode, strings.Count(responseBody, `data-item-status="200"`), len(hub.requests()), updated, found, err, responseBody)
	}
}

func TestUIP4RecommendedDecisionBatchAdvancesOverFifty(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	tasks := p4RecommendedDecisionTasks(t, h, uiLane(t, "lane-a"), "GT4 recommended", 51)
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	response, rendered := submitRenderedDecisionForm(t, h, assertion, cookie, body, url.Values{"csrf": {csrf}, "mode": {"recommended"}})
	responseBody := responseText(t, response)
	if rendered < len(tasks) || response.StatusCode != http.StatusOK || strings.Count(responseBody, `data-item-status="200"`) != 50 || !regexp.MustCompile(`[1-9][0-9]*건이 남았습니다\. 다시 제출하면 이어서 처리됩니다\.`).MatchString(responseBody) || len(hub.requests()) != 50 {
		t.Fatalf("GT4 first submit rendered=%d status=%d results=%d hub=%d body=%q", rendered, response.StatusCode, strings.Count(responseBody, `data-item-status="200"`), len(hub.requests()), responseBody)
	}
	for attempt := 0; attempt < 10; attempt++ {
		allClaimed := true
		for _, id := range tasks {
			updated, found, err := s.GetTask(t.Context(), id)
			if err != nil || !found {
				t.Fatalf("GT4 task=%d found=%t err=%v", id, found, err)
			}
			if updated.State != "claimed" {
				allClaimed = false
				break
			}
		}
		if allClaimed {
			break
		}
		response, _ = submitRenderedDecisionForm(t, h, assertion, cookie, responseBody, url.Values{"csrf": {csrf}, "mode": {"recommended"}})
		responseBody = responseText(t, response)
		if response.StatusCode != http.StatusOK || strings.Count(responseBody, `data-item-status="200"`) == 0 {
			t.Fatalf("GT4 retry status=%d results=%d body=%q", response.StatusCode, strings.Count(responseBody, `data-item-status="200"`), responseBody)
		}
	}
	for _, id := range tasks {
		updated, found, err := s.GetTask(t.Context(), id)
		if err != nil || !found || updated.State != "claimed" {
			t.Fatalf("GT4 task=%d state=%q found=%t err=%v", id, updated.State, found, err)
		}
	}
}

func TestUIP4QueueOperatorViewIncludesNonterminalDecide(t *testing.T) {
	s := uiStore(t)
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

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue", assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, inProgress.Title) || !strings.Contains(body, claimed.Title) {
		t.Fatalf("SHOULD-3 operator queue omitted nonterminal decide task: %q", body)
	}
	for _, task := range []store.Task{merged, dropped, backlog} {
		if strings.Contains(body, task.Title) {
			t.Fatalf("SHOULD-3 operator queue rendered terminal/backlog decide task %q: %q", task.Title, body)
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
	// every later test's decision page and slow the suite to a timeout. A
	// single answered event on this lane closes all of them, which is the same
	// rule the console itself uses. t.Context is already cancelled by the time
	// cleanup runs, so this uses its own context.
	t.Cleanup(func() {
		if _, _, err := s.AppendRelayEvent(context.Background(), store.RelayEvent{
			Kind: "lane.event", JobID: "ht-cleanup", OwnerLane: lane,
			Text: "[decision-answered] HT cleanup", EventID: "ht-cleanup-" + lane,
		}); err != nil {
			t.Logf("lane decision cleanup: %v", err)
		}
	})
	return ids
}

// p4Escalations seeds open escalations under one job id so a single completed
// event closes the whole batch on cleanup.
func p4Escalations(t *testing.T, s *store.Store, lane string, count int, question ...string) []int64 {
	t.Helper()
	jobID := "ht-esc-" + lane
	ids := make([]int64, 0, count)
	for index := 0; index < count; index++ {
		text := "HT escalation " + strconv.Itoa(index)
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

// HT1 is now an HTTP-defense test. The render contract intentionally prevents
// UI-generated forms from exceeding 1000 cards, but a direct oversized POST
// must still truncate rather than lock the operator out.
func TestUIP4OversizedFormTruncatesInsteadOfLocking(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	values := url.Values{"csrf": {csrf}, "mode": {"selected"}}
	for index := 0; index < p4DecisionBatchRenderLimit+1; index++ {
		p4Item(values, index, "task", int64(index+1), "A: 노드 로컬 저장")
	}
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
	responseBody := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(responseBody, "절단됨 1000/1001") || len(hub.requests()) != 0 {
		t.Fatalf("HT1 direct oversized POST status=%d hub=%d body=%q", response.StatusCode, len(hub.requests()), responseBody)
	}
}

func p4RequireFullSuccessfulControls(t *testing.T, values url.Values, indexes []int) {
	t.Helper()
	for _, index := range indexes {
		for _, suffix := range []string{"type", "id", "select", "answer", "custom", "note"} {
			key := "items." + strconv.Itoa(index) + "." + suffix
			if got := values[key]; len(got) != 1 {
				t.Fatalf("successful control %s values=%q", key, got)
			}
		}
		for _, suffix := range []string{"custom", "note"} {
			key := "items." + strconv.Itoa(index) + "." + suffix
			if values.Get(key) != "" {
				t.Fatalf("empty successful control %s=%q", key, values.Get(key))
			}
		}
	}
}

func p4VisibleOwnedIndexes(t *testing.T, body, kind string, ids []int64) []int {
	t.Helper()
	wanted := make(map[int64]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}
	values, indexes := p4SerializedDecisionForm(t, body, nil)
	visible := make([]int, 0)
	for _, index := range indexes {
		prefix := "items." + strconv.Itoa(index) + "."
		if values.Get(prefix+"type") != kind {
			continue
		}
		id, err := strconv.ParseInt(values.Get(prefix+"id"), 10, 64)
		if err != nil {
			t.Fatalf("rendered %s item %d id=%q: %v", kind, index, values.Get(prefix+"id"), err)
		}
		if wanted[id] {
			visible = append(visible, index)
		}
	}
	return visible
}

// AC3: inspect the actual rendered DOM, including empty text controls and the
// clicked submitter. This fails if the helper regresses to hidden fields only.
func TestUIP4RenderedDecisionFormSuccessfulControls(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	task := p4StructuredDecisionTasks(t, s, uiLane(t, "lane-a"), "AC3", 1)[0]
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	index := p4Index(t, body, "task", task)
	values, indexes := p4SerializedDecisionForm(t, body, url.Values{
		"csrf": {csrf},
		"only": {strconv.Itoa(index)},
		"items." + strconv.Itoa(index) + ".select": {"1"},
	})
	if len(indexes) == 0 || values.Get("csrf") != csrf || values.Get("only") != strconv.Itoa(index) || values.Get("mode") != "" {
		t.Fatalf("AC3 submitter/fixed controls indexes=%v csrf=%q only=%q mode=%q", indexes, values.Get("csrf"), values.Get("only"), values.Get("mode"))
	}
	p4RequireFullSuccessfulControls(t, values, []int{index})
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
	responseBody := responseText(t, response)
	updated, found, err := s.GetTask(t.Context(), task)
	if response.StatusCode != http.StatusOK || strings.Count(responseBody, `data-item-status="200"`) != 1 || len(hub.requests()) != 1 || err != nil || !found || updated.State != "claimed" {
		t.Fatalf("AC3 status=%d results=%d hub=%d task=%+v found=%t err=%v body=%q", response.StatusCode, strings.Count(responseBody, `data-item-status="200"`), len(hub.requests()), updated, found, err, responseBody)
	}
	if values.Get("items."+strconv.Itoa(index)+".answer") != "A: 노드 로컬 저장" || values.Get("items."+strconv.Itoa(index)+".select") != "1" {
		t.Fatalf("AC3 checked controls answer=%q select=%q", values.Get("items."+strconv.Itoa(index)+".answer"), values.Get("items."+strconv.Itoa(index)+".select"))
	}
}

// AC4: maximum rendered cards use the real template's successful controls,
// not a hand-counted per-card estimate. Each normal submit mode stays beneath
// Go's default URL parameter cap and reaches the real handler.
func TestUIP4RenderedDecisionFormParameterBudget(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	p4StructuredDecisionTasks(t, s, uiLane(t, "lane-a"), "AC4", p4DecisionBatchRenderLimit)
	p4Escalations(t, s, uiLane(t, "lane-e"), p4DecisionBatchRenderLimit, p4StructuredDecisionQuestion())
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	controls := p4DecisionFormControls(t, body)
	indexes := p4RenderedDecisionIndexes(t, controls)
	if len(indexes) != p4DecisionBatchRenderLimit {
		t.Fatalf("AC4 rendered cards=%d want=%d", len(indexes), p4DecisionBatchRenderLimit)
	}
	for _, test := range []struct {
		name  string
		field string
		value string
	}{
		{"only", "only", strconv.Itoa(indexes[len(indexes)-1])},
		{"selected", "mode", "selected"},
		{"recommended", "mode", "recommended"},
	} {
		t.Run(test.name, func(t *testing.T) {
			extra := url.Values{"csrf": {csrf}, test.field: {test.value}}
			for _, index := range indexes {
				extra.Set("items."+strconv.Itoa(index)+".select", "1")
			}
			values, gotIndexes := p4SerializedDecisionForm(t, body, extra)
			if !reflect.DeepEqual(gotIndexes, indexes) {
				t.Fatalf("AC4 rendered indexes=%v want=%v", gotIndexes, indexes)
			}
			p4RequireFullSuccessfulControls(t, values, gotIndexes)
			parameterCount := 0
			for _, fieldValues := range values {
				parameterCount += len(fieldValues)
			}
			if parameterCount >= 10000 {
				t.Fatalf("AC4 %s successful parameters=%d, exceeds default parser budget", test.name, parameterCount)
			}
			response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
			responseBody := responseText(t, response)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("AC4 %s parameters=%d status=%d body=%q", test.name, parameterCount, response.StatusCode, responseBody)
			}
			t.Logf("AC4 %s: rendered=%d successful parameters=%d handler=200", test.name, len(gotIndexes), parameterCount)
		})
	}
}

// AC1/AC2: 1000 cards remain a complete render; 1001 cards retain the same
// cap and say only what the three capped retrieval queries establish. Signals
// are informational and must not consume the form-card budget.
func TestUIP4DecisionRenderLimitAndNotice(t *testing.T) {
	for _, test := range []struct {
		name       string
		cards      int
		wantNotice bool
	}{
		{"1000", 1000, false},
		{"1001", 1001, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := uiStore(t)
			fixture := newUIJWTFixture(t)
			hub := newFakeIngressHub(t)
			h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
			defer h.Close()
			assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
			laneCards := test.cards
			var taskIDs []int64
			if !test.wantNotice {
				laneCards--
				taskIDs = p4StructuredDecisionTasks(t, s, uiLane(t, "lane-t"), "AC1", 1)
			}
			laneIDs := p4LaneDecisions(t, s, uiLane(t, "lane-a"), laneCards)
			if !test.wantNotice {
				jobID := uiLane(t, "signal")
				seedRelay(t, s, uiLane(t, "lane-s"), "job.escalate", jobID, "", "PING", "")
				t.Cleanup(func() {
					if _, _, err := s.AppendRelayEvent(context.Background(), store.RelayEvent{Kind: "job.completed", JobID: jobID, OwnerLane: "lane-a", ReportPath: "cleanup.md", Reason: "cleanup"}); err != nil {
						t.Logf("signal cleanup: %v", err)
					}
				})
			}
			body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
			_, indexes := p4SerializedDecisionForm(t, body, nil)
			if len(indexes) != p4DecisionBatchRenderLimit {
				t.Fatalf("AC1 backlog=%d rendered=%d want=%d", test.cards, len(indexes), p4DecisionBatchRenderLimit)
			}
			hasNotice := strings.Contains(body, "data-render-notice")
			if hasNotice && (!strings.Contains(body, "현재 조회된") || !strings.Contains(body, "표시") || strings.Contains(body, "전체")) {
				t.Fatalf("AC2 backlog=%d dishonest render notice: %q", test.cards, body)
			}
			if test.wantNotice && !hasNotice {
				t.Fatalf("AC2 backlog=%d omitted source-limit notice: %q", test.cards, body)
			}
			if !test.wantNotice {
				ownedVisible := len(p4VisibleOwnedIndexes(t, body, "task", taskIDs)) + len(p4VisibleOwnedIndexes(t, body, "lane", laneIDs))
				if ownedVisible == p4DecisionBatchRenderLimit && hasNotice {
					t.Fatalf("AC2 exact 1000-card owned render unexpectedly showed a notice: %q", body)
				}
			}
			if !test.wantNotice && !strings.Contains(body, "PING") {
				t.Fatalf("AC1 signal was not rendered separately: %q", body)
			}
		})
	}
}

// HT4 remains an HTTP-defense test after the UI cap: a manually constructed
// request can name an item past the parser window and must receive the
// truncation/range notice instead of a new 400 lock.
func TestUIP4OnlyBeyondDirectTruncationWindowIsReported(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	values := url.Values{"csrf": {csrf}, "only": {strconv.Itoa(p4DecisionBatchRenderLimit)}}
	for index := 0; index < p4DecisionBatchRenderLimit+1; index++ {
		p4Item(values, index, "task", int64(index+1), "A: 노드 로컬 저장")
	}
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer-batch", assertion, cookie, values, h.URL, false)
	responseBody := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(responseBody, "절단됨 1000/1001") || !strings.Contains(responseBody, "처리 범위 밖") || len(hub.requests()) != 0 {
		t.Fatalf("HT4 direct status=%d hub=%d body=%q", response.StatusCode, len(hub.requests()), responseBody)
	}
}

// HT5: recommended mode used to drop the remainder notice whenever the first
// batch produced no result rows, repeating "nothing selected" forever.
func TestUIP4RecommendedKeepsRemainderNoticeWithoutResults(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	for index := 0; index < 51; index++ {
		claimAndTransition(t, s, createUITask(t, s, lane, "HT5 legacy "+strconv.Itoa(index)), "needs_decision", "Pick\noptions: 승인 | 반려")
	}
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	extra := url.Values{"csrf": {csrf}, "mode": {"recommended"}}
	response, _ := submitRenderedDecisionForm(t, h, assertion, cookie, body, extra)
	responseBody := responseText(t, response)
	if response.StatusCode != http.StatusOK || len(hub.requests()) != 0 {
		t.Fatalf("HT5 status=%d hub=%d", response.StatusCode, len(hub.requests()))
	}
	if !strings.Contains(responseBody, "건이 남았습니다") {
		t.Fatalf("HT5 remainder notice dropped: %q", responseBody)
	}
}

// AC5: start with the reachable 3000-card mixed backlog. The repeated
// submissions assert against the owned records still open in storage, not the
// re-rendered-card count (which legitimately remains 1000 for many rounds).
func TestUIP4MixedThreeThousandBacklogConverges(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	tasks := p4StructuredDecisionTasks(t, s, uiLane(t, "lane-a"), "AC5 task", 1000)
	escalations := p4Escalations(t, s, uiLane(t, "lane-e"), 1000, p4StructuredDecisionQuestion())
	lanes := []int64{}
	for group := 0; group < 20; group++ {
		lanes = append(lanes, p4LaneDecisions(t, s, uiLane(t, "lane-l"), 50)...)
	}
	hub.mu.Lock()
	hub.persist = func(request ingressRequest) {
		if _, _, err := s.AppendRelayEvent(context.Background(), store.RelayEvent{Kind: request.Kind, OwnerLane: request.Lane, EventID: request.EventID, Text: request.Text, Reason: "http_ingress:" + request.Label}); err != nil {
			t.Errorf("persist mixed decision event: %v", err)
		}
	}
	hub.mu.Unlock()
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	counter := newP4OwnedDecisionCounter(t, tasks, escalations, lanes)
	counts := counter.counts(t)
	if counts != (p4OpenDecisionCounts{Tasks: 1000, Escalations: 1000, Lanes: 1000}) {
		t.Fatalf("AC5 initial open decisions=%+v", counts)
	}
	body := responseText(t, uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, ""))
	_, indexes := p4SerializedDecisionForm(t, body, nil)
	if len(indexes) != p4DecisionBatchRenderLimit || !strings.Contains(body, `data-render-notice>현재 조회된 3000건 중 1000건 표시`) {
		t.Fatalf("AC5 initial render=%d notice=%t", len(indexes), strings.Contains(body, `data-render-notice>현재 조회된 3000건 중 1000건 표시`))
	}
	submitAndRequireProgress := func(label string, form string, fields url.Values) string {
		t.Helper()
		before := counter.counts(t)
		response, rendered := submitRenderedDecisionForm(t, h, assertion, cookie, form, fields)
		responseBody := responseText(t, response)
		after := counter.counts(t)
		if rendered > p4DecisionBatchRenderLimit || response.StatusCode != http.StatusOK || after.total() >= before.total() {
			t.Fatalf("AC5 %s rendered=%d status=%d open before=%+v after=%+v body=%q", label, rendered, response.StatusCode, before, after, responseBody)
		}
		return responseBody
	}
	onlyIndex := p4Index(t, body, "task", tasks[len(tasks)-1])
	body = submitAndRequireProgress("only", body, url.Values{"csrf": {csrf}, "only": {strconv.Itoa(onlyIndex)}})
	selectedIndex := p4Index(t, body, "task", tasks[len(tasks)-2])
	body = submitAndRequireProgress("selected", body, url.Values{"csrf": {csrf}, "mode": {"selected"}, "items." + strconv.Itoa(selectedIndex) + ".select": {"1"}})
	body = submitAndRequireProgress("recommended", body, url.Values{"csrf": {csrf}, "mode": {"recommended"}})
	for pass := 0; counts.total() > 0 && pass < 70; pass++ {
		counts = counter.counts(t)
		if counts.total() == 0 {
			break
		}
		fields := url.Values{"csrf": {csrf}, "mode": {"recommended"}}
		if counts.Tasks == 0 && counts.Escalations > 0 {
			fields.Set("mode", "selected")
			visible := p4VisibleOwnedIndexes(t, body, "escalation", escalations)
			if len(visible) == 0 {
				t.Fatalf("AC5 no owned escalation was visible while %+v remained", counts)
			}
			for _, index := range visible {
				fields.Set("items."+strconv.Itoa(index)+".select", "1")
			}
		}
		if counts.Tasks == 0 && counts.Escalations == 0 && counts.Lanes > 0 {
			fields.Set("mode", "selected")
			visible := p4VisibleOwnedIndexes(t, body, "lane", lanes)
			if len(visible) == 0 {
				t.Fatalf("AC5 no owned lane decision was visible while %+v remained", counts)
			}
			for _, index := range visible {
				fields.Set("items."+strconv.Itoa(index)+".select", "1")
				fields.Set("items."+strconv.Itoa(index)+".custom", "continue")
			}
		}
		body = submitAndRequireProgress("drain-"+strconv.Itoa(pass), body, fields)
		counts = counter.counts(t)
	}
	if counts != (p4OpenDecisionCounts{}) {
		t.Fatalf("AC5 mixed backlog did not converge: remaining=%+v", counts)
	}
}
