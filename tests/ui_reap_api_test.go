package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// reapHubFixture serves GET /v1/session-reap from the body the panewire hub
// itself produced for its #603 fixture (internal/ui/testdata), with the job
// ids suffixed per run so rows in the shared test database cannot collide.
type reapHubFixture struct {
	body   []byte
	status atomic.Int32
	calls  atomic.Int32
}

func (f *reapHubFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+fleetTestSecret {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path != "/v1/session-reap" || r.Method != http.MethodGet {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.calls.Add(1)
	if status := int(f.status.Load()); status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(f.body)
}

func newReapHubFixture(t *testing.T, suffix string) *reapHubFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "internal", "ui", "testdata", "panewire-session-reap.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.ReplaceAll(string(raw), `"job_id": "`, `"job_id": "`+suffix+`-`)
	body = strings.ReplaceAll(body, `"job_id":"`, `"job_id":"`+suffix+`-`)
	return &reapHubFixture{body: []byte(body)}
}

func getReapAPI(t *testing.T, server *httptest.Server, assertion string) (int, string, map[string]any) {
	t.Helper()
	response := uiRequest(t, server.Client(), http.MethodGet, server.URL+"/ui/api/reap", assertion, "")
	body := responseText(t, response)
	var decoded map[string]any
	if response.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Fatalf("decode reap JSON: %v body=%q", err, body)
		}
	}
	return response.StatusCode, body, decoded
}

func reapRowsByPane(t *testing.T, decoded map[string]any, key string) map[string]map[string]any {
	t.Helper()
	raw, ok := decoded[key].([]any)
	if !ok {
		t.Fatalf("missing %s in %v", key, decoded)
	}
	out := map[string]map[string]any{}
	for _, item := range raw {
		row := item.(map[string]any)
		out[row["pane_id"].(string)] = row
	}
	return out
}

func TestReapAPIJoinsHubReportWithTasks(t *testing.T) {
	s := uiStore(t)
	suffix := fmt.Sprintf("r%d", time.Now().UnixNano())
	hub := newReapHubFixture(t, suffix)
	server := httptest.NewServer(hub)
	defer server.Close()
	ui, assertion := newFleetUI(t, server.URL, fleetTestSecret, 10*time.Second)

	lane := uiLane(t, "reap")
	merged := createJobTask(t, s, lane, "merged builder task", suffix+"-599-merged-builder-20260923")
	claimAndTransition(t, s, merged, "in_progress", "built")
	for _, to := range []string{"verifying", "merged"} {
		if _, err := s.TransitionTask(t.Context(), merged.ID, to, "test-node", to, nil); err != nil {
			t.Fatal(err)
		}
	}
	open := createJobTask(t, s, lane, "open builder task", suffix+"-529-deploy-view-20260923-1535")
	claimAndTransition(t, s, open, "in_progress", "building")

	status, body, decoded := getReapAPI(t, ui, assertion)
	if status != http.StatusOK || decoded["status"] != "ok" {
		t.Fatalf("status=%d body=%s", status, body)
	}
	assertNoSecret(t, body, http.Header{}, fleetTestSecret, server.URL)
	candidates := reapRowsByPane(t, decoded, "candidates")
	held := reapRowsByPane(t, decoded, "held")
	if len(candidates) != 1 || candidates["w2:p25"]["basis"] != "job-terminal" {
		t.Fatalf("candidates = %v", candidates)
	}
	// Merged just now: linked and terminal, but still inside the node's
	// 600s grace, so the builder is held with its task named.
	if row := held["w2:p32"]; row["reason"] != "task-within-grace" || int64(row["task_id"].(float64)) != merged.ID || row["task_state"] != "merged" {
		t.Fatalf("merged builder row = %v", row)
	}
	if row := held["w2:p3"]; row["reason"] != "task-open" || int64(row["task_id"].(float64)) != open.ID {
		t.Fatalf("open builder row = %v", row)
	}
	if row := held["w2:p26"]; row["reason"] != "protected" {
		t.Fatalf("protected row = %v", row)
	}
	if len(held) != 13 {
		t.Fatalf("held rows = %d", len(held))
	}
}

// A hub from before #603 answers 404: the console says unsupported and lists
// nothing, rather than an empty candidate list that reads as "all clear".
func TestReapAPIOldHubAndFailures(t *testing.T) {
	uiStore(t)
	hub := newReapHubFixture(t, "old")
	server := httptest.NewServer(hub)
	defer server.Close()
	ui, assertion := newFleetUI(t, server.URL, fleetTestSecret, 10*time.Second)
	for status, want := range map[int32]string{http.StatusNotFound: "unsupported", http.StatusMethodNotAllowed: "unsupported", http.StatusUnauthorized: "auth_failed", http.StatusBadGateway: "http_error"} {
		hub.status.Store(status)
		code, body, decoded := getReapAPI(t, ui, assertion)
		if code != http.StatusOK || decoded["status"] != want {
			t.Fatalf("hub %d: code=%d body=%s", status, code, body)
		}
		if len(decoded["candidates"].([]any)) != 0 || decoded["fetched_at"] != "" {
			t.Fatalf("hub %d served candidates: %s", status, body)
		}
	}
	// Unconfigured hub: no request leaves the console.
	before := hub.calls.Load()
	bare, bareAssertion := newFleetUI(t, "", "", 10*time.Second)
	if code, body, decoded := getReapAPI(t, bare, bareAssertion); code != http.StatusOK || decoded["status"] != "unconfigured" {
		t.Fatalf("unconfigured: %d %s", code, body)
	}
	if hub.calls.Load() != before {
		t.Fatal("unconfigured console contacted a hub")
	}
	// Unauthenticated browsers never reach the endpoint.
	response := uiRequest(t, ui.Client(), http.MethodGet, ui.URL+"/ui/api/reap", "", "")
	_ = responseText(t, response)
	if response.StatusCode == http.StatusOK {
		t.Fatal("reap API served without an access assertion")
	}
}
