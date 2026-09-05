package tests

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/ui"
)

type uiJWTFixture struct {
	private *rsa.PrivateKey
	issuer  string
	certs   *httptest.Server
}

func newUIJWTFixture(t *testing.T) *uiJWTFixture {
	t.Helper()
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &uiJWTFixture{private: private}
	fixture.certs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		n := base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes())
		exponent := private.PublicKey.E
		e := []byte{byte(exponent >> 16), byte(exponent >> 8), byte(exponent)}
		for len(e) > 1 && e[0] == 0 {
			e = e[1:]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kid": "test-kid", "kty": "RSA", "n": n, "e": base64.RawURLEncoding.EncodeToString(e)}}})
	}))
	fixture.issuer = fixture.certs.URL
	t.Cleanup(fixture.certs.Close)
	return fixture
}

func (f *uiJWTFixture) token(t *testing.T, email string, audience any, expires time.Time, key *rsa.PrivateKey) string {
	t.Helper()
	if key == nil {
		key = f.private
	}
	claims := jwt.MapClaims{"iss": f.issuer, "aud": audience, "exp": expires.Unix(), "email": email}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "test-kid"
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *uiJWTFixture) noneToken(t *testing.T) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"iss": f.issuer, "aud": "ui-audience", "exp": time.Now().Add(time.Hour).Unix(), "email": "admin@example.com"})
	token.Header["kid"] = "test-kid"
	raw, err := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f *uiJWTFixture) hsToken(t *testing.T) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"iss": f.issuer, "aud": "ui-audience", "exp": time.Now().Add(time.Hour).Unix(), "email": "admin@example.com"})
	token.Header["kid"] = "test-kid"
	raw, err := token.SignedString(x509.MarshalPKCS1PublicKey(&f.private.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func newUITestServer(t *testing.T, s *store.Store, fixture *uiJWTFixture, hubURL, hubToken string, poll time.Duration) *httptest.Server {
	return newUITestServerWithClient(t, s, fixture, hubURL, hubToken, poll, nil)
}

func newUITestServerWithClient(t *testing.T, s *store.Store, fixture *uiJWTFixture, hubURL, hubToken string, poll time.Duration, hubClient *http.Client) *httptest.Server {
	t.Helper()
	access, err := cfaccess.New(cfaccess.Config{
		TeamDomain:    "example.cloudflareaccess.com",
		AUD:           "ui-audience",
		AllowedEmails: []string{"admin@example.com"},
		Issuer:        fixture.issuer,
		CertsURL:      fixture.certs.URL,
		CacheTTL:      time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := ui.New(ui.Config{Store: s, Access: access, HubURL: hubURL, HubToken: hubToken, HubHTTPClient: hubClient, PollInterval: poll})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token"}, UI: handler}.Handler())
}

func uiRequest(t *testing.T, client *http.Client, method, rawURL, assertion, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if assertion != "" {
		req.Header.Set("Cf-Access-Jwt-Assertion", assertion)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func responseText(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func uiStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for UI tests")
	}
	s, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func uiLane(t *testing.T, prefix string) string {
	t.Helper()
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func seedRelay(t *testing.T, s *store.Store, lane, kind, jobID, text, question, last string) store.RelayEvent {
	t.Helper()
	if jobID == "" {
		jobID = fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	x, _, err := s.AppendRelayEvent(t.Context(), store.RelayEvent{Kind: kind, JobID: jobID, OwnerLane: lane, ReportPath: fmt.Sprintf("report-%d.md", time.Now().UnixNano()), Reason: fmt.Sprintf("reason-%d", time.Now().UnixNano()), Text: text, Question: question, ReportLastLine: last, EventID: eventID(kind)})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func eventID(kind string) string {
	if kind == "lane.event" {
		return fmt.Sprintf("event-%d", time.Now().UnixNano())
	}
	return ""
}

func createUITask(t *testing.T, s *store.Store, lane, title string) store.Task {
	t.Helper()
	task, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "test-node"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func claimAndTransition(t *testing.T, s *store.Store, task store.Task, to, note string) store.Task {
	t.Helper()
	if _, err := s.ClaimTask(t.Context(), task.ID, "test-owner"); err != nil {
		t.Fatal(err)
	}
	updated, err := s.TransitionTask(t.Context(), task.ID, to, "test-node", note, nil)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestUIAuthenticationAndBoundarySeparation(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	lane := uiLane(t, "lane-a")
	seedRelay(t, s, lane, "lane.event", "", "timeline body", "", "")
	valid := fixture.token(t, "ADMIN@example.com", []string{"ui-audience", "other"}, time.Now().Add(time.Hour), nil)

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline", valid, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(responseText(t, response), "timeline body") {
		t.Fatal("valid CF Access assertion did not render the timeline")
	}
	for name, assertion := range map[string]string{
		"expired":           fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(-time.Minute), nil),
		"wrong audience":    fixture.token(t, "admin@example.com", "other", time.Now().Add(time.Hour), nil),
		"unallowed email":   fixture.token(t, "viewer@example.com", "ui-audience", time.Now().Add(time.Hour), nil),
		"alg none":          fixture.noneToken(t),
		"alg HS256":         fixture.hsToken(t),
		"invalid signature": fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), mustOtherKey(t)),
	} {
		response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline", assertion, "")
		body := responseText(t, response)
		if response.StatusCode != http.StatusUnauthorized || strings.Contains(body, "Fleet console") {
			t.Fatalf("%s: status=%d body=%q", name, response.StatusCode, body)
		}
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline", "", "")
	if response.StatusCode != http.StatusUnauthorized || responseText(t, response) != "" {
		t.Fatal("missing assertion was not a bodyless 401")
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/tasks", valid, "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("CF assertion authenticated API: %d", response.StatusCode)
	}
	response.Body.Close()
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline", "", "node-token")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer token authenticated UI: %d", response.StatusCode)
	}
	response.Body.Close()
}

func mustOtherKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestUIFailClosedAndMethodGuard(t *testing.T) {
	s := uiStore(t)
	bare := httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token"}}.Handler())
	defer bare.Close()
	for _, endpoint := range []string{"/ui", "/ui/timeline", "/ui/events", "/ui/static/htmx.min.js"} {
		response := uiRequest(t, bare.Client(), http.MethodGet, bare.URL+endpoint, "", "")
		if response.StatusCode != http.StatusNotFound {
			t.Fatalf("%s status=%d, want 404", endpoint, response.StatusCode)
		}
		response.Body.Close()
	}
	response := uiRequest(t, bare.Client(), http.MethodGet, bare.URL+"/healthz", "", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("healthz status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = uiRequest(t, bare.Client(), http.MethodGet, bare.URL+"/v1/tasks", "", "node-token")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("API status=%d", response.StatusCode)
	}
	response.Body.Close()

	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	before := uiRowCounts(t)
	for _, endpoint := range []string{"/ui", "/ui/timeline", "/ui/queue", "/ui/decisions", "/ui/fleet", "/ui/events", "/ui/fragments/timeline"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			response := uiRequest(t, h.Client(), method, h.URL+endpoint, assertion, "")
			if response.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s status=%d", method, endpoint, response.StatusCode)
			}
			response.Body.Close()
		}
	}
	if after := uiRowCounts(t); after != before {
		t.Fatalf("read-only method guard changed rows: before=%v after=%v", before, after)
	}
}

type rowCounts struct{ Relay, Tasks, TaskEvents int64 }

func uiRowCounts(t *testing.T) rowCounts {
	t.Helper()
	db, err := pgx.Connect(t.Context(), os.Getenv("HANDOFFKEEP_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(t.Context())
	var counts rowCounts
	if err := db.QueryRow(t.Context(), `SELECT (SELECT COUNT(*) FROM relay_events),(SELECT COUNT(*) FROM tasks),(SELECT COUNT(*) FROM task_events)`).Scan(&counts.Relay, &counts.Tasks, &counts.TaskEvents); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestUITimelineQueueAndEscaping(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	laneA, laneB := uiLane(t, "lane-a"), uiLane(t, "lane-b")
	checkpointSession := uiLane(t, "session")
	if _, err := s.CreateCheckpoint(t.Context(), store.Checkpoint{Session: checkpointSession, Kind: "checkpoint", Title: "older checkpoint", Body: "first", CreatedBy: "test-node"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCheckpoint(t.Context(), store.Checkpoint{Session: checkpointSession, Kind: "checkpoint", Title: "latest checkpoint", Body: "second", CreatedBy: "test-node"}); err != nil {
		t.Fatal(err)
	}
	textEvent := seedRelay(t, s, laneA, "lane.event", "", "text wins", "", "")
	questionEvent := seedRelay(t, s, laneA, "job.escalate", "", "", "question wins", "line loses")
	lineEvent := seedRelay(t, s, laneB, "job.joined", "", "", "", "line wins")
	if _, err := s.MarkRelayEventDelivered(t.Context(), textEvent.ID, "host-a", "pane-a"); err != nil {
		t.Fatal(err)
	}
	seedRelay(t, s, laneA, "lane.event", "", "<script>alert(1)</script>", "", "")
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline?lane="+laneA, assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "text wins") || !strings.Contains(body, "question wins") || strings.Contains(body, "line wins") {
		t.Fatalf("timeline lane filter/content status=%d body=%q", response.StatusCode, body)
	}
	if !strings.Contains(body, "✓ delivered") || !strings.Contains(body, "title=\"host-a/pane-a\"") {
		t.Fatal("delivery marker or tooltip missing")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") || strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("stored XSS text was not escaped")
	}
	if !strings.Contains(body, "latest checkpoint") || strings.Contains(body, "older checkpoint") {
		t.Fatal("session checkpoint panel did not select the latest entry")
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline?kind=job.escalate", assertion, "")
	body = responseText(t, response)
	if !strings.Contains(body, "question wins") || strings.Contains(body, "text wins") {
		t.Fatal("kind filter did not restrict timeline")
	}
	today := time.Now().UTC().Format("2006-01-02")
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline?lane="+laneA+"&since="+today, assertion, "")
	if !strings.Contains(responseText(t, response), "text wins") {
		t.Fatal("since date boundary did not include today's relay event")
	}
	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline?lane="+laneA+"&until="+yesterday, assertion, "")
	if strings.Contains(responseText(t, response), "text wins") {
		t.Fatal("until date boundary did not exclude a later relay event")
	}
	_ = questionEvent
	_ = lineEvent

	pageLane := uiLane(t, "page")
	for i := 0; i < 201; i++ {
		seedRelay(t, s, pageLane, "lane.event", "", "page-item-"+strconv.Itoa(i), "", "")
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline?lane="+pageLane, assertion, "")
	body = responseText(t, response)
	if response.StatusCode != http.StatusOK || strings.Count(body, "page-item-") != 200 || !strings.Contains(body, "before_id=") {
		t.Fatalf("first page did not have 200 rows: count=%d", strings.Count(body, "page-item-"))
	}
	beforeID := beforeIDFromBody(t, body)
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/timeline?lane="+pageLane+"&before_id="+beforeID, assertion, "")
	body = responseText(t, response)
	if strings.Count(body, "page-item-") != 1 {
		t.Fatalf("cursor page count=%d", strings.Count(body, "page-item-"))
	}

	queueLane := uiLane(t, "queue")
	active := createUITask(t, s, queueLane, "in progress title")
	claimAndTransition(t, s, active, "in_progress", "started work")
	decision := createUITask(t, s, queueLane, "decision title")
	claimAndTransition(t, s, decision, "needs_decision", "choose a path")
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue", assertion, "")
	body = responseText(t, response)
	if !strings.Contains(body, "in progress title") || !strings.Contains(body, "decision-card") || !strings.Contains(body, "needs decision") {
		t.Fatal("queue board did not render state cells and decision emphasis")
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/fragments/task/"+strconv.FormatInt(active.ID, 10), assertion, "")
	body = responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "backlog → claimed") || !strings.Contains(body, "claimed → in_progress") || !strings.Contains(body, "started work") {
		t.Fatal("task event fragment did not render ordered history")
	}
}

func beforeIDFromBody(t *testing.T, body string) string {
	t.Helper()
	marker := "before_id="
	index := strings.Index(body, marker)
	if index < 0 {
		t.Fatal("missing timeline cursor")
	}
	value := body[index+len(marker):]
	for i, r := range value {
		if r < '0' || r > '9' {
			return value[:i]
		}
	}
	return value
}

func TestUIDecisionInbox(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	lane := uiLane(t, "lane-a")
	question := strings.Repeat("q", 2048)
	openTask := createUITask(t, s, lane, "open decision")
	claimAndTransition(t, s, openTask, "needs_decision", question)
	closedTask := createUITask(t, s, lane, "closed decision")
	claimAndTransition(t, s, closedTask, "needs_decision", "closed question")
	if _, err := s.TransitionTask(t.Context(), closedTask.ID, "claimed", "test-node", "resolved", nil); err != nil {
		t.Fatal(err)
	}
	openEscalation := seedRelay(t, s, lane, "job.escalate", "open-escalation-"+strconv.FormatInt(time.Now().UnixNano(), 10), "", "open escalation", "")
	closedJob := "closed-escalation-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	seedRelay(t, s, lane, "job.escalate", closedJob, "", "closed escalation", "")
	seedRelay(t, s, lane, "job.joined", closedJob, "", "", "")
	openLane := seedRelay(t, s, lane, "lane.event", "", "[decision-needed] decide this", "", "")
	answeredLane := uiLane(t, "lane-b")
	seedRelay(t, s, answeredLane, "lane.event", "", "[decision-needed] already answered", "", "")
	seedRelay(t, s, answeredLane, "lane.event", "", "[decision-answered] answer", "", "")
	seedRelay(t, s, lane, "lane.event", "", "ordinary event", "", "")
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/decisions", assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, question) || strings.Contains(body, "closed question") {
		t.Fatal("task decision inbox did not preserve only unresolved full question")
	}
	if !strings.Contains(body, openEscalation.JobID) || strings.Contains(body, "closed escalation") {
		t.Fatal("escalation unresolved definition was not applied")
	}
	if !strings.Contains(body, openLane.Text) || strings.Contains(body, "already answered") || strings.Contains(body, "ordinary event") {
		t.Fatal("lane event unresolved definition was not applied")
	}
}

func TestUIFleetProxyAndTokenBoundary(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	active := createUITask(t, s, uiLane(t, "lane-a"), "fleet active task")
	claimAndTransition(t, s, active, "in_progress", "started")

	unconfigured := newUITestServer(t, s, fixture, "", "", 0)
	response := uiRequest(t, unconfigured.Client(), http.MethodGet, unconfigured.URL+"/ui/fleet", assertion, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(responseText(t, response), "hub 미설정") {
		t.Fatal("unconfigured hub did not leave fleet page available")
	}
	for _, endpoint := range []string{"/ui/timeline", "/ui/queue", "/ui/decisions"} {
		response := uiRequest(t, unconfigured.Client(), http.MethodGet, unconfigured.URL+endpoint, assertion, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d with hub unset", endpoint, response.StatusCode)
		}
		response.Body.Close()
	}
	unconfigured.Close()

	mode := "normal"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/nodes":
			if mode == "fail" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"nodes":[{"machine_id":"host-a","state":"connected","accepting":true,"accepting_effective":true,"last_ping_ms":1234,"memory":{"free_pct":41.2,"compressed_mb":900,"swap_used_mb":120},"remote_meta":{"version":"v1"}}]}`)
		case "/v1/jobs":
			if mode == "unsupported" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"jobs":[{"machine":"host-a","job_id":"job-a","owner_lane":"lane-a","pane":"p1","tier":"T1","started_at":"now"}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()
	hubValue := "hub-test-value"
	h := newUITestServer(t, s, fixture, hub.URL, hubValue, 0)
	defer h.Close()
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/fleet", assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "host-a") || !strings.Contains(body, "1234") || !strings.Contains(body, "v1") || !strings.Contains(body, "41.2") || !strings.Contains(body, "900") || !strings.Contains(body, "120") || !strings.Contains(body, "job-a") || !strings.Contains(body, "fleet active task") {
		t.Fatalf("normal hub rendering failed: %q", body)
	}
	if strings.Contains(body, hubValue) {
		t.Fatal("hub credential was rendered")
	}
	for key, values := range response.Header {
		if strings.Contains(strings.Join(values, ","), hubValue) {
			t.Fatalf("hub credential leaked in header %s", key)
		}
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/static/htmx.min.js", assertion, "")
	if strings.Contains(responseText(t, response), hubValue) {
		t.Fatal("hub credential leaked in static content")
	}
	mode = "unsupported"
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/fleet", assertion, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(responseText(t, response), "jobs API 미지원") {
		t.Fatal("404 jobs endpoint was not marked unsupported")
	}
	mode = "fail"
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/fleet", assertion, "")
	body = responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "stale (") || !strings.Contains(body, "host-a") {
		t.Fatal("hub failure did not retain a stale snapshot")
	}

	freshFailure := newUITestServer(t, s, fixture, hub.URL, hubValue, 0)
	defer freshFailure.Close()
	response = uiRequest(t, freshFailure.Client(), http.MethodGet, freshFailure.URL+"/ui/fleet", assertion, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(responseText(t, response), "hub 도달 불가") {
		t.Fatal("initial hub failure did not stay available with unavailable state")
	}
}

func TestUIFleetThresholdsAndNullableMemory(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/jobs" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"nodes":[{"machine_id":"host-a","state":"connected","accepting":true,"accepting_effective":false,"accepting_override":"maintenance","alert_class":"","memory":{"free_pct":29.9,"compressed_mb":900,"swap_used_mb":1537}},{"machine_id":"host-b","state":"connected","accepting":true,"accepting_effective":true,"memory":{"free_pct":31,"compressed_mb":900,"swap_used_mb":1536}},{"machine_id":"host-c","state":"connected","accepting":true,"accepting_effective":true,"memory":null},{"machine_id":"host-d","state":"connected","accepting":true,"accepting_effective":true,"memory":{"free_pct":null,"compressed_mb":null,"swap_used_mb":null}}]}`)
	}))
	defer hub.Close()
	h := newUITestServer(t, s, fixture, hub.URL, "hub-test-value", 0)
	defer h.Close()
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/fleet", assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK || !strings.Contains(body, "free_pct 29.9") || !strings.Contains(body, "swap_used_mb 1537") || !strings.Contains(body, "reason maintenance") {
		t.Fatal("threshold input values did not render")
	}
	if strings.Count(body, `class="threshold-alert"`) != 3 { // free_pct, swap_used_mb, accepting_effective=false
		t.Fatalf("threshold alerts=%d body=%q", strings.Count(body, `class="threshold-alert"`), body)
	}
	if !strings.Contains(body, "free_pct 31") || !strings.Contains(body, "swap_used_mb 1536") || strings.Count(body, "미측정") < 6 {
		t.Fatal("non-alert boundaries or nullable memory handling failed")
	}
}

func TestUIEvents(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	h := newUITestServer(t, s, fixture, "", "", 25*time.Millisecond)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.URL+"/ui/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Cf-Access-Jwt-Assertion", assertion)
	response, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("SSE status=%d content-type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(response.Body)
	if line, _ := reader.ReadString('\n'); line != "event: delta\n" {
		t.Fatalf("initial event=%q", line)
	}
	if line, _ := reader.ReadString('\n'); !strings.Contains(line, "relay_max_id") || !strings.Contains(line, "task_event_max_id") {
		t.Fatalf("initial data=%q", line)
	}
	if line, _ := reader.ReadString('\n'); line != "\n" {
		t.Fatalf("initial terminator=%q", line)
	}
	noDelta := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		noDelta <- line
	}()
	select {
	case line := <-noDelta:
		t.Fatalf("unexpected delta without a change: %q", line)
	case <-time.After(80 * time.Millisecond):
	}
	seedRelay(t, s, uiLane(t, "lane-a"), "lane.event", "", "SSE update", "", "")
	select {
	case line := <-noDelta:
		if line != "event: delta\n" {
			t.Fatalf("delta event=%q", line)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive an SSE delta after a relay event")
	}
}

func TestUIStaticDoesNotUseExternalCDN(t *testing.T) {
	b, err := os.ReadFile("../internal/ui/static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 || bytes.Contains(b, []byte("https://")) {
		t.Fatal("vendored htmx asset is missing or contains an external CDN reference")
	}
}
