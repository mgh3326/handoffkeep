package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/cfaccess"
	"github.com/mgh3326/handoffkeep/internal/ui"
)

const fleetTestSecret = "test-secret-XYZ"

type fleetHub struct {
	mu        sync.Mutex
	paths     []string
	nodesHits int
	serve     func(http.ResponseWriter, *http.Request)
}

func (h *fleetHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.paths = append(h.paths, r.Method+" "+r.URL.Path)
	if r.URL.Path == "/v1/nodes" {
		h.nodesHits++
	}
	serve := h.serve
	h.mu.Unlock()
	if serve != nil {
		serve(w, r)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (h *fleetHub) setServe(fn func(http.ResponseWriter, *http.Request)) {
	h.mu.Lock()
	h.serve = fn
	h.mu.Unlock()
}

func (h *fleetHub) snapshot() (int, []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nodesHits, append([]string(nil), h.paths...)
}

func newFleetHub(t *testing.T, serve func(http.ResponseWriter, *http.Request)) *fleetHub {
	t.Helper()
	hub := &fleetHub{serve: serve}
	return hub
}

func startFleetHub(t *testing.T, hub *fleetHub) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(hub)
	t.Cleanup(server.Close)
	return server
}

func newFleetUI(t *testing.T, hubURL, hubToken string, cacheTTL time.Duration) (*httptest.Server, string) {
	t.Helper()
	s := uiStore(t)
	fixture := newUIJWTFixture(t)
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
	handler, err := ui.New(ui.Config{
		Store:       s,
		Access:      access,
		HubURL:      hubURL,
		HubToken:    hubToken,
		HubCacheTTL: cacheTTL,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Server{Service: api.Service{Store: s}, Tokens: api.Tokens{"node": "node-token"}, UI: handler}.Handler())
	t.Cleanup(server.Close)
	return server, fixture.token(t, "admin@example.com", "ui-audience", time.Now().Add(time.Hour), nil)
}

func writeFleetNodes(w http.ResponseWriter, nodes []map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"nodes": nodes})
}

func readyTrue() *bool {
	v := true
	return &v
}

func sessionSnapshot(status string, truncated, stale bool, receivedAt string, sessions []map[string]any) map[string]any {
	if sessions == nil {
		sessions = []map[string]any{}
	}
	return map[string]any{
		"sessions":        sessions,
		"snapshot_status": status,
		"truncated":       truncated,
		"received_at":     receivedAt,
		"stale":           stale,
	}
}

func syntheticSession(pane, label, status string, ready *bool) map[string]any {
	session := map[string]any{
		"pane_id":          pane,
		"workspace_id":     "ws-1",
		"label":            label,
		"status":           status,
		"revision":         int64(1),
		"state_change_seq": int64(1),
	}
	if ready != nil {
		session["interactive_ready"] = *ready
	}
	return session
}

func fleetNode(machine, state string, lastPing *int64, snap map[string]any) map[string]any {
	node := map[string]any{
		"machine_id":          machine,
		"state":               state,
		"accepting":           true,
		"accepting_effective": true,
		"accepting_override":  "",
		"session_snapshot":    snap,
	}
	if lastPing != nil {
		node["last_ping_ms"] = *lastPing
	}
	return node
}

func getFleetAPI(t *testing.T, server *httptest.Server, assertion string) (int, http.Header, string, map[string]any) {
	t.Helper()
	response := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/api/fleet", assertion, "")
	header := response.Header.Clone()
	body := responseText(t, response)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode fleet JSON: %v body=%q", err, body)
	}
	return response.StatusCode, header, body, decoded
}

func fleetNodes(t *testing.T, decoded map[string]any) []map[string]any {
	t.Helper()
	raw, _ := decoded["nodes"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		node, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("node type %T", item)
		}
		out = append(out, node)
	}
	return out
}

func assertNoSecret(t *testing.T, body string, header http.Header, secret, hubURL string) {
	t.Helper()
	if strings.Contains(body, secret) {
		t.Fatalf("hub token leaked in body: %q", body)
	}
	if hubURL != "" && strings.Contains(body, hubURL) {
		t.Fatalf("hub URL leaked in body: %q", body)
	}
	for key, values := range header {
		joined := strings.Join(values, ",")
		if strings.Contains(joined, secret) || (hubURL != "" && strings.Contains(joined, hubURL)) {
			t.Fatalf("secret leaked in header %s: %q", key, joined)
		}
	}
}

