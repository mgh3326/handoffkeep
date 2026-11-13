package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// UI disposition tests (#493): hk:doc decision/2026-09-21/task493-phase1-review
// C1 (operator identity only), C3 (next batch shown), C4 (no-JWT requests,
// e.g. a direct tailnet listener, never reach a disposition write).

func uiGetWithCookie(t *testing.T, client *http.Client, endpoint, assertion string, cookie *http.Cookie) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cf-Access-Jwt-Assertion", assertion)
	req.AddCookie(cookie)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status=%d", endpoint, response.StatusCode)
	}
	return body
}

// uiDispositionPage is the JSON view of GET /ui/api/dispositions: the open
// items, the signed batch snapshot, and the unnotified answers.
type uiDispositionPage struct {
	Head  string `json:"head"`
	Items []struct {
		Task        store.Task `json:"task"`
		Gen         int64      `json:"gen"`
		Recommended string     `json:"recommended"`
	} `json:"items"`
	BatchN     int    `json:"batch_n"`
	Counts     string `json:"counts"`
	NextBatch  int    `json:"next_batch"`
	BatchID    string `json:"batch_id"`
	IssuedAt   string `json:"issued_at"`
	Snapshot   string `json:"snapshot"`
	Token      string `json:"token"`
	CSRF       string `json:"csrf"`
	Unnotified []struct {
		EventID string `json:"event_id"`
		IDs     string `json:"ids"`
	} `json:"unnotified"`
}

func uiDispositions(t *testing.T, client *http.Client, base, assertion string, cookie *http.Cookie) uiDispositionPage {
	t.Helper()
	var page uiDispositionPage
	if err := json.Unmarshal([]byte(uiGetWithCookie(t, client, base+"/ui/api/dispositions", assertion, cookie)), &page); err != nil {
		t.Fatalf("decode /ui/api/dispositions: %v", err)
	}
	return page
}

func uiUnnotified(page uiDispositionPage, eventID string) bool {
	for _, notice := range page.Unnotified {
		if notice.EventID == eventID {
			return true
		}
	}
	return false
}

func persistToStore(t *testing.T, s *store.Store) func(ingressRequest) {
	return func(request ingressRequest) {
		if _, _, err := s.AppendRelayEvent(t.Context(), store.RelayEvent{Kind: "lane.event", OwnerLane: request.Lane, EventID: request.EventID, Text: request.Text}); err != nil {
			t.Errorf("persist relay event: %v", err)
		}
	}
}

func setHub(hub *fakeIngressHub, mode string, persist func(ingressRequest)) {
	hub.mu.Lock()
	hub.mode, hub.persist = mode, persist
	hub.mu.Unlock()
}

func TestUIDispositionSingleAnswerRecordsThenNotifies(t *testing.T) {
	s, _ := schemaTestStore(t, "ui_disposition_single")
	drainOpenDispositions(t, s)
	t.Cleanup(func() { drainOpenDispositions(t, s) })
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	setHub(hub, "created", persistToStore(t, s))
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "director")
	x := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 2, "C"))
	gen := openGen(t, s, x.ID)

	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/queue", assertion)
	page := uiDispositions(t, h.Client(), h.URL, assertion, cookie)
	if !strings.Contains(page.Head, "미처분 1 · 최고령 0일") || len(page.Items) != 1 || page.Items[0].Task.ID != x.ID || page.Items[0].Recommended != "C" {
		t.Fatalf("disposition section not served: %+v", page)
	}
	fields := url.Values{"csrf": {csrf}, "id": {strconv.FormatInt(x.ID, 10)}, "gen": {strconv.FormatInt(gen, 10)}, "key": {"B"}}
	response := uiPostForm(t, h.Client(), h.URL+"/ui/dispositions/answer", assertion, cookie, fields, h.URL, true)
	body := responseText(t, response)
	eventID := fmt.Sprintf("web-disposition-%d-g%d", x.ID, gen)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "처분 기록·전송됨(event_id="+eventID+")") {
		t.Fatalf("answer status=%d body=%q", response.StatusCode, body)
	}
	requests := hub.requests()
	want := fmt.Sprintf("[decision] #%d: B: 후속 발주 (from operator(web) admin@example.com)", x.ID)
	if len(requests) != 1 || requests[0].EventID != eventID || requests[0].Lane != lane || requests[0].Text != want {
		t.Fatalf("hub requests=%+v", requests)
	}
	got, _, _ := s.GetTask(t.Context(), x.ID)
	if got.State != "researching" || got.Refs.Disposition.Answer == nil || got.Refs.Disposition.Answer.Key != "B" || got.Refs.Disposition.Answer.By != "operator:admin@example.com" {
		t.Fatalf("answered item=%+v", got.Refs.Disposition)
	}
	// A second submission of the same form is a conflict and emits nothing.
	response = uiPostForm(t, h.Client(), h.URL+"/ui/dispositions/answer", assertion, cookie, fields, h.URL, true)
	if response.StatusCode != http.StatusConflict || len(hub.requests()) != 1 {
		t.Fatalf("resubmit status=%d hub=%d", response.StatusCode, len(hub.requests()))
	}
	responseText(t, response)
}

