package tests

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
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
	return newUITestServerConfig(t, fixture, ui.Config{Store: s, HubURL: hubURL, HubToken: hubToken, HubHTTPClient: hubClient, PollInterval: poll})
}

// newUITestServerConfig serves config (with the fixture's Access verifier)
// behind the API handler, as `serve` does.
func newUITestServerConfig(t *testing.T, fixture *uiJWTFixture, config ui.Config) *httptest.Server {
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
	config.Access = access
	handler, err := ui.New(config)
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(api.Server{Service: api.Service{Store: config.Store}, Tokens: api.Tokens{"node": "node-token"}, UI: handler}.Handler())
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
	url := testdb.URL(t)
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
	task, err := s.CreateTask(t.Context(), store.Task{Lane: lane, Title: title, Kind: "implement", CreatedBy: "test-node", Project: projectRef("experiment")})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func claimAndTransition(t *testing.T, s *store.Store, task store.Task, to, note string) store.Task {
	t.Helper()
	// A task filed with a refs.job_id keeps it — claiming with a different
	// id would clobber the link fixtures assert on.
	jobID := task.Refs.JobID
	if jobID == "" {
		jobID = "job-1"
	}
	if _, err := s.ClaimTask(t.Context(), task.ID, "test-owner", jobID, ""); err != nil {
		t.Fatal(err)
	}
	updated, err := s.TransitionTask(t.Context(), task.ID, to, "test-node", note, nil, "")
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
	seedRelay(t, s, lane, "lane.event", "", "queue body", "", "")
	valid := fixture.token(t, "ADMIN@example.com", []string{"ui-audience", "other"}, time.Now().Add(time.Hour), nil)

	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue", valid, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(responseText(t, response), `id="shell-root"`) {
		t.Fatal("valid CF Access assertion did not render the console shell")
	}
	for name, assertion := range map[string]string{
		"expired":           fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(-time.Minute), nil),
		"wrong audience":    fixture.token(t, "admin@example.com", "other", time.Now().Add(time.Hour), nil),
		"unallowed email":   fixture.token(t, "viewer@example.com", "ui-audience", time.Now().Add(time.Hour), nil),
		"alg none":          fixture.noneToken(t),
		"alg HS256":         fixture.hsToken(t),
		"invalid signature": fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), mustOtherKey(t)),
	} {
		response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue", assertion, "")
		body := responseText(t, response)
		if response.StatusCode != http.StatusUnauthorized || strings.Contains(body, "Fleet console") {
			t.Fatalf("%s: status=%d body=%q", name, response.StatusCode, body)
		}
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue", "", "")
	if response.StatusCode != http.StatusUnauthorized || responseText(t, response) != "" {
		t.Fatal("missing assertion was not a bodyless 401")
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/v1/tasks", valid, "")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("CF assertion authenticated API: %d", response.StatusCode)
	}
	response.Body.Close()
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/queue", "", "node-token")
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
	s, dbURL := schemaTestStore(t, "ui_method_guard")
	bare := httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token"}}.Handler())
	defer bare.Close()
	for _, endpoint := range []string{"/ui", "/ui/queue", "/ui/timeline", "/ui/events", "/ui/static/htmx.min.js"} {
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
	before := uiRowCountsAt(t, dbURL)
	for _, endpoint := range []string{"/ui", "/ui/queue", "/ui/deploys", "/ui/decisions", "/ui/home", "/ui/work"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
			response := uiRequest(t, h.Client(), method, h.URL+endpoint, assertion, "")
			if response.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s status=%d", method, endpoint, response.StatusCode)
			}
			response.Body.Close()
		}
	}
	if after := uiRowCountsAt(t, dbURL); after != before {
		t.Fatalf("read-only method guard changed rows: before=%v after=%v", before, after)
	}
}

type rowCounts struct{ Relay, Tasks, TaskEvents int64 }

// uiLaneRowCounts counts only rows that belong to lane: its tasks, their
// events and relay events owned by it. Tests in the shared schema must not
// compare global counts (AGENTS.md): other packages write there concurrently.
func uiLaneRowCounts(t *testing.T, lane string) rowCounts {
	t.Helper()
	return uiLaneRowCountsAt(t, testdb.URL(t), lane)
}