func TestFleetAPIHealthySessions(t *testing.T) {
	ping := int64(1234)
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/nodes" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "connected", &ping, sessionSnapshot("ok", false, false, "2026-01-02T03:04:05Z", []map[string]any{
				syntheticSession("pane-1", "worker-a", "idle", readyTrue()),
			})),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 0)
	status, header, body, got := getFleetAPI(t, server, assertion)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%q", status, body)
	}
	assertNoSecret(t, body, header, fleetTestSecret, upstream.URL)
	if got["upstream"] != "ok" || got["fetched_at"] == "" {
		t.Fatalf("envelope=%v", got)
	}
	nodes := fleetNodes(t, got)
	if len(nodes) != 1 {
		t.Fatalf("nodes=%v", nodes)
	}
	node := nodes[0]
	if node["machine_id"] != "node-a" || node["state"] != "connected" || node["snapshot_status"] != "ok" || node["truncated"] != false || node["stale"] != false {
		t.Fatalf("node=%v", node)
	}
	if node["last_ping"] != float64(1234) || node["received_at"] != "2026-01-02T03:04:05Z" {
		t.Fatalf("timing fields=%v", node)
	}
	sessions, _ := node["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions=%v", sessions)
	}
	session := sessions[0].(map[string]any)
	if session["pane_id"] != "pane-1" || session["workspace_id"] != "ws-1" || session["label"] != "worker-a" || session["status"] != "idle" || session["interactive_ready"] != true || session["model"] != "미수집" {
		t.Fatalf("session=%v", session)
	}
	hits, paths := hub.snapshot()
	if hits != 1 || strings.Join(paths, ",") != "GET /v1/nodes" {
		t.Fatalf("hub traffic hits=%d paths=%v", hits, paths)
	}
}

func TestFleetAPIEmptySessionsNotUnavailable(t *testing.T) {
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "connected", nil, sessionSnapshot("ok", false, false, "2026-01-02T03:04:05Z", []map[string]any{})),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 0)
	_, _, body, got := getFleetAPI(t, server, assertion)
	node := fleetNodes(t, got)[0]
	sessions, _ := node["sessions"].([]any)
	if node["snapshot_status"] != "ok" || len(sessions) != 0 {
		t.Fatalf("empty list collapsed: %v body=%q", node, body)
	}
	if node["snapshot_status"] == "unavailable" {
		t.Fatal("empty sessions marked unavailable")
	}
}

func TestFleetAPISnapshotUnavailable(t *testing.T) {
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "connected", nil, sessionSnapshot("unavailable", false, false, "2026-01-02T03:04:05Z", []map[string]any{})),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 0)
	_, _, _, got := getFleetAPI(t, server, assertion)
	node := fleetNodes(t, got)[0]
	if node["snapshot_status"] != "unavailable" {
		t.Fatalf("collection failure not preserved: %v", node)
	}
	if node["state"] != "connected" {
		t.Fatalf("collection failure marked the node down: %v", node)
	}
}

func TestFleetAPITruncated64(t *testing.T) {
	sessions := make([]map[string]any, 64)
	for i := 0; i < 64; i++ {
		sessions[i] = syntheticSession(fmt.Sprintf("pane-%d", i+1), fmt.Sprintf("session-%d", i+1), "idle", nil)
	}
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "connected", nil, sessionSnapshot("ok", true, false, "2026-01-02T03:04:05Z", sessions)),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 0)
	_, _, body, got := getFleetAPI(t, server, assertion)
	node := fleetNodes(t, got)[0]
	gotSessions, _ := node["sessions"].([]any)
	if node["truncated"] != true || len(gotSessions) != 64 || node["snapshot_status"] != "ok" {
		t.Fatalf("truncation projection=%v", node)
	}
	if _, ok := node["ended_sessions"]; ok {
		t.Fatal("sessions outside the 64-cap were judged ended")
	}
	for _, item := range gotSessions {
		session := item.(map[string]any)
		if session["status"] == "ended" || session["status"] == "terminated" {
			t.Fatalf("truncated list invented an ended session: %v", session)
		}
	}
	if strings.Contains(body, `"ended"`) && strings.Contains(body, "pane-65") {
		t.Fatal("missing pane judged ended")
	}
}

