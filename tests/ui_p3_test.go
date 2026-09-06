package tests

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ui"
)

const p3HubToken = "hub-fixture-token"

const p3Nodes = `{"nodes":[{"machine_id":"machine-a","state":"connected","accepting":true,"accepting_effective":true,"accepting_override":"","alert_class":"","connected_since":"2026-01-01T00:00:00Z","last_ping_ms":12,"load":{"load1":0.4,"load5":0.4,"load15":0.4,"ncpu":4},"memory":{"free_pct":91.8,"compressed_mb":null,"swap_used_mb":0,"psi_some_avg10":0.1,"source":"proc_meminfo"},"last_note":"synthetic note","remote_meta":{"source":"fixture","nested":{"value":1}}}]}`
const p3Lanes = `{"lanes":[{"lane":"lane-a","machine":"machine-a","pane":"w1:p1","parent":"","sink":false}]}`
const p3Jobs = `{"jobs":[{"job_id":"job-a","machine":"machine-a","owner_lane":"lane-a","role":"worker","tier":"T1","pane":"w1:p1","started_at":"2026-01-01T00:00:00Z","last_event_kind":"job.claimed","last_event_at":"2026-01-01T00:00:00Z"},{"job_id":"job-b","machine":"machine-a","owner_lane":"lane-a","role":"verifier","tier":"T2","pane":"w1:p2","started_at":"2026-01-01T00:01:00Z"}]}`

type p3FakeHub struct {
	mu sync.Mutex

	token string
	code  map[string]int
	body  map[string]string

	acceptingCalls int
	server         *httptest.Server
}

func newP3FakeHub(t *testing.T) *p3FakeHub {
	t.Helper()
	hub := &p3FakeHub{
		token: p3HubToken,
		code:  map[string]int{"nodes": http.StatusOK, "lanes": http.StatusOK, "jobs": http.StatusOK, "accepting": http.StatusOK},
		body: map[string]string{
			"nodes":     p3Nodes,
			"lanes":     p3Lanes,
			"jobs":      p3Jobs,
			"accepting": `{"machine_id":"machine-a","accepting":true}`,
		},
	}
	hub.server = httptest.NewServer(http.HandlerFunc(hub.serveHTTP))
	t.Cleanup(hub.server.Close)
	return hub
}

