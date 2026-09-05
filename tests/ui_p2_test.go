package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type ingressRequest struct {
	Method        string
	Path          string
	Authorization string
	Kind          string `json:"kind"`
	Lane          string `json:"lane"`
	EventID       string `json:"event_id"`
	Text          string `json:"text"`
	Label         string `json:"label"`
}

type fakeIngressHub struct {
	t       *testing.T
	mu      sync.Mutex
	mode    string
	seen    []ingressRequest
	persist func(ingressRequest)
	server  *httptest.Server
}

func newFakeIngressHub(t *testing.T) *fakeIngressHub {
	t.Helper()
	hub := &fakeIngressHub{t: t, mode: "created"}
	hub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/relay/events" {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/v1/nodes":
				_, _ = io.WriteString(w, `{"nodes":[]}`)
			case "/v1/jobs":
				_, _ = io.WriteString(w, `{"jobs":[]}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		var payload ingressRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode fake hub request: %v", err)
		}
		payload.Method, payload.Path, payload.Authorization = r.Method, r.URL.Path, r.Header.Get("Authorization")
		hub.mu.Lock()
		hub.seen = append(hub.seen, payload)
		mode, persist := hub.mode, hub.persist
		hub.mu.Unlock()
		if mode == "timeout" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode {
		case "created":
			if persist != nil {
				persist(payload)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"id":42,"event_id":"`+payload.EventID+`","lane":"`+payload.Lane+`","routed":true,"machine":""}`)
		case "duplicate":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"duplicate_event_id","id":42}`)
		case "bad":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_event"}`)
		case "failed":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"persistence_failed"}`)
		default:
			t.Fatalf("unknown fake hub mode %q", mode)
		}
	}))
	t.Cleanup(hub.server.Close)
	return hub
}

func (h *fakeIngressHub) requests() []ingressRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ingressRequest(nil), h.seen...)
}

func uiCSRF(t *testing.T, client *http.Client, endpoint, assertion string) (*http.Cookie, string) {
	t.Helper()
	response := uiRequest(t, client, http.MethodGet, endpoint, assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("form GET %s status=%d body=%q", endpoint, response.StatusCode, body)
	}
	start := strings.Index(body, `name="csrf" value="`)
	if start < 0 {
		t.Fatalf("form GET %s did not render CSRF field: %q", endpoint, body)
	}
	start += len(`name="csrf" value="`)
	end := strings.Index(body[start:], `"`)
	if end < 0 {
		t.Fatal("unterminated CSRF field")
	}
	var cookie *http.Cookie
	for _, candidate := range response.Cookies() {
		if candidate.Name == "hk_ui_csrf" {
			cookie = candidate
			break
		}
	}
	if cookie == nil || cookie.Value != body[start:start+end] || !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/ui" || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("invalid CSRF cookie=%+v token=%q", cookie, body[start:start+end])
	}
	return cookie, cookie.Value
}

func uiPostForm(t *testing.T, client *http.Client, endpoint, assertion string, cookie *http.Cookie, fields url.Values, origin string, htmx bool) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint, strings.NewReader(fields.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Cf-Access-Jwt-Assertion", assertion)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decisionFields(kind string, id int64, answer, csrf string) url.Values {
	return url.Values{"type": {kind}, "id": {strconv.FormatInt(id, 10)}, "answer": {answer}, "csrf": {csrf}}
}