func TestFleetAPINodeStalePreservesReceivedAt(t *testing.T) {
	const receivedAt = "2026-01-02T03:04:05Z"
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "stale", nil, sessionSnapshot("ok", false, true, receivedAt, []map[string]any{
				syntheticSession("pane-1", "worker-a", "idle", nil),
			})),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 0)
	_, _, _, got := getFleetAPI(t, server, assertion)
	node := fleetNodes(t, got)[0]
	if node["stale"] != true || node["state"] != "stale" || node["received_at"] != receivedAt {
		t.Fatalf("stale received_at rewritten: %v", node)
	}
}

func TestFleetAPIHubTimeoutKeepsLastSuccess(t *testing.T) {
	const receivedAt = "2026-01-02T03:04:05Z"
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "connected", nil, sessionSnapshot("ok", false, false, receivedAt, []map[string]any{
				syntheticSession("pane-1", "worker-a", "idle", nil),
			})),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 40*time.Millisecond)
	_, _, _, first := getFleetAPI(t, server, assertion)
	fetchedAt, _ := first["fetched_at"].(string)
	if first["upstream"] != "ok" || fetchedAt == "" {
		t.Fatalf("first fetch=%v", first)
	}
	hub.setServe(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(4 * time.Second)
		w.WriteHeader(http.StatusOK)
	})
	time.Sleep(80 * time.Millisecond)
	status, _, body, second := getFleetAPI(t, server, assertion)
	if status != http.StatusOK || second["upstream"] != "timeout" {
		t.Fatalf("timeout envelope status=%d body=%q", status, body)
	}
	if second["fetched_at"] != fetchedAt {
		t.Fatalf("fetched_at changed after timeout: first=%q second=%q", fetchedAt, second["fetched_at"])
	}
	node := fleetNodes(t, second)[0]
	if node["machine_id"] != "node-a" || node["received_at"] != receivedAt || node["snapshot_status"] != "ok" {
		t.Fatalf("timeout dropped last success: %v", node)
	}
}

func TestFleetAPIAuthFailedDoesNotMarkNodesDown(t *testing.T) {
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "connected", nil, sessionSnapshot("ok", false, false, "2026-01-02T03:04:05Z", []map[string]any{
				syntheticSession("pane-1", "worker-a", "idle", nil),
			})),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 40*time.Millisecond)
	_, _, _, first := getFleetAPI(t, server, assertion)
	fetchedAt := first["fetched_at"]
	hub.setServe(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	time.Sleep(80 * time.Millisecond)
	_, _, body, second := getFleetAPI(t, server, assertion)
	if second["upstream"] != "auth_failed" {
		t.Fatalf("auth failure envelope=%v body=%q", second, body)
	}
	if second["fetched_at"] != fetchedAt {
		t.Fatalf("fetched_at changed after auth failure")
	}
	node := fleetNodes(t, second)[0]
	if node["state"] != "connected" || node["machine_id"] != "node-a" {
		t.Fatalf("auth failure displayed as node down: %v", node)
	}
	if node["state"] == "down" || node["state"] == "disconnected" {
		t.Fatalf("auth failure rewrote node state: %v", node)
	}
}

func TestFleetAPICSPHeader(t *testing.T) {
	server, assertion := newFleetUI(t, "", "", 0)
	apiResp := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/api/fleet", assertion, "")
	apiCSP := apiResp.Header.Get("Content-Security-Policy")
	_ = responseText(t, apiResp)
	pageResp := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/fleet", assertion, "")
	pageCSP := pageResp.Header.Get("Content-Security-Policy")
	pageBody := responseText(t, pageResp)
	if apiCSP != ui.ConsoleCSP || pageCSP != ui.ConsoleCSP {
		t.Fatalf("csp api=%q page=%q", apiCSP, pageCSP)
	}
	if !strings.Contains(pageBody, `id="fleet-root"`) || !strings.Contains(pageBody, `/ui/static/console/fleet.js`) {
		t.Fatalf("fleet page mount missing: %q", pageBody)
	}
	timeline := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/timeline", assertion, "")
	if timeline.Header.Get("Content-Security-Policy") != "" {
		t.Fatal("CSP applied to an htmx page")
	}
	_ = responseText(t, timeline)
}