func (h *p3FakeHub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+h.token {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
		return
	}
	var key string
	switch r.URL.Path {
	case "/v1/nodes":
		key = "nodes"
	case "/v1/lanes":
		key = "lanes"
	case "/v1/jobs":
		key = "jobs"
	case "/v1/nodes/machine-a/accepting":
		key = "accepting"
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	h.mu.Lock()
	if key == "accepting" {
		h.acceptingCalls++
	}
	status, body := h.code[key], h.body[key]
	h.mu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (h *p3FakeHub) set(key string, status int, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.code[key], h.body[key] = status, body
}

func (h *p3FakeHub) calls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.acceptingCalls
}

func newP3UITestServer(t *testing.T, s *store.Store, fixture *uiJWTFixture, hubURL, hubToken string, services []string) *httptest.Server {
	t.Helper()
	access, err := cfaccess.New(cfaccess.Config{
		TeamDomain:          "example.cloudflareaccess.com",
		AUD:                 "ui-audience",
		AllowedEmails:       []string{"admin@example.com"},
		AllowedServiceNames: services,
		Issuer:              fixture.issuer,
		CertsURL:            fixture.certs.URL,
		CacheTTL:            time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := ui.New(ui.Config{Store: s, Access: access, HubURL: hubURL, HubToken: hubToken})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token"}, UI: handler}.Handler())
}

func p3ServiceAssertion(t *testing.T, fixture *uiJWTFixture) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":         fixture.issuer,
		"aud":         "ui-audience",
		"exp":         time.Now().Add(time.Hour).Unix(),
		"common_name": "glance-fixture",
		"email":       "",
	})
	token.Header["kid"] = "test-kid"
	raw, err := token.SignedString(fixture.private)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func p3Request(t *testing.T, client *http.Client, method, rawURL, assertion string, body []byte, contentType string, cookies ...*http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cf-Access-Jwt-Assertion", assertion)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, cookie := range cookies {
		if cookie != nil {
			req.AddCookie(cookie)
		}
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func p3JSON(t *testing.T, response *http.Response) (string, map[string]any) {
	t.Helper()
	body := responseText(t, response)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode JSON: %v body=%q", err, body)
	}
	return body, decoded
}

func p3Glance(t *testing.T, h *httptest.Server, assertion string) (string, map[string]any) {
	t.Helper()
	response := p3Request(t, h.Client(), http.MethodGet, h.URL+"/ui/api/glance", assertion, nil, "")
	if response.StatusCode != http.StatusOK {
		body := responseText(t, response)
		t.Fatalf("glance status=%d body=%q", response.StatusCode, body)
	}
	return p3JSON(t, response)
}

func TestUIP3GlanceSuccessAndNoLeakage(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newP3FakeHub(t)
	h := newP3UITestServer(t, s, fixture, hub.server.URL, hub.token, []string{" GLANCE-FIXTURE "})
	defer h.Close()
	assertion := p3ServiceAssertion(t, fixture)

	response := p3Request(t, h.Client(), http.MethodGet, h.URL+"/ui/api/glance", assertion, nil, "")
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/json; charset=utf-8" || response.Header.Get("Cache-Control") != "no-store" {
		body := responseText(t, response)
		t.Fatalf("glance headers status=%d content-type=%q cache=%q body=%q", response.StatusCode, response.Header.Get("Content-Type"), response.Header.Get("Cache-Control"), body)
	}
	body, got := p3JSON(t, response)
	if strings.Contains(body, hub.token) || strings.Contains(body, assertion) || strings.Contains(body, "admin@example.com") || strings.Contains(body, hub.server.URL) {
		t.Fatalf("glance leaked a configured secret or origin: %q", body)
	}
	hubView := got["hub"].(map[string]any)
	if hubView["ok"] != true || hubView["error"] != "" {
		t.Fatalf("hub view=%v", hubView)
	}
	node := got["nodes"].([]any)[0].(map[string]any)
	for _, key := range []string{"machine_id", "last_note", "remote_meta", "memory", "active_jobs"} {
		if _, ok := node[key]; !ok {
			t.Fatalf("node omitted hub key %q: %v", key, node)
		}
	}
	if node["active_jobs"] != float64(2) || node["memory"].(map[string]any)["psi_some_avg10"] != float64(0.1) {
		t.Fatalf("node augmentation=%v", node)
	}
	var wantJobs map[string]any
	if err := json.Unmarshal([]byte(p3Jobs), &wantJobs); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["jobs"], wantJobs["jobs"]) {
		t.Fatalf("jobs changed: got=%v want=%v", got["jobs"], wantJobs["jobs"])
	}
	var wantLanes map[string]any
	if err := json.Unmarshal([]byte(p3Lanes), &wantLanes); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["lanes"], wantLanes["lanes"]) {
		t.Fatalf("lanes changed: got=%v want=%v", got["lanes"], wantLanes["lanes"])
	}
	if paths := got["console_paths"]; !reflect.DeepEqual(paths, map[string]any{"decisions": "/ui/decisions", "queue": "/ui/queue", "fleet": "/ui/fleet"}) {
		t.Fatalf("console paths=%v", paths)
	}
}

func TestUIP3GlanceFailureNoLeakage(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	assertion := p3ServiceAssertion(t, fixture)
	h := newP3UITestServer(t, s, fixture, "", p3HubToken, []string{"glance-fixture"})
	defer h.Close()
	body, got := p3Glance(t, h, assertion)
	if got["hub"].(map[string]any)["error"] != "unconfigured" || strings.Contains(body, p3HubToken) || strings.Contains(body, assertion) || strings.Contains(body, "admin@example.com") {
		t.Fatalf("failure glance leaked a configured secret: %q", body)
	}
}