func TestUIWriteDecisionRoutes(t *testing.T) {
	t.Setenv("HANDOFFKEEP_UI_LANES", "lane-a,lane-b")
	t.Setenv("HANDOFFKEEP_UI_ADMIRAL_LANES", "lane-a")
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	// Task decision: protocol payload, htmx fragment, transition audit, and log.
	task := createUITask(t, s, "lane-a", "approve task")
	claimAndTransition(t, s, task, "needs_decision", "Pick one\noptions: approve | hold | reject")
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	var audit bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&audit)
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("task", task.ID, "approve", csrf), h.URL, true)
	body := responseText(t, response)
	log.SetOutput(oldOutput)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `id="ui-content"`) || !strings.Contains(body, "전송됨(event_id=web-decision-task-") {
		t.Fatalf("task htmx result status=%d body=%q", response.StatusCode, body)
	}
	requests := hub.requests()
	if len(requests) != 1 {
		t.Fatalf("hub request count=%d", len(requests))
	}
	got := requests[0]
	if got.Method != http.MethodPost || got.Path != "/v1/relay/events" || got.Authorization != "Bearer hub-test-token" || got.Kind != "lane.event" || got.Lane != "lane-a" || !strings.HasPrefix(got.EventID, "web-decision-task-"+strconv.FormatInt(task.ID, 10)+"-") || got.Text != "[decision] #"+strconv.FormatInt(task.ID, 10)+": approve (from operator(web) admin@example.com)" || got.Label != "operator-web" {
		t.Fatalf("hub request=%+v", got)
	}
	updated, found, err := s.GetTask(t.Context(), task.ID)
	if err != nil || !found || updated.State != "claimed" {
		t.Fatalf("task after answer=%+v found=%t err=%v", updated, found, err)
	}
	latest := updated.Events[len(updated.Events)-1]
	if latest.By != "operator:admin@example.com" || !strings.Contains(latest.Note, "approve") || !strings.Contains(latest.Note, "via web by admin@example.com") {
		t.Fatalf("task event=%+v", latest)
	}
	lines := strings.Split(strings.TrimSpace(audit.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "ui.write email=admin@example.com action=decision target=lane-a#") || !strings.Contains(lines[0], "result=ok") || strings.Contains(lines[0], "approve") || strings.Contains(lines[0], "hub-test-token") {
		t.Fatalf("audit=%q", audit.String())
	}

	// A 409 still transitions a task and is presented as an idempotent success.
	hub.mu.Lock()
	hub.mode = "duplicate"
	hub.mu.Unlock()
	duplicateTask := createUITask(t, s, "lane-a", "duplicate task")
	claimAndTransition(t, s, duplicateTask, "needs_decision", "choose")
	cookie, csrf = uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("task", duplicateTask.ID, "yes", csrf), h.URL, true)
	if body = responseText(t, response); !strings.Contains(body, "이미 전송됨") {
		t.Fatalf("duplicate response=%q", body)
	}
	updated, _, _ = s.GetTask(t.Context(), duplicateTask.ID)
	if updated.State != "claimed" {
		t.Fatalf("duplicate did not transition task: %s", updated.State)
	}

	// A failed emit leaves the task untouched.
	hub.mu.Lock()
	hub.mode = "failed"
	hub.mu.Unlock()
	failedTask := createUITask(t, s, "lane-a", "failed task")
	claimAndTransition(t, s, failedTask, "needs_decision", "choose")
	cookie, csrf = uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("task", failedTask.ID, "yes", csrf), h.URL, true)
	if body = responseText(t, response); !strings.Contains(body, "레인 전송 실패: hub status 502") {
		t.Fatalf("failure response=%q", body)
	}
	updated, _, _ = s.GetTask(t.Context(), failedTask.ID)
	if updated.State != "needs_decision" {
		t.Fatalf("failed emit transitioned task: %s", updated.State)
	}

	// A concurrent transition after a 201 is distinguished from a hub failure.
	hub.mu.Lock()
	hub.mode = "created"
	conflictTask := createUITask(t, s, "lane-a", "conflict task")
	claimAndTransition(t, s, conflictTask, "needs_decision", "choose")
	hub.persist = func(ingressRequest) {
		if _, err := s.TransitionTask(context.Background(), conflictTask.ID, "claimed", "other-operator", "already handled", nil); err != nil {
			t.Errorf("make task conflict: %v", err)
		}
	}
	hub.mu.Unlock()
	cookie, csrf = uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("task", conflictTask.ID, "yes", csrf), h.URL, true)
	if body = responseText(t, response); !strings.Contains(body, "이미 답변됨") {
		t.Fatalf("conflict response=%q", body)
	}

	// A store rejection after a 201 retains the event identifier and explicit
	// manual-transition action instead of conflating it with delivery failure.
	hub.mu.Lock()
	hub.persist = nil
	hub.mu.Unlock()
	errorTask := createUITask(t, s, "lane-a", "transition error task")
	claimAndTransition(t, s, errorTask, "needs_decision", "choose")
	cookie, csrf = uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("task", errorTask.ID, "-----BEGIN synthetic", csrf), h.URL, true)
	if body = responseText(t, response); !strings.Contains(body, "web-decision-task-") || !strings.Contains(body, "수동 전이 필요") {
		t.Fatalf("transition error response=%q", body)
	}
	updated, _, _ = s.GetTask(t.Context(), errorTask.ID)
	if updated.State != "needs_decision" {
		t.Fatalf("task error changed state: %s", updated.State)
	}

	// Invalid hub input is also surfaced as a lane-send failure and cannot
	// transition the task.
	hub.mu.Lock()
	hub.mode = "bad"
	hub.mu.Unlock()
	badTask := createUITask(t, s, "lane-a", "bad hub task")
	claimAndTransition(t, s, badTask, "needs_decision", "choose")
	cookie, csrf = uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("task", badTask.ID, "yes", csrf), h.URL, true)
	if body = responseText(t, response); !strings.Contains(body, "레인 전송 실패: hub status 400") {
		t.Fatalf("bad hub response=%q", body)
	}
	updated, _, _ = s.GetTask(t.Context(), badTask.ID)
	if updated.State != "needs_decision" {
		t.Fatalf("bad hub transitioned task: %s", updated.State)
	}

	// A lane decision receives the mandatory closing prefix and hub persistence
	// makes the original inbox item disappear.
	hub.mu.Lock()
	hub.mode = "created"
	hub.persist = func(request ingressRequest) {
		_, _, err := s.AppendRelayEvent(context.Background(), store.RelayEvent{Kind: "lane.event", OwnerLane: request.Lane, EventID: request.EventID, Text: request.Text, Reason: "http_ingress:" + request.Label})
		if err != nil {
			t.Errorf("persist fake ingress event: %v", err)
		}
	}
	hub.mu.Unlock()
	laneEvent := seedRelay(t, s, "lane-b", "lane.event", "", "[decision-needed] choose lane", "", "")
	cookie, csrf = uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("lane", laneEvent.ID, "go", csrf), h.URL, true)
	if body = responseText(t, response); !strings.Contains(body, "전송됨(event_id=web-decision-lane-") {
		t.Fatalf("lane response=%q", body)
	}
	requests = hub.requests()
	if !strings.HasPrefix(requests[len(requests)-1].Text, "[decision-answered] #"+strconv.FormatInt(laneEvent.ID, 10)+":") {
		t.Fatalf("lane event text=%q", requests[len(requests)-1].Text)
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, "")
	if body = responseText(t, response); strings.Contains(body, laneEvent.Text) {
		t.Fatalf("answered lane item remained in inbox: %q", body)
	}

	// Escalations route to their owner lane and do not transition unrelated tasks.
	escalation := seedRelay(t, s, "lane-b", "job.escalate", "p2-open-escalation", "", "Need a decision", "")
	before := uiRowCounts(t)
	cookie, csrf = uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response = uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("escalation", escalation.ID, "continue", csrf), h.URL, true)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("escalation status=%d", response.StatusCode)
	}
	requests = hub.requests()
	if requests[len(requests)-1].Lane != "lane-b" || before.Tasks != uiRowCounts(t).Tasks || before.TaskEvents != uiRowCounts(t).TaskEvents {
		t.Fatalf("escalation did not preserve task state: request=%+v before=%+v after=%+v", requests[len(requests)-1], before, uiRowCounts(t))
	}
}