func TestFleetAPISecretNotExposed(t *testing.T) {
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "connected", nil, sessionSnapshot("ok", false, false, "2026-01-02T03:04:05Z", []map[string]any{
				syntheticSession("pane-1", "worker-a", "idle", nil),
			})),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 0)
	status, header, body, got := getFleetAPI(t, server, assertion)
	if status != http.StatusOK || got["token"] != nil || got["hub_token"] != nil || got["hub_url"] != nil {
		t.Fatalf("secret fields present: %v", got)
	}
	assertNoSecret(t, body, header, fleetTestSecret, upstream.URL)
	page := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/fleet", assertion, "")
	assertNoSecret(t, responseText(t, page), page.Header, fleetTestSecret, upstream.URL)
}

func TestFleetAPINoExtraNotifications(t *testing.T) {
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "connected", nil, sessionSnapshot("ok", false, false, "2026-01-02T03:04:05Z", []map[string]any{
				syntheticSession("pane-1", "worker-a", "idle", nil),
			})),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 0)
	_, _, _, _ = getFleetAPI(t, server, assertion)
	hits, paths := hub.snapshot()
	if hits != 1 {
		t.Fatalf("nodes hits=%d", hits)
	}
	for _, path := range paths {
		if path != "GET /v1/nodes" && path != "GET /v1/jobs" {
			t.Fatalf("unexpected hub path %q in %v", path, paths)
		}
	}
	if len(paths) != 1 || paths[0] != "GET /v1/nodes" {
		t.Fatalf("fleet path called extra hub routes: %v", paths)
	}
}

func TestFleetAPISharedPolling(t *testing.T) {
	hub := newFleetHub(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(120 * time.Millisecond)
		writeFleetNodes(w, []map[string]any{
			fleetNode("node-a", "connected", nil, sessionSnapshot("ok", false, false, "2026-01-02T03:04:05Z", []map[string]any{
				syntheticSession("pane-1", "worker-a", "idle", nil),
			})),
		})
	})
	upstream := startFleetHub(t, hub)
	server, assertion := newFleetUI(t, upstream.URL, fleetTestSecret, 10*time.Second)
	warmup := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/fleet", assertion, "")
	if warmup.StatusCode != http.StatusOK {
		t.Fatalf("warmup status=%d", warmup.StatusCode)
	}
	_ = responseText(t, warmup)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/ui/api/fleet", nil)
			if err != nil {
				t.Error(err)
				return
			}
			req.Header.Set("Cf-Access-Jwt-Assertion", assertion)
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status=%d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	hits, _ := hub.snapshot()
	if hits != 1 {
		t.Fatalf("concurrent polling made %d upstream GETs, want 1", hits)
	}
	_, _, _, _ = getFleetAPI(t, server, assertion)
	hits, paths := hub.snapshot()
	if hits != 1 {
		t.Fatalf("cached replay made extra upstream GETs: hits=%d paths=%v", hits, paths)
	}
}

func TestFleetAPIUnauthorizedSameAsUI(t *testing.T) {
	server, _ := newFleetUI(t, "", "", 0)
	fleet := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/api/fleet", "", "")
	timeline := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/timeline", "", "")
	fleetBody := responseText(t, fleet)
	timelineBody := responseText(t, timeline)
	if fleet.StatusCode != http.StatusUnauthorized || timeline.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status fleet=%d timeline=%d", fleet.StatusCode, timeline.StatusCode)
	}
	if fleetBody != "" || timelineBody != "" {
		t.Fatalf("bodies fleet=%q timeline=%q", fleetBody, timelineBody)
	}
	apiOK := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/v1/tasks", "", "node-token")
	if apiOK.StatusCode != http.StatusOK {
		t.Fatalf("/v1/tasks status=%d", apiOK.StatusCode)
	}
	apiOK.Body.Close()
}
