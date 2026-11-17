package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/capabilities"
)

// A decision-path answer carries no target, so a server that predates the
// plain-question shape must still get the answer: the lane is omitted there
// instead of being sent as an unknown field (hk 5000).
func TestAnswerDotsOmitsRequesterLaneWhenUnlisted(t *testing.T) {
	resetCapabilitiesCache()
	var listed []string
	for _, f := range capabilities.Features {
		if f != capabilities.FeatureDotsAnswerQuestion {
			listed = append(listed, f)
		}
	}
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/capabilities" {
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_version": capabilities.ProtocolVersion, "features": listed})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"task_id":7,"comment_id":3,"duplicate":false}`))
	}))
	defer server.Close()

	if _, err := (Client{URL: server.URL, Token: "t"}).AnswerDots(context.Background(), 7, "answer", "a-lane", 0); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["requester_lane"]; ok {
		t.Fatalf("requester_lane reached a server that does not list it: %v", got)
	}
	if got["text"] != "answer" {
		t.Fatalf("answer body=%v", got)
	}
}

// A new server lists the shape, so the lane and the target ride it.
func TestAnswerDotsSendsQuestionFieldsWhenListed(t *testing.T) {
	resetCapabilitiesCache()
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/capabilities" {
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol_version": capabilities.ProtocolVersion, "features": capabilities.Features})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"task_id":7,"comment_id":3,"duplicate":false}`))
	}))
	defer server.Close()

	if _, err := (Client{URL: server.URL, Token: "t"}).AnswerDots(context.Background(), 7, "answer", "a-lane", 42); err != nil {
		t.Fatal(err)
	}
	if got["requester_lane"] != "a-lane" || got["question_id"] != float64(42) {
		t.Fatalf("answer body=%v", got)
	}
}

// A failing capabilities read must not fail a decision-path answer: the lane
// is optional there, so it is omitted and the answer goes out (hk 5000).
func TestAnswerDotsSurvivesCapabilitiesFailure(t *testing.T) {
	resetCapabilitiesCache()
	var got map[string]any
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/capabilities" {
			reads.Add(1)
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"task_id":7,"comment_id":3,"duplicate":false}`))
	}))
	defer server.Close()

	if _, err := (Client{URL: server.URL, Token: "t"}).AnswerDots(context.Background(), 7, "answer", "a-lane", 0); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 1 {
		t.Fatalf("capabilities reads=%d, want 1", reads.Load())
	}
	if _, ok := got["requester_lane"]; ok {
		t.Fatalf("requester_lane sent after a failed read: %v", got)
	}
}
