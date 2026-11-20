package remote

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/capabilities"
	"github.com/mgh3326/handoffkeep/internal/store"
)

// resolveBodies serves capabilities (with features) and the resolve route, and
// records each resolve body, so a test can see whether recording_lane was sent.
func resolveBodies(t *testing.T, features []string) (Client, func() []string) {
	t.Helper()
	resetCapabilitiesCache()
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerCurrentCapabilitiesWith(w, r, features) {
			return
		}
		if strings.HasSuffix(r.URL.Path, "/decision-request/resolve") {
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			bodies = append(bodies, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	return Client{URL: server.URL, Token: "t"}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// answerCurrentCapabilitiesWith answers GET /v1/capabilities with features.
func answerCurrentCapabilitiesWith(w http.ResponseWriter, r *http.Request, features []string) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/capabilities" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"protocol_version": capabilities.ProtocolVersion, "features": features})
	return true
}

// Invariant (hk 5527): recording_lane is sent only to a server that lists
// FeatureDecisionRecordingLane. An older server gets the answer without the
// field, so the answer is still recorded.
func TestResolveDecisionChannelOmitsRecordingLaneOnOlderServer(t *testing.T) {
	in := DecisionChannelResolveInput{
		DecisionResolveInput: store.DecisionResolveInput{RequestID: "dr-1-1", Kind: store.DecisionRequestAnswered, Option: "A", RecordingLane: "asker-lane"},
		Channel:              store.DecisionChannelSession,
	}

	older, olderBodies := resolveBodies(t, []string{capabilities.FeatureClaimJobID})
	if _, err := older.ResolveDecisionChannel(context.Background(), 1, in); err != nil {
		t.Fatalf("older server: %v", err)
	}
	if body := olderBodies(); len(body) != 1 || strings.Contains(body[0], "recording_lane") {
		t.Fatalf("older server bodies=%v, want one body without recording_lane", body)
	}

	current, currentBodies := resolveBodies(t, capabilities.Features)
	if _, err := current.ResolveDecisionChannel(context.Background(), 1, in); err != nil {
		t.Fatalf("current server: %v", err)
	}
	if body := currentBodies(); len(body) != 1 || !strings.Contains(body[0], `"recording_lane":"asker-lane"`) {
		t.Fatalf("current server bodies=%v, want recording_lane sent", body)
	}
}