// ESC-5 / tester surface ④: record first; a failed emit is shown and can be
// re-sent with the same event ID and text.
func TestUIDispositionEmitFailureStaysRecordedAndRenotifies(t *testing.T) {
	s, _ := schemaTestStore(t, "ui_disposition_emit")
	drainOpenDispositions(t, s)
	t.Cleanup(func() { drainOpenDispositions(t, s) })
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	setHub(hub, "failed", nil)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "director")
	x := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))
	gen := openGen(t, s, x.ID)
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/queue", assertion)
	fields := url.Values{"csrf": {csrf}, "id": {strconv.FormatInt(x.ID, 10)}, "gen": {strconv.FormatInt(gen, 10)}, "key": {"A"}}
	response := uiPostForm(t, h.Client(), h.URL+"/ui/dispositions/answer", assertion, cookie, fields, h.URL, true)
	body := responseText(t, response)
	eventID := fmt.Sprintf("web-disposition-%d-g%d", x.ID, gen)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "처분 기록됨 · 레인 통지 실패") || !strings.Contains(body, "event_id="+eventID) {
		t.Fatalf("emit failure status=%d body=%q", response.StatusCode, body)
	}
	if !uiUnnotified(uiDispositions(t, h.Client(), h.URL, assertion, cookie), eventID) {
		t.Fatal("failed emit is not listed as awaiting notification")
	}
	if got, _, _ := s.GetTask(t.Context(), x.ID); got.State != "researching" || got.Refs.Disposition.Answer.EventID != eventID {
		t.Fatalf("failed emit must keep the recorded answer: %+v", got)
	}
	first := hub.requests()[0]
	setHub(hub, "created", persistToStore(t, s))
	response = uiPostForm(t, h.Client(), h.URL+"/ui/dispositions/renotify", assertion, cookie, url.Values{"csrf": {csrf}, "event_id": {eventID}}, h.URL, true)
	body = responseText(t, response)
	requests := hub.requests()
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "재통지됨") || len(requests) != 2 || requests[1].EventID != eventID || requests[1].Text != first.Text {
		t.Fatalf("renotify status=%d requests=%+v body=%q", response.StatusCode, requests, body)
	}
	if uiUnnotified(uiDispositions(t, h.Client(), h.URL, assertion, cookie), eventID) {
		t.Fatal("notified item still listed as pending")
	}
	response = uiPostForm(t, h.Client(), h.URL+"/ui/dispositions/renotify", assertion, cookie, url.Values{"csrf": {csrf}, "event_id": {eventID}}, h.URL, true)
	if body = responseText(t, response); !strings.Contains(body, "이미 통지됨") || len(hub.requests()) != 2 {
		t.Fatalf("second renotify body=%q hub=%d", body, len(hub.requests()))
	}
}

