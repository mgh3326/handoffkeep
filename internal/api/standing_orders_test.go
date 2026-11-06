package api

import (
	"github.com/mgh3326/handoffkeep/internal/store"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestStandingOrdersAPI(t *testing.T) {
	h, st := changesServer(t)
	key := "decision/standing-api"
	if _, _, err := st.PutDocument(t.Context(), store.Document{Key: key, Kind: "note", Body: "fake", CreatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
	in := store.StandingOrderInput{ScopeKind: "lane", Scope: "api-lane", TopicKey: "deploy", Statement: "A", SourceDoc: key, DecidedBy: "operator", DecidedAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Second)}
	if status, body := changesCall(t, h, http.MethodPost, "/v1/standing-orders", "peer-token", in); status != 403 || body["error"] != "operator_required" {
		t.Fatalf("peer: %d %v", status, body)
	}
	if status, body := changesCall(t, h, http.MethodGet, "/v1/standing-orders", "peer-token", nil); status != 400 || body["error"] != "invalid_context" {
		t.Fatalf("missing: %d %v", status, body)
	}
	if status, body := changesCall(t, h, http.MethodPost, "/v1/standing-orders", "op-token", in); status != 201 {
		t.Fatalf("put: %d %v", status, body)
	}
	in.Statement = "B"
	if status, body := changesCall(t, h, http.MethodPost, "/v1/standing-orders", "op-token", in); status != 409 || body["error"] != "standing_not_newer" {
		t.Fatalf("older: %d %v", status, body)
	}
	in.ScopeKind = "project"
	in.Scope = "api-project"
	if status, body := changesCall(t, h, http.MethodPost, "/v1/standing-orders", "desk-token", in); status != 201 {
		t.Fatalf("project: %d %v", status, body)
	}
	if status, body := changesCall(t, h, http.MethodGet, "/v1/standing-orders?project=api-project&lane=api-lane", "peer-token", nil); status != 200 || len(body["orders"].([]any)) != 2 {
		t.Fatalf("list: %d %v", status, body)
	}
	in.ScopeKind = "lane"
	in.Scope = "race"
	in.TopicKey = "new"
	var wg sync.WaitGroup
	type result struct {
		status int
		body   map[string]any
	}
	results := make(chan result, 2)
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			status, body := changesCall(t, h, http.MethodPost, "/v1/standing-orders", "op-token", in)
			results <- result{status, body}
		}()
	}
	<-ready
	<-ready
	close(start)
	wg.Wait()
	close(results)
	created := 0
	for result := range results {
		if result.status == 201 {
			created++
		} else if result.status == 200 && result.body["duplicate"] == true {
			continue
		} else if result.status == 409 && result.body["error"] == "standing_conflict" {
			continue
		} else {
			t.Fatalf("race response %d %v", result.status, result.body)
		}
	}
	if created != 1 {
		t.Fatalf("race created %d", created)
	}
	if status, body := changesCall(t, h, http.MethodGet, "/v1/standing-orders?lane=race", "peer-token", nil); status != 200 || len(body["orders"].([]any)) != 1 {
		t.Fatalf("race list: %d %v", status, body)
	}
}