func TestUIP3GlanceTaskAggregation(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer h.Close()
	assertion := p3ServiceAssertion(t, fixture)
	_, before := p3Glance(t, h, assertion)
	beforeTasks := before["tasks"].(map[string]any)
	beforeStates := beforeTasks["by_state"].(map[string]any)
	beforePending := int(beforeTasks["decisions_pending"].(float64))
	lane := uiLane(t, "p3-task")

	backlog := createUITask(t, s, lane, "backlog")
	_ = backlog
	claimed := createUITask(t, s, lane, "claimed")
	if _, err := s.ClaimTask(t.Context(), claimed.ID, "p3-worker"); err != nil {
		t.Fatal(err)
	}
	inProgress := createUITask(t, s, lane, "in progress")
	inProgress = claimAndTransition(t, s, inProgress, "in_progress", "work")
	verifying := createUITask(t, s, lane, "verifying")
	verifying = claimAndTransition(t, s, verifying, "in_progress", "work")
	if _, err := s.TransitionTask(t.Context(), verifying.ID, "verifying", "p3-worker", "verify", nil); err != nil {
		t.Fatal(err)
	}
	joining := createUITask(t, s, lane, "joining")
	joining = claimAndTransition(t, s, joining, "in_progress", "work")
	if _, err := s.TransitionTask(t.Context(), joining.ID, "join", "p3-worker", "join", nil); err != nil {
		t.Fatal(err)
	}
	decision := createUITask(t, s, lane, "decision")
	_ = claimAndTransition(t, s, decision, "needs_decision", "choose")
	hold := createUITask(t, s, lane, "hold")
	if _, err := s.TransitionTask(t.Context(), hold.ID, "hold", "p3-worker", "hold", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 21; i++ {
		task := createUITask(t, s, lane, "active task")
		if _, err := s.ClaimTask(t.Context(), task.ID, "p3-worker"); err != nil {
			t.Fatal(err)
		}
	}
	seedRelay(t, s, lane, "lane.event", "", "[decision-needed] choose synthetic", "", "")

	_, after := p3Glance(t, h, assertion)
	tasks := after["tasks"].(map[string]any)
	states := tasks["by_state"].(map[string]any)
	if len(states) != 7 {
		t.Fatalf("by_state keys=%v", states)
	}
	for state, delta := range map[string]int{"backlog": 1, "claimed": 22, "in_progress": 1, "verifying": 1, "join": 1, "needs_decision": 1, "hold": 1} {
		if got, want := int(states[state].(float64)), int(beforeStates[state].(float64))+delta; got != want {
			t.Fatalf("state %s got=%d want=%d", state, got, want)
		}
	}
	if got, want := int(tasks["decisions_pending"].(float64)), beforePending+2; got != want {
		t.Fatalf("decisions pending got=%d want=%d", got, want)
	}
	active := tasks["active"].([]any)
	if len(active) != 20 {
		t.Fatalf("active length=%d", len(active))
	}
	ids := make([]int64, 0, len(active))
	for _, raw := range active {
		entry := raw.(map[string]any)
		if len(entry) != 7 {
			t.Fatalf("active fields=%v", entry)
		}
		ids = append(ids, int64(entry["id"].(float64)))
	}
	if !sort.SliceIsSorted(ids, func(i, j int) bool { return ids[i] > ids[j] }) {
		t.Fatalf("active IDs are not descending for tied updates: %v", ids)
	}
}

func TestUIP3GlanceHubFailures(t *testing.T) {
	fixture := newUIJWTFixture(t)
	assertion := p3ServiceAssertion(t, fixture)

	t.Run("unconfigured", func(t *testing.T) {
		h := newP3UITestServer(t, uiStore(t), fixture, "", p3HubToken, []string{"glance-fixture"})
		defer h.Close()
		body, got := p3Glance(t, h, assertion)
		if got["hub"].(map[string]any)["error"] != "unconfigured" || !strings.Contains(body, `"nodes":[]`) || !strings.Contains(body, `"lanes":[]`) || !strings.Contains(body, `"jobs":[]`) || strings.Contains(body, p3HubToken) || strings.Contains(body, assertion) || strings.Contains(body, "admin@example.com") {
			t.Fatalf("unconfigured glance=%q", body)
		}
	})
	t.Run("status", func(t *testing.T) {
		hub := newP3FakeHub(t)
		hub.set("nodes", http.StatusInternalServerError, `{"error":"fixture"}`)
		h := newP3UITestServer(t, uiStore(t), fixture, hub.server.URL, hub.token, []string{"glance-fixture"})
		defer h.Close()
		_, got := p3Glance(t, h, assertion)
		if got["hub"].(map[string]any)["error"] != "status_500" || len(got["nodes"].([]any)) != 0 || len(got["lanes"].([]any)) != 0 || len(got["jobs"].([]any)) != 0 {
			t.Fatalf("status failure=%v", got)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		hub := newP3FakeHub(t)
		h := newP3UITestServer(t, uiStore(t), fixture, hub.server.URL, hub.token, []string{"glance-fixture"})
		hub.server.Close()
		defer h.Close()
		_, got := p3Glance(t, h, assertion)
		if got["hub"].(map[string]any)["error"] != "unreachable" || len(got["nodes"].([]any)) != 0 || len(got["lanes"].([]any)) != 0 || len(got["jobs"].([]any)) != 0 {
			t.Fatalf("unreachable failure=%v", got)
		}
	})
}

func TestUIP3GlanceTruncation(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
	defer h.Close()
	lane := uiLane(t, "p3-large")
	for i := 0; i < 5; i++ {
		task := createUITask(t, s, lane, strings.Repeat("x", 60*1024))
		if _, err := s.ClaimTask(t.Context(), task.ID, "p3-worker"); err != nil {
			t.Fatal(err)
		}
	}
	body, got := p3Glance(t, h, p3ServiceAssertion(t, fixture))
	if len(body) > 256*1024 || got["truncated"] != true {
		t.Fatalf("truncation size=%d response=%v", len(body), got["truncated"])
	}
}

func TestUIP3ServiceIdentityBoundaryAndFailClosed(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	service := p3ServiceAssertion(t, fixture)
	t.Run("service boundary", func(t *testing.T) {
		h := newP3UITestServer(t, s, fixture, "", "", []string{"glance-fixture"})
		defer h.Close()
		for _, test := range []struct{ method, path string }{
			{http.MethodGet, "/ui"}, {http.MethodGet, "/ui/timeline"}, {http.MethodGet, "/ui/queue"}, {http.MethodGet, "/ui/decisions"}, {http.MethodGet, "/ui/compose"}, {http.MethodGet, "/ui/fleet"}, {http.MethodGet, "/ui/events"}, {http.MethodGet, "/ui/fragments/timeline"}, {http.MethodGet, "/ui/doc/synthetic"}, {http.MethodGet, "/ui/static/htmx.min.js"}, {http.MethodPost, "/ui/decisions/answer"}, {http.MethodPost, "/ui/compose"},
		} {
			response := p3Request(t, h.Client(), test.method, h.URL+test.path, service, nil, "")
			if response.StatusCode != http.StatusForbidden {
				body := responseText(t, response)
				t.Fatalf("%s %s status=%d body=%q", test.method, test.path, response.StatusCode, body)
			}
			_ = responseText(t, response)
		}
	})
	t.Run("empty allowlist", func(t *testing.T) {
		h := newP3UITestServer(t, s, fixture, "", "", nil)
		defer h.Close()
		response := p3Request(t, h.Client(), http.MethodGet, h.URL+"/ui/api/glance", service, nil, "")
		if response.StatusCode != http.StatusUnauthorized {
			body := responseText(t, response)
			t.Fatalf("service fail-open status=%d body=%q", response.StatusCode, body)
		}
		_ = responseText(t, response)
		email := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
		for _, path := range []string{"/ui/timeline", "/ui/api/glance"} {
			response = p3Request(t, h.Client(), http.MethodGet, h.URL+path, email, nil, "")
			if response.StatusCode != http.StatusOK {
				body := responseText(t, response)
				t.Fatalf("email %s status=%d body=%q", path, response.StatusCode, body)
			}
			_ = responseText(t, response)
		}
	})
}

func TestUIP3AcceptingEmailCSRFAndServiceProxy(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newP3FakeHub(t)
	h := newP3UITestServer(t, s, fixture, hub.server.URL, hub.token, []string{"glance-fixture"})
	defer h.Close()
	email := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	service := p3ServiceAssertion(t, fixture)
	payload := []byte(`{"accepting":true,"reason":"synthetic"}`)

	cookie, csrf := uiCSRF(t, h.Client(), h.URL+"/ui/compose", email)
	validReq, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.URL+"/ui/api/nodes/machine-a/accepting", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	validReq.Header.Set("Cf-Access-Jwt-Assertion", email)
	validReq.Header.Set("Content-Type", "application/json")
	validReq.Header.Set("Origin", h.URL)
	validReq.Header.Set("X-CSRF-Token", csrf)
	validReq.AddCookie(cookie)
	response, err := h.Client().Do(validReq)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		body := responseText(t, response)
		t.Fatalf("email accepting status=%d body=%q", response.StatusCode, body)
	}
	_ = responseText(t, response)
	if hub.calls() != 1 {
		t.Fatalf("email CSRF calls=%d", hub.calls())
	}
	missingOrigin, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.URL+"/ui/api/nodes/machine-a/accepting", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	missingOrigin.Header.Set("Cf-Access-Jwt-Assertion", email)
	missingOrigin.Header.Set("Content-Type", "application/json")
	missingOrigin.Header.Set("X-CSRF-Token", csrf)
	missingOrigin.AddCookie(cookie)
	response, err = h.Client().Do(missingOrigin)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		body := responseText(t, response)
		t.Fatalf("email missing Origin status=%d body=%q", response.StatusCode, body)
	}
	_ = responseText(t, response)
	for _, token := range []string{"", "wrong"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.URL+"/ui/api/nodes/machine-a/accepting", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Cf-Access-Jwt-Assertion", email)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", h.URL)
		if token != "" {
			req.Header.Set("X-CSRF-Token", token)
		}
		req.AddCookie(cookie)
		response, err := h.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusForbidden {
			body := responseText(t, response)
			t.Fatalf("email CSRF token=%q status=%d body=%q", token, response.StatusCode, body)
		}
		_ = responseText(t, response)
	}
	if hub.calls() != 1 {
		t.Fatalf("CSRF failure reached hub calls=%d", hub.calls())
	}

	response = p3Request(t, h.Client(), http.MethodPost, h.URL+"/ui/api/nodes/machine-a/accepting", service, payload, "application/json")
	if response.StatusCode != http.StatusOK || responseText(t, response) != `{"machine_id":"machine-a","accepting":true}` {
		t.Fatalf("service accepting proxy status=%d", response.StatusCode)
	}
	hub.set("accepting", http.StatusBadRequest, `{"error":"fixture_bad"}`)
	response = p3Request(t, h.Client(), http.MethodPost, h.URL+"/ui/api/nodes/machine-a/accepting", service, payload, "application/json")
	if response.StatusCode != http.StatusBadRequest || responseText(t, response) != `{"error":"fixture_bad"}` {
		t.Fatalf("hub 4xx status=%d", response.StatusCode)
	}
	calls := hub.calls()
	for _, machine := range []string{"Machine-A", strings.Repeat("a", 33), "a_b"} {
		response = p3Request(t, h.Client(), http.MethodPost, h.URL+"/ui/api/nodes/"+machine+"/accepting", service, payload, "application/json")
		if response.StatusCode != http.StatusBadRequest {
			body := responseText(t, response)
			t.Fatalf("machine %q status=%d body=%q", machine, response.StatusCode, body)
		}
		_ = responseText(t, response)
	}
	response = p3Request(t, h.Client(), http.MethodPost, h.URL+"/ui/api/nodes/machine-a/accepting", service, []byte(`{"accepting":true,"reason":"`+strings.Repeat("x", 121)+`"}`), "application/json")
	if response.StatusCode != http.StatusBadRequest {
		body := responseText(t, response)
		t.Fatalf("long reason status=%d body=%q", response.StatusCode, body)
	}
	_ = responseText(t, response)
	response = p3Request(t, h.Client(), http.MethodPost, h.URL+"/ui/api/nodes/machine-a/accepting", service, payload, "")
	if response.StatusCode != http.StatusUnsupportedMediaType {
		body := responseText(t, response)
		t.Fatalf("missing content type status=%d body=%q", response.StatusCode, body)
	}
	_ = responseText(t, response)
	if hub.calls() != calls {
		t.Fatalf("invalid accepting calls=%d want=%d", hub.calls(), calls)
	}
	hub.server.Close()
	response = p3Request(t, h.Client(), http.MethodPost, h.URL+"/ui/api/nodes/machine-a/accepting", service, payload, "application/json")
	if response.StatusCode != http.StatusBadGateway || responseText(t, response) != `{"error":"hub_unavailable"}` {
		t.Fatalf("hub unavailable status=%d", response.StatusCode)
	}
}