// AC ④ / AC+2: one POST answers exactly the rendered snapshot.
func TestUIDispositionBatchIsOneActionOverTheSnapshot(t *testing.T) {
	s, dbURL := schemaTestStore(t, "ui_disposition_batch")
	drainOpenDispositions(t, s)
	t.Cleanup(func() { drainOpenDispositions(t, s) })
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	setHub(hub, "created", persistToStore(t, s))
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "director")
	items := []store.Task{
		mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A")),
		mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 1, "C")),
		mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "E")),
	}
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/queue", assertion)
	page := uiDispositions(t, h.Client(), h.URL, assertion, cookie)
	if page.BatchN != 3 || page.Counts != "A 1 · C 1 · E 1" || page.NextBatch != 0 {
		t.Fatalf("batch section: %+v", page)
	}
	fields := url.Values{"csrf": {csrf}, "batch_id": {page.BatchID}, "issued_at": {page.IssuedAt}, "snapshot": {page.Snapshot}, "token": {page.Token}}
	late := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))

	// Tampering with the snapshot (dropping an item) is refused with no write.
	tampered := url.Values{}
	for k, v := range fields {
		tampered[k] = v
	}
	tampered.Set("snapshot", strings.Join(strings.Split(fields.Get("snapshot"), ",")[:2], ","))
	before := uiRowCountsAt(t, dbURL)
	response := uiPostForm(t, h.Client(), h.URL+"/ui/dispositions/accept-batch", assertion, cookie, tampered, h.URL, true)
	responseText(t, response)
	if response.StatusCode != http.StatusForbidden || uiRowCountsAt(t, dbURL) != before || len(hub.requests()) != 0 {
		t.Fatalf("tampered snapshot status=%d", response.StatusCode)
	}

	response = uiPostForm(t, h.Client(), h.URL+"/ui/dispositions/accept-batch", assertion, cookie, fields, h.URL, true)
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "권고 일괄 수락 3건 기록") {
		t.Fatalf("batch status=%d body=%q", response.StatusCode, body)
	}
	for _, item := range items {
		got, _, _ := s.GetTask(t.Context(), item.ID)
		if got.State != "researching" || got.Refs.Disposition.Answer == nil || got.Refs.Disposition.Answer.Key != got.Refs.Disposition.Recommended || got.Refs.Disposition.Answer.BatchID == "" {
			t.Fatalf("#%d after batch: state=%s answer=%+v", item.ID, got.State, got.Refs.Disposition.Answer)
		}
	}
	if got, _, _ := s.GetTask(t.Context(), late.ID); got.State != "backlog" {
		t.Fatalf("item created after the snapshot was answered: %s", got.State)
	}
	requests := hub.requests()
	wantText := fmt.Sprintf("[decision] disposition-batch %s: #%d=A #%d=C #%d=E (from operator(web) admin@example.com)", fields.Get("batch_id"), items[0].ID, items[1].ID, items[2].ID)
	if len(requests) != 1 || requests[0].Text != wantText || requests[0].EventID != "web-disposition-batch-"+fields.Get("batch_id")+"-"+lane {
		t.Fatalf("batch emits=%+v", requests)
	}
	// Replaying the same action answers nothing and emits nothing.
	response = uiPostForm(t, h.Client(), h.URL+"/ui/dispositions/accept-batch", assertion, cookie, fields, h.URL, true)
	if body = responseText(t, response); !strings.Contains(body, "권고 일괄 수락 0건 기록") || !strings.Contains(body, "건너뜀 3건") || len(hub.requests()) != 1 {
		t.Fatalf("replay body=%q hub=%d", body, len(hub.requests()))
	}
}

// ESC-2 / C3: beyond 50 open items the header and the button name the next batch.
func TestUIDispositionNextBatchIsShown(t *testing.T) {
	s, _ := schemaTestStore(t, "ui_disposition_next_batch")
	drainOpenDispositions(t, s)
	t.Cleanup(func() { drainOpenDispositions(t, s) })
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "director")
	for i := 0; i < store.DispositionBatchLimit+2; i++ {
		mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "E"))
	}
	cookie, _ := uiCSRF(t, h.Client(), h.URL+"/ui/queue", assertion)
	page := uiDispositions(t, h.Client(), h.URL, assertion, cookie)
	if !strings.Contains(page.Head, "미처분 52 · 최고령 0일 · 다음 묶음 2건") || page.BatchN != store.DispositionBatchLimit || page.Counts != "E 50" || page.NextBatch != 2 {
		t.Fatalf("next batch not named: %+v", page)
	}
	if n := len(strings.Split(page.Snapshot, ",")); n != store.DispositionBatchLimit {
		t.Fatalf("snapshot size=%d", n)
	}
}