func TestUIWriteSecurityAndCompose(t *testing.T) {
	t.Setenv("HANDOFFKEEP_UI_LANES", "lane-a")
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/compose", assertion)
	base := url.Values{"lane": {"lane-a"}, "text": {"hello"}, "csrf": {csrf}}
	before := uiRowCounts(t)

	for name, mutate := range map[string]func(*http.Cookie, url.Values, string) (int, string){
		"missing cookie": func(_ *http.Cookie, form url.Values, origin string) (int, string) {
			return http.StatusForbidden, origin
		},
		"mismatched form": func(_ *http.Cookie, form url.Values, origin string) (int, string) {
			form.Set("csrf", "not-the-cookie")
			return http.StatusForbidden, origin
		},
		"email bound": func(_ *http.Cookie, form url.Values, origin string) (int, string) {
			return http.StatusForbidden, origin
		},
		"expired": func(c *http.Cookie, form url.Values, origin string) (int, string) {
			c.Value = "0." + strings.Split(c.Value, ".")[1] + "." + strings.Split(c.Value, ".")[2]
			form.Set("csrf", c.Value)
			return http.StatusForbidden, origin
		},
	} {
		t.Run(name, func(t *testing.T) {
			form := url.Values{}
			for key, values := range base {
				form[key] = append([]string(nil), values...)
			}
			copyCookie := *cookie
			want, origin := mutate(&copyCookie, form, h.URL)
			useCookie := &copyCookie
			useAssertion := assertion
			if name == "missing cookie" {
				useCookie = nil
			}
			if name == "email bound" {
				useAssertion = fixture.token(t, "ADMIN@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
			}
			response := uiPostForm(t, h.Client(), h.URL+"/ui/compose", useAssertion, useCookie, form, origin, true)
			if response.StatusCode != want {
				t.Fatalf("status=%d, want %d body=%q", response.StatusCode, want, responseText(t, response))
			}
			response.Body.Close()
		})
	}
	for _, origin := range []string{"http://elsewhere.invalid", ""} {
		response := uiPostForm(t, h.Client(), h.URL+"/ui/compose", assertion, cookie, base, origin, true)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("origin=%q status=%d", origin, response.StatusCode)
		}
		response.Body.Close()
	}
	for _, invalid := range []string{
		strings.Repeat("x", 2049-len("[event]  (from operator(web) admin@example.com)")),
		"contains\nnewline",
	} {
		form := url.Values{"lane": {"lane-a"}, "text": {invalid}, "csrf": {csrf}}
		response := uiPostForm(t, h.Client(), h.URL+"/ui/compose", assertion, cookie, form, h.URL, true)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid compose status=%d body=%q", response.StatusCode, responseText(t, response))
		}
		response.Body.Close()
	}
	response := uiPostForm(t, h.Client(), h.URL+"/ui/compose", assertion, cookie, url.Values{"lane": {"other"}, "text": {"hello"}, "csrf": {csrf}}, h.URL, true)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unregistered lane status=%d", response.StatusCode)
	}
	response.Body.Close()
	if got := hub.requests(); len(got) != 0 || uiRowCounts(t) != before {
		t.Fatalf("rejected requests had side effects: hub=%+v before=%+v after=%+v", got, before, uiRowCounts(t))
	}

	// Exactly 2048 bytes succeeds; htmx gets a replacement fragment.
	exact := strings.Repeat("x", 2048-len("[event]  (from operator(web) admin@example.com)"))
	response = uiPostForm(t, h.Client(), h.URL+"/ui/compose", assertion, cookie, url.Values{"lane": {"lane-a"}, "text": {exact}, "csrf": {csrf}}, h.URL, true)
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, `id="ui-content"`) || !strings.Contains(body, "Timeline's ✓") {
		t.Fatalf("exact compose status=%d body=%q", response.StatusCode, body)
	}
	if got := hub.requests(); len(got) != 1 || len([]byte(got[0].Text)) != 2048 || !strings.HasPrefix(got[0].EventID, "web-msg-") {
		t.Fatalf("exact compose hub=%+v", got)
	}
	beforeGET, hubGET := uiRowCounts(t), len(hub.requests())
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/compose?lane=lane-a&text=must-not-send", assertion, "")
	response.Body.Close()
	if afterGET := uiRowCounts(t); afterGET != beforeGET || len(hub.requests()) != hubGET {
		t.Fatalf("GET performed a write: db before=%+v after=%+v hub before=%d after=%d", beforeGET, afterGET, hubGET, len(hub.requests()))
	}

	noRedirect := &http.Client{Transport: h.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cookie, csrf = uiCSRF(t, noRedirect, h.URL+"/ui/compose", assertion)
	response = uiPostForm(t, noRedirect, h.URL+"/ui/compose", assertion, cookie, url.Values{"lane": {"lane-a"}, "text": {"native"}, "csrf": {csrf}}, h.URL, false)
	if response.StatusCode != http.StatusSeeOther || !strings.HasPrefix(response.Header.Get("Location"), "/ui/compose?result=") {
		t.Fatalf("native response status=%d location=%q", response.StatusCode, response.Header.Get("Location"))
	}
	response.Body.Close()

	for _, invalidAssertion := range []string{"", fixture.token(t, "admin@example.com", "other", time.Now().Add(time.Hour), nil), fixture.token(t, "viewer@example.com", "ui-audience", time.Now().Add(time.Hour), nil), fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(-time.Hour), nil)} {
		response = uiPostForm(t, h.Client(), h.URL+"/ui/compose", invalidAssertion, cookie, url.Values{"lane": {"lane-a"}, "text": {"nope"}, "csrf": {csrf}}, h.URL, true)
		if response.StatusCode != http.StatusUnauthorized || responseText(t, response) != "" {
			t.Fatalf("invalid assertion status=%d", response.StatusCode)
		}
	}

	// No destination setting disables compose and rejects a direct POST before
	// contacting the otherwise configured fake hub.
	t.Setenv("HANDOFFKEEP_UI_LANES", "")
	noLanes := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer noLanes.Close()
	cookie, csrf = uiCSRF(t, noLanes.Client(), noLanes.URL+"/ui/compose", assertion)
	response = uiPostForm(t, noLanes.Client(), noLanes.URL+"/ui/compose", assertion, cookie, url.Values{"lane": {"lane-a"}, "text": {"blocked"}, "csrf": {csrf}}, noLanes.URL, true)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unconfigured lanes status=%d", response.StatusCode)
	}
	response.Body.Close()
	if body := responseText(t, uiRequest(t, noLanes.Client(), http.MethodGet, noLanes.URL+"/ui/compose", assertion, "")); !strings.Contains(body, "fieldset disabled") || !strings.Contains(body, "No destination lanes are configured.") {
		t.Fatal("compose form was not disabled without lanes")
	}
}

