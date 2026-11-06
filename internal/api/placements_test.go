package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestPlacementNodeCredentialAndDecode(t *testing.T) {
	s := Server{Tokens: Tokens{"node": "node-token", "operator": "operator-token"}, HostGrants: HostGrants{"node": {"host-a": true}, "operator": {"host-a": true, "host-b": true}}}
	h := s.Handler()
	for _, tc := range []struct {
		token, body string
		want        int
	}{{"", "{}", http.StatusUnauthorized}, {"operator-token", "{}", http.StatusForbidden}, {"node-token", `{"unknown":true}`, http.StatusBadRequest}, {"node-token", `{} {}`, http.StatusBadRequest}, {"node-token", strings.Repeat("x", (64<<10)+1), http.StatusBadRequest}} {
		req := httptest.NewRequest(http.MethodPost, "/v1/placements", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
		}
	}
}

func TestPlacementEndpointPersistsNodeIdentityAndRetry(t *testing.T) {
	_, url := testdb.Schema(t, "placement_api")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	srv := httptest.NewServer(Server{Service: Service{Store: st}, Tokens: Tokens{"node": "node-token"}, HostGrants: HostGrants{"node": {"host-a": true}}}.Handler())
	t.Cleanup(srv.Close)
	body := map[string]any{"job_id": "refused-job", "attempt": 1, "decided_at": time.Now().UTC().Truncate(time.Microsecond), "decided_on": "spoof", "source": "forced", "ranking": "forced", "state_source": "direct", "outcome": "refused", "reason": "unavailable", "candidates": []any{}}
	data, _ := json.Marshal(body)
	status, response := jobsRequest(t, http.MethodPost, srv.URL+"/v1/placements", "node-token", string(data))
	if status != http.StatusCreated || response["decided_on"] != "host-a" {
		t.Fatal(status, response)
	}
	status, _ = jobsRequest(t, http.MethodPost, srv.URL+"/v1/placements", "node-token", string(data))
	if status != http.StatusOK {
		t.Fatal(status)
	}
	body["reason"] = "different"
	data, _ = json.Marshal(body)
	status, response = jobsRequest(t, http.MethodPost, srv.URL+"/v1/placements", "node-token", string(data))
	if status != http.StatusConflict || response["error"] != "placement_conflict" {
		t.Fatal(status, response)
	}
	status, response = jobsRequest(t, http.MethodGet, srv.URL+"/v1/placements?job=refused-job&limit=1", "node-token", "")
	if status != http.StatusOK || len(response["placements"].([]any)) != 1 {
		t.Fatal(status, response)
	}
}

// panewireCandidate is a candidates[] item in panewire's PlacementSignals shape.
const panewireCandidate = `{"host":"node-a","outcome":"ok","reason":"","score":0.42,"signal_age_s":1.5,"cpu_busy_short":0.1,"cpu_busy_5m":0.2,"mem_available":0.6,"swap_in_mbps":0,"power_source":"ac","bridged_jobs":1,"active_jobs":2,"tasks":3,"task_slots":4,"power":12.5,"live_jobs":3,"load1_per_core":0.75,"terms":{"cpu":0.1,"mem":0.2,"io":0.3,"load":0.4,"jobs":0.5}}`

func placementBody(candidate string) string {
	return `{"job_id":"job-v2","attempt":1,"decided_at":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","source":"auto","ranking":"score","state_source":"hub","outcome":"refused","candidates":[` + candidate + `]}`
}

func TestPlacementDecodesScoreV2Candidate(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/placements", strings.NewReader(placementBody(panewireCandidate)))
	var in store.PlacementDecision
	if err := decode(req, &in, bodyLarge); err != nil {
		t.Fatal(err)
	}
	c := in.Candidates[0]
	if c.LiveJobs == nil || *c.LiveJobs != 3 || c.Load1PerCore == nil || *c.Load1PerCore != 0.75 || c.Terms == nil || c.Terms.Jobs != 0.5 || c.Terms.CPU != 0.1 {
		t.Fatalf("score-v2 fields lost: %+v", c)
	}
}

// TestPlacementScoreV2StatusAndReason drives the handler without a database:
// every case is refused before the (absent) store is touched.
func TestPlacementScoreV2StatusAndReason(t *testing.T) {
	s := Server{Tokens: Tokens{"node": "node-token"}, HostGrants: HostGrants{"node": {"host-a": true}}}
	h := s.Handler()
	cand := func(extra string) string { return `{"host":"node-a","outcome":"ok",` + extra + `}` }
	for _, tc := range []struct {
		name, candidate, reason string
	}{
		{"negative live_jobs", cand(`"live_jobs":-1`), "candidates[0].live_jobs"},
		{"negative load1_per_core", cand(`"load1_per_core":-0.5`), "candidates[0].load1_per_core"},
		{"terms below 0", cand(`"terms":{"cpu":-0.1,"mem":0,"io":0,"load":0,"jobs":0}`), "candidates[0].terms.cpu"},
		{"terms above 1", cand(`"terms":{"cpu":0,"mem":0,"io":0,"load":1.01,"jobs":0}`), "candidates[0].terms.load"},
		{"unknown candidate field", cand(`"live_job":1`), ""},
		{"unknown terms field", cand(`"terms":{"cpu":0,"mem":0,"io":0,"load":0,"jobs":0,"gpu":0}`), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/placements", strings.NewReader(placementBody(tc.candidate)))
			req.Header.Set("Authorization", "Bearer node-token")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			var out map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			if w.Code != http.StatusBadRequest || out["error"] != "invalid_context" {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if reason, _ := out["reason"].(string); tc.reason != "" && !strings.Contains(reason, tc.reason) {
				t.Fatalf("reason=%q want %q", reason, tc.reason)
			}
		})
	}
}