// AC+1 / C1 / C4 / tester surface ①: every non-operator path is refused and writes nothing.
func TestUIDispositionRefusesNonOperatorPaths(t *testing.T) {
	s, dbURL := schemaTestStore(t, "ui_disposition_refuse")
	drainOpenDispositions(t, s)
	t.Cleanup(func() { drainOpenDispositions(t, s) })
	lane := uiLane(t, "director")
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	setHub(hub, "created", persistToStore(t, s))
	h := newP3UITestServer(t, s, fixture, hub.server.URL, "hub-test-token", []string{"glance-fixture"})
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	x := mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))
	gen := openGen(t, s, x.ID)
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/queue", assertion)
	answer := url.Values{"csrf": {csrf}, "id": {strconv.FormatInt(x.ID, 10)}, "gen": {strconv.FormatInt(gen, 10)}, "key": {"A"}}
	before := uiRowCountsAt(t, dbURL)
	unchanged := func(label string) {
		t.Helper()
		if got, _, _ := s.GetTask(t.Context(), x.ID); got.State != "backlog" {
			t.Fatalf("%s moved the item to %s", label, got.State)
		}
		if after := uiRowCountsAt(t, dbURL); after.TaskEvents != before.TaskEvents || after.Tasks != before.Tasks {
			t.Fatalf("%s wrote: before=%+v after=%+v", label, before, after)
		}
	}

	// C1: an allowlisted Access service identity is not the operator.
	service := p3ServiceAssertion(t, fixture)
	for _, path := range []string{"/ui/dispositions/answer", "/ui/dispositions/accept-batch", "/ui/dispositions/renotify"} {
		response := uiPostForm(t, h.Client(), h.URL+path, service, cookie, answer, h.URL, false)
		// An empty body is ServeHTTP's boundary refusal; the handler's own
		// refusal (defence in depth) is tested in internal/ui.
		if body := responseText(t, response); response.StatusCode != http.StatusForbidden || body != "" {
			t.Fatalf("service identity %s status=%d", path, response.StatusCode)
		}
	}
	unchanged("service identity")

	// C4: a request with no Access assertion (e.g. straight to the tailnet
	// listener, which serves the same mux) never reaches the handler.
	for _, path := range []string{"/ui/dispositions/answer", "/ui/dispositions/accept-batch"} {
		response := uiPostForm(t, h.Client(), h.URL+path, "", cookie, answer, h.URL, false)
		responseText(t, response)
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no-JWT %s status=%d", path, response.StatusCode)
		}
	}
	unchanged("no JWT")

	// The retired generic decision and compose POST routes are gone: they
	// answer 405 and emit nothing.
	for _, path := range []string{"/ui/decisions/answer", "/ui/decisions/answer-batch", "/ui/compose"} {
		response := uiPostForm(t, h.Client(), h.URL+path, assertion, cookie, answer, h.URL, false)
		responseText(t, response)
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("retired route %s status=%d", path, response.StatusCode)
		}
	}
	if len(hub.requests()) != 0 {
		t.Fatalf("a retired route emitted: %+v", hub.requests())
	}
	unchanged("retired routes")

	// Bearer-token API: the transition and resolve routes refuse.
	for path, payload := range map[string]any{
		fmt.Sprintf("/v1/tasks/%d/transition", x.ID): map[string]string{"to": "dropped", "note": "self"},
		"/v1/decisions/resolve":                      map[string]any{"type": "task", "id": x.ID, "by": "operator", "answer": "A: 배포"},
	} {
		raw, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, h.URL+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer node-token")
		req.Header.Set("Content-Type", "application/json")
		response, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if body := responseText(t, response); response.StatusCode != http.StatusConflict || !strings.Contains(body, "disposition_operator_only") {
			t.Fatalf("API %s status=%d body=%q", path, response.StatusCode, body)
		}
	}
	unchanged("bearer API")

	// The operator's own route still works afterwards.
	response := uiPostForm(t, h.Client(), h.URL+"/ui/dispositions/answer", assertion, cookie, answer, h.URL, true)
	responseText(t, response)
	if got, _, _ := s.GetTask(t.Context(), x.ID); response.StatusCode != http.StatusOK || got.State != "researching" {
		t.Fatalf("operator answer status=%d state=%s", response.StatusCode, got.State)
	}
}

// Director ruling on SHOULD-3: glance reports open disposition items beside,
// not inside, decisions_pending.
func TestUIGlanceReportsDispositionsSeparately(t *testing.T) {
	s, _ := schemaTestStore(t, "ui_glance_dispositions")
	drainOpenDispositions(t, s)
	t.Cleanup(func() { drainOpenDispositions(t, s) })
	fixture := newUIJWTFixture(t)
	h := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer h.Close()
	assertion := p3ServiceAssertion(t, fixture)
	_, before := p3Glance(t, h, assertion)
	lane := uiLane(t, "director")
	mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "A"))
	mustCreateDisposition(t, s, dispositionInput(lane, dispositionPR(t), 0, "E"))
	claimAndTransition(t, s, createUITask(t, s, lane, "builder question"), "needs_decision", "which?")
	_, after := p3Glance(t, h, assertion)
	b, a := before["tasks"].(map[string]any), after["tasks"].(map[string]any)
	delta := func(key string) int { return int(a[key].(float64)) - int(b[key].(float64)) }
	if delta("dispositions_open") != 2 || delta("decisions_pending") != 1 {
		t.Fatalf("glance: dispositions_open +%d decisions_pending +%d, want +2 and +1", delta("dispositions_open"), delta("decisions_pending"))
	}
	if int(a["by_state"].(map[string]any)["awaiting_decision"].(float64))-int(b["by_state"].(map[string]any)["awaiting_decision"].(float64)) != 3 {
		t.Fatal("by_state.awaiting_decision must stay the raw awaiting tally")
	}
}