func TestUIWriteHubTimeoutLeavesTaskUnchanged(t *testing.T) {
	t.Setenv("HANDOFFKEEP_UI_LANES", "lane-a")
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	hub.mu.Lock()
	hub.mode = "timeout"
	hub.mu.Unlock()
	h := newUITestServerWithClient(t, s, fixture, hub.server.URL, "hub-test-token", 0, &http.Client{Timeout: 20 * time.Millisecond})
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	task := createUITask(t, s, "lane-a", "timeout task")
	claimAndTransition(t, s, task, "needs_decision", "choose")
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/decisions", assertion)
	response := uiPostForm(t, h.Client(), h.URL+"/ui/decisions/answer", assertion, cookie, decisionFields("task", task.ID, "yes", csrf), h.URL, true)
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "레인 전송 실패:") {
		t.Fatalf("timeout response status=%d body=%q", response.StatusCode, body)
	}
	updated, _, _ := s.GetTask(t.Context(), task.ID)
	if updated.State != "needs_decision" || len(hub.requests()) != 1 {
		t.Fatalf("timeout changed task or did not make one hub request: task=%s hub=%d", updated.State, len(hub.requests()))
	}
}

func TestUIWriteNeverLeaksHubToken(t *testing.T) {
	t.Setenv("HANDOFFKEEP_UI_LANES", "lane-a")
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	const token = "hub-test-token"
	h := newUITestServer(t, s, fixture, hub.server.URL, token, 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	for _, endpoint := range []string{"/ui/timeline", "/ui/queue", "/ui/decisions", "/ui/fleet", "/ui/compose"} {
		response := uiRequest(t, h.Client(), http.MethodGet, h.URL+endpoint, assertion, "")
		body := responseText(t, response)
		if strings.Contains(body, token) {
			t.Fatalf("token leaked in %s body", endpoint)
		}
		for header, values := range response.Header {
			if strings.Contains(strings.Join(values, ","), token) {
				t.Fatalf("token leaked in %s header %s", endpoint, header)
			}
		}
	}
	hub.mu.Lock()
	hub.mode = "failed"
	hub.mu.Unlock()
	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/compose", assertion)
	response := uiPostForm(t, h.Client(), h.URL+"/ui/compose", assertion, cookie, url.Values{"lane": {"lane-a"}, "text": {"failure"}, "csrf": {csrf}}, h.URL, true)
	body := responseText(t, response)
	if strings.Contains(body, token) {
		t.Fatal("token leaked in write failure")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL+"/ui/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cf-Access-Jwt-Assertion", assertion)
	response, err = h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(response.Body)
	line, _ := reader.ReadString('\n')
	data, _ := reader.ReadString('\n')
	cancel()
	response.Body.Close()
	if strings.Contains(line+data, token) || strings.Contains(strings.Join(response.Header.Values("Content-Type"), ","), token) {
		t.Fatal("token leaked in SSE")
	}
}

func TestUIDocIngressBadgeAndApproval(t *testing.T) {
	t.Setenv("HANDOFFKEEP_UI_LANES", "lane-a")
	t.Setenv("HANDOFFKEEP_UI_ADMIRAL_LANES", "lane-a")
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	if _, _, err := s.PutDocument(t.Context(), store.Document{Key: "reports/x/y.md", Kind: "report", Body: "<report body>", CreatedBy: "test-node"}); err != nil {
		t.Fatal(err)
	}
	seedRelay(t, s, "lane-a", "lane.event", "", "see doc:reports/x/y.md", "", "")
	ingress := seedRelay(t, s, "lane-a", "lane.event", "", "from ingress", "", "")
	if _, _, err := s.AppendRelayEvent(t.Context(), store.RelayEvent{Kind: "lane.event", OwnerLane: ingress.OwnerLane, EventID: "p2-ingress-badge-" + strconv.FormatInt(time.Now().UnixNano(), 10), Text: "badge event", Reason: "http_ingress:operator-web"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AppendRelayEvent(t.Context(), store.RelayEvent{Kind: "lane.event", OwnerLane: ingress.OwnerLane, EventID: "p2-hidden-reason-" + strconv.FormatInt(time.Now().UnixNano(), 10), Text: "hidden reason", Reason: "lane-ping"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AppendRelayEvent(t.Context(), store.RelayEvent{Kind: "lane.event", OwnerLane: ingress.OwnerLane, EventID: "p2-ingress-decision-" + strconv.FormatInt(time.Now().UnixNano(), 10), Text: "[decision-needed] ingress decision", Reason: "http_ingress:operator-web"}); err != nil {
		t.Fatal(err)
	}
	seedRelay(t, s, "lane-a", "lane.event", "", "hidden reason", "", "")
	approvalTitle := uiLane(t, "approval")
	generalTitle := uiLane(t, "general")
	approvalOptions := []string{uiLane(t, "approve"), uiLane(t, "hold"), uiLane(t, "reject")}
	escalationOptions := []string{uiLane(t, "esc-yes"), uiLane(t, "esc-no")}
	laneOptions := []string{uiLane(t, "lane-yes"), uiLane(t, "lane-no")}
	approval := createUITask(t, s, "lane-a", approvalTitle)
	claimAndTransition(t, s, approval, "needs_decision", "Approve?\noptions: "+strings.Join(approvalOptions, " | "))
	general := createUITask(t, s, "lane-b", generalTitle)
	claimAndTransition(t, s, general, "needs_decision", "Free text")
	seedRelay(t, s, "lane-a", "job.escalate", uiLane(t, "options-job"), "", "Choose\noptions: "+strings.Join(escalationOptions, " | "), "")
	seedRelay(t, s, "lane-a", "lane.event", "", "[decision-needed] options: "+strings.Join(laneOptions, " | "), "", "")

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline", assertion, "")
	body := responseText(t, response)
	if !strings.Contains(body, `href="/ui/doc/reports/x/y.md"`) || !strings.Contains(body, "operator-web") || strings.Contains(body, "lane-ping") {
		t.Fatalf("timeline doc/badge body=%q", body)
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/doc/reports/x/y.md", assertion, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(responseText(t, response), "&lt;report body&gt;") {
		t.Fatal("document was not safely rendered")
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/doc/missing.md", assertion, "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing document status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/doc/reports/../secret", assertion, "")
	if response.StatusCode != http.StatusBadRequest && response.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid document status=%d", response.StatusCode)
	}
	response.Body.Close()

	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, "")
	body = responseText(t, response)
	for _, option := range append(append(approvalOptions, escalationOptions...), laneOptions...) {
		if strings.Count(body, `name="answer" value="`+option+`"`) != 1 {
			t.Fatalf("option %q was not one button: %q", option, body)
		}
	}
	generalStart := strings.Index(body, generalTitle)
	generalEnd := -1
	if generalStart >= 0 {
		generalEnd = strings.Index(body[generalStart:], "</article>")
	}
	if !strings.Contains(body, "Awaiting your approval") || strings.Count(body, approvalTitle) != 1 || generalStart < 0 || generalEnd < 0 || !strings.Contains(body[generalStart:generalStart+generalEnd], `name="answer" required`) || !strings.Contains(body, "sender operator-web") {
		t.Fatalf("approval/options body=%q", body)
	}
	t.Setenv("HANDOFFKEEP_UI_ADMIRAL_LANES", "")
	withoutApproval := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer withoutApproval.Close()
	response = uiRequest(t, withoutApproval.Client(), http.MethodGet, withoutApproval.URL+"/ui/decisions", assertion, "")
	if body = responseText(t, response); strings.Contains(body, "Awaiting your approval") {
		t.Fatal("approval section rendered without admiral lanes")
	}

	// All other UI POST routes remain GET-only, and GET query strings cannot write.
	before := uiRowCounts(t)
	for _, endpoint := range []string{"/ui/compose?lane=lane-a&text=unsafe", "/ui/decisions/answer?type=task&id=" + strconv.FormatInt(approval.ID, 10)} {
		response = uiRequest(t, h.Client(), http.MethodGet, h.URL+endpoint, assertion, "")
		response.Body.Close()
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		response = uiRequest(t, h.Client(), method, h.URL+"/ui/not-a-write-route", assertion, "")
		if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet {
			t.Fatalf("method guard %s status=%d allow=%q", method, response.StatusCode, response.Header.Get("Allow"))
		}
		response.Body.Close()
	}
	if after := uiRowCounts(t); after != before {
		t.Fatalf("GET/method guard wrote rows: before=%+v after=%+v", before, after)
	}
}