// uiLaneRowCountsAt is uiLaneRowCounts in the database dbURL names.
func uiLaneRowCountsAt(t *testing.T, dbURL, lane string) rowCounts {
	t.Helper()
	db, err := pgx.Connect(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(t.Context())
	var counts rowCounts
	if err := db.QueryRow(t.Context(), `SELECT (SELECT COUNT(*) FROM relay_events WHERE owner_lane=$1),(SELECT COUNT(*) FROM tasks WHERE lane=$1),(SELECT COUNT(*) FROM task_events e JOIN tasks x ON x.id=e.task_id WHERE x.lane=$1)`, lane).Scan(&counts.Relay, &counts.Tasks, &counts.TaskEvents); err != nil {
		t.Fatal(err)
	}
	return counts
}

// uiRowCountsAt counts whole tables in dbURL. Pass a throwaway schema
// (schemaTestStore) — a global count in the shared schema moves whenever a
// concurrently running package writes a row (AGENTS.md §Tests).
func uiRowCountsAt(t *testing.T, dbURL string) rowCounts {
	t.Helper()
	db, err := pgx.Connect(t.Context(), dbURL)
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

func TestUIFleetProxyAndTokenBoundary(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)

	unconfigured := newUITestServer(t, s, fixture, "", "", 0)
	apiResp := uiRequest(t, unconfigured.Client(), http.MethodGet, unconfigured.URL+"/ui/api/fleet", assertion, "")
	apiBody := responseText(t, apiResp)
	if apiResp.StatusCode != http.StatusOK || !strings.Contains(apiBody, `"upstream":"unconfigured"`) {
		t.Fatalf("unconfigured fleet API=%q", apiBody)
	}
	for _, endpoint := range []string{"/ui/queue", "/ui/decisions"} {
		response := uiRequest(t, unconfigured.Client(), http.MethodGet, unconfigured.URL+endpoint, assertion, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d with hub unset", endpoint, response.StatusCode)
		}
		response.Body.Close()
	}
	unconfigured.Close()

	mode := "normal"
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/nodes" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if mode == "fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"nodes":[{"machine_id":"host-a","state":"connected","accepting":true,"accepting_effective":true,"last_ping_ms":1234,"session_snapshot":{"sessions":[{"pane_id":"pane-1","workspace_id":"ws-1","label":"worker-a","status":"idle","revision":1,"state_change_seq":1}],"snapshot_status":"ok","truncated":false,"received_at":"2026-01-02T03:04:05Z","stale":false}}]}`)
	}))
	defer hub.Close()
	hubValue := "hub-test-value"
	h := newUITestServer(t, s, fixture, hub.URL, hubValue, 0)
	defer h.Close()
	apiResp = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/fleet", assertion, "")
	apiBody = responseText(t, apiResp)
	if apiResp.StatusCode != http.StatusOK || !strings.Contains(apiBody, `"machine_id":"host-a"`) || !strings.Contains(apiBody, `"pane_id":"pane-1"`) || strings.Contains(apiBody, hubValue) {
		t.Fatalf("normal fleet API failed: %q", apiBody)
	}
	mode = "fail"
	apiResp = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/fleet", assertion, "")
	apiBody = responseText(t, apiResp)
	if apiResp.StatusCode != http.StatusOK || !strings.Contains(apiBody, `"machine_id":"host-a"`) || !strings.Contains(apiBody, `"upstream":"ok"`) {
		t.Fatalf("cache window did not keep the last success snapshot: %q", apiBody)
	}

	freshFailure := newUITestServer(t, s, fixture, hub.URL, hubValue, 0)
	defer freshFailure.Close()
	apiResp = uiRequest(t, freshFailure.Client(), http.MethodGet, freshFailure.URL+"/ui/api/fleet", assertion, "")
	apiBody = responseText(t, apiResp)
	if apiResp.StatusCode != http.StatusOK || !strings.Contains(apiBody, `"upstream":"http_error"`) || strings.Contains(apiBody, `"state":"down"`) {
		t.Fatalf("initial hub failure was not available without marking nodes down: %q", apiBody)
	}
}

func TestUIFleetThresholdsAndNullableMemory(t *testing.T) {
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"nodes":[{"machine_id":"host-a","state":"connected","accepting":true,"accepting_effective":false,"accepting_override":"maintenance","session_snapshot":{"sessions":[],"snapshot_status":"ok","truncated":false,"received_at":"2026-01-02T03:04:05Z","stale":false}},{"machine_id":"host-b","state":"connected","session_snapshot":{"sessions":[],"snapshot_status":"ok","truncated":false,"received_at":"2026-01-02T03:04:05Z","stale":false}},{"machine_id":"host-c","state":"connected","memory":null,"session_snapshot":{"sessions":[],"snapshot_status":"unavailable","truncated":false,"received_at":"2026-01-02T03:04:05Z","stale":false}},{"machine_id":"host-d","state":"connected","memory":{"free_pct":null},"session_snapshot":{"sessions":[{"pane_id":"pane-1","workspace_id":"ws-1","label":"worker-a","status":"idle","revision":1,"state_change_seq":1}],"snapshot_status":"ok","truncated":false,"received_at":"2026-01-02T03:04:05Z","stale":false}}]}`)
	}))
	defer hub.Close()
	h := newUITestServer(t, s, fixture, hub.URL, "hub-test-value", 0)
	defer h.Close()
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/fleet", assertion, "")
	body := responseText(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("fleet API status=%d body=%q", response.StatusCode, body)
	}
	for _, id := range []string{"host-a", "host-b", "host-c", "host-d"} {
		if !strings.Contains(body, `"machine_id":"`+id+`"`) {
			t.Fatalf("missing %s in %q", id, body)
		}
	}
	if !strings.Contains(body, `"snapshot_status":"ok"`) || !strings.Contains(body, `"snapshot_status":"unavailable"`) {
		t.Fatal("empty-list and collection-failure statuses were not preserved together")
	}
	if strings.Contains(body, `"state":"down"`) {
		t.Fatal("nullable memory nodes were marked down")
	}
}
