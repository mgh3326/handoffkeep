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
