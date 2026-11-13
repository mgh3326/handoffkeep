package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// Server-rendered forms carry the token in name="csrf"; the React shell
	// carries it in <meta name="hk-csrf">. Accept either.
	field, metaField := `name="csrf" value="`, `<meta name="hk-csrf" content="`
	start := strings.Index(body, field)
	if start >= 0 {
		start += len(field)
	} else if start = strings.Index(body, metaField); start >= 0 {
		start += len(metaField)
	} else {
		t.Fatalf("form GET %s did not render CSRF field: %q", endpoint, body)
	}
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

func TestUIWriteNeverLeaksHubToken(t *testing.T) {
	t.Setenv("HANDOFFKEEP_UI_LANES", "lane-a")
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	const token = "hub-test-token"
	h := newUITestServer(t, s, fixture, hub.server.URL, token, 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	for _, endpoint := range []string{"/ui/queue", "/ui/deploys", "/ui/decisions"} {
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
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/glance", assertion, "")
	if body := responseText(t, response); strings.Contains(body, token) {
		t.Fatal("token leaked in glance failure")
	}
}

func TestUIDocIngressBadgeAndApproval(t *testing.T) {
	t.Setenv("HANDOFFKEEP_UI_LANES", "lane-a")
	s, dbURL := schemaTestStore(t, "ui_doc_ingress")
	fixture := newUIJWTFixture(t)
	hub := newFakeIngressHub(t)
	h := newUITestServer(t, s, fixture, hub.server.URL, "hub-test-token", 0)
	defer h.Close()
	assertion := fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
	if _, _, err := s.PutDocument(t.Context(), store.Document{Key: "reports/x/y.md", Kind: "report", Body: "<report body>", CreatedBy: "test-node"}); err != nil {
		t.Fatal(err)
	}

	// /ui/doc/{key} is the shell page; the browser reads the document body
	// through the JSON route, so the page itself carries no document text.
	response := uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/doc/reports/x/y.md", assertion, "")
	if page := responseText(t, response); response.StatusCode != http.StatusOK || !strings.Contains(page, `id="shell-root"`) || strings.Contains(page, "report body") {
		t.Fatalf("document page did not mount the shell without the body: status=%d", response.StatusCode)
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/board/doc?key=reports/x/y.md", assertion, "")
	if response.StatusCode != http.StatusOK || !strings.Contains(responseText(t, response), "report body") {
		t.Fatal("document JSON did not carry the body")
	}
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/api/board/doc?key=missing.md", assertion, "")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing document status=%d", response.StatusCode)
	}
	response.Body.Close()
	response = uiRequest(t, h.Client(), http.MethodGet, h.URL+"/ui/doc/reports/../secret", assertion, "")
	if response.StatusCode != http.StatusBadRequest && response.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid document status=%d", response.StatusCode)
	}
	response.Body.Close()

	// Every UI route stays GET-only for writes: unknown paths refuse other methods.
	before := uiRowCountsAt(t, dbURL)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		response = uiRequest(t, h.Client(), method, h.URL+"/ui/not-a-write-route", assertion, "")
		if response.StatusCode != http.StatusMethodNotAllowed || response.Header.Get("Allow") != http.MethodGet {
			t.Fatalf("method guard %s status=%d allow=%q", method, response.StatusCode, response.Header.Get("Allow"))
		}
		response.Body.Close()
	}
	if after := uiRowCountsAt(t, dbURL); after != before {
		t.Fatalf("GET/method guard wrote rows: before=%+v after=%+v", before, after)
	}
}
