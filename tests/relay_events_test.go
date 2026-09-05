package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func relayPayload(lane, job string) map[string]any {
	return map[string]any{
		"kind":             "job.completed",
		"job_id":           job,
		"epoch":            1,
		"owner_lane":       lane,
		"machine":          "host-a",
		"pane_id":          "w1:p1",
		"report_path":      "/tmp/report.md",
		"report_last_line": "VERDICT: JOIN",
		"question":         "",
		"pr":               "",
		"head":             "",
		"reason":           "",
	}
}

func laneEventPayload(lane, eventID, text string) map[string]any {
	return map[string]any{
		"kind":       "lane.event",
		"owner_lane": lane,
		"event_id":   eventID,
		"text":       text,
	}
}

func postRelayEvent(t *testing.T, hURL, token string, body map[string]any) (int, store.RelayEvent) {
	t.Helper()
	resp := request(t, http.DefaultClient, http.MethodPost, hURL+"/v1/relay/events", token, body)
	defer resp.Body.Close()
	var got store.RelayEvent
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, got
}

func TestRelayEventsIdempotent(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	body := relayPayload(lane, "relay-idempotent-"+lane)
	firstStatus, first := postRelayEvent(t, h.URL, "node-token", body)
	secondStatus, second := postRelayEvent(t, h.URL, "node-token", body)
	if firstStatus != http.StatusCreated || secondStatus != http.StatusOK || first.ID != second.ID || second.Attempts != 1 {
		t.Fatalf("first=(%d,%+v) second=(%d,%+v)", firstStatus, first, secondStatus, second)
	}
	got, err := s.ListRelayEvents(t.Context(), lane, false, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("events=%+v err=%v", got, err)
	}
}

func TestLaneEventsIdempotentAndDelivered(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	firstStatus, first := postRelayEvent(t, h.URL, "node-token", laneEventPayload(lane, "producer-1", "first payload"))
	secondStatus, second := postRelayEvent(t, h.URL, "node-token", laneEventPayload(lane, "producer-1", "changed payload"))
	if firstStatus != http.StatusCreated || secondStatus != http.StatusOK || first.ID != second.ID || second.Attempts != 1 || second.Text != "first payload" {
		t.Fatalf("first=(%d,%+v) second=(%d,%+v)", firstStatus, first, secondStatus, second)
	}
	path := fmt.Sprintf("%s/v1/relay/events/%d/delivered", h.URL, first.ID)
	resp := request(t, h.Client(), http.MethodPost, path, "node-token", map[string]string{"machine": "host-a", "pane": "w1:p1"})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("delivery status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	_, afterDelivery := postRelayEvent(t, h.URL, "node-token", laneEventPayload(lane, "producer-1", "third payload"))
	if afterDelivery.DeliveredAt == nil || afterDelivery.Text != "first payload" || afterDelivery.Attempts != 2 {
		t.Fatalf("duplicate changed durable delivery or payload: %+v", afterDelivery)
	}
	listed, err := s.ListRelayEvents(t.Context(), lane, true, 0)
	if err != nil || len(listed) != 0 {
		t.Fatalf("undelivered=%+v err=%v", listed, err)
	}
}

func TestLaneEventsRequireLaneAndEventID(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	for _, body := range []map[string]any{
		laneEventPayload("", "producer-1", "payload"),
		laneEventPayload(lane, "", "payload"),
		laneEventPayload(lane, "producer-2", "bad\ntext"),
	} {
		resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/relay/events", "node-token", body)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("body=%+v status=%d", body, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if got, err := s.ListRelayEvents(t.Context(), lane, false, 0); err != nil || len(got) != 0 {
		t.Fatalf("invalid lane-event created rows=%+v err=%v", got, err)
	}
	job := relayPayload(lane, "lane-event-job-field-"+lane)
	job["event_id"] = "not-allowed"
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/relay/events", "node-token", job)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("job event-id status=%d", resp.StatusCode)
	}
}

type relayPostResult struct {
	status int
	event  store.RelayEvent
	err    error
}

func concurrentRelayPost(t *testing.T, client *http.Client, url, token string, body map[string]any) relayPostResult {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		return relayPostResult{err: err}
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/v1/relay/events", bytes.NewReader(b))
	if err != nil {
		return relayPostResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return relayPostResult{err: err}
	}
	defer resp.Body.Close()
	var event store.RelayEvent
	if err := json.NewDecoder(resp.Body).Decode(&event); err != nil {
		return relayPostResult{err: err}
	}
	return relayPostResult{status: resp.StatusCode, event: event}
}

func TestRelayEventsConcurrentIdempotent(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	body := relayPayload(lane, "relay-concurrent-"+lane)
	start := make(chan struct{})
	results := make(chan relayPostResult, 32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- concurrentRelayPost(t, h.Client(), h.URL, "node-token", body)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	created, duplicate := 0, 0
	var id int64
	for result := range results {
		if result.err != nil {
			t.Errorf("post: %v", result.err)
			continue
		}
		if id == 0 {
			id = result.event.ID
		}
		if result.event.ID != id {
			t.Errorf("id=%d want=%d", result.event.ID, id)
		}
		switch result.status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			duplicate++
		default:
			t.Errorf("status=%d event=%+v", result.status, result.event)
		}
	}
	if created != 1 || duplicate != 31 {
		t.Fatalf("created=%d duplicate=%d", created, duplicate)
	}
	got, err := s.ListRelayEvents(t.Context(), lane, false, 0)
	if err != nil || len(got) != 1 || got[0].ID != id {
		t.Fatalf("events=%+v err=%v id=%d", got, err, id)
	}
}

func TestRelayEventsEmptyIdempotencyFields(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	body := relayPayload(lane, "relay-empty-"+lane)
	body["report_path"] = ""
	body["reason"] = ""
	firstStatus, first := postRelayEvent(t, h.URL, "node-token", body)
	secondStatus, second := postRelayEvent(t, h.URL, "node-token", body)
	if firstStatus != http.StatusCreated || secondStatus != http.StatusOK || first.ID != second.ID || second.Attempts != 1 {
		t.Fatalf("first=(%d,%+v) second=(%d,%+v)", firstStatus, first, secondStatus, second)
	}
	got, err := s.ListRelayEvents(t.Context(), lane, false, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("empty-key events=%+v err=%v", got, err)
	}
	body["reason"] = "a distinct reason remains a distinct idempotency key"
	thirdStatus, third := postRelayEvent(t, h.URL, "node-token", body)
	if thirdStatus != http.StatusCreated || third.ID == first.ID {
		t.Fatalf("third=(%d,%+v) first=%+v", thirdStatus, third, first)
	}
	got, err = s.ListRelayEvents(t.Context(), lane, false, 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("events=%+v err=%v", got, err)
	}
}

func TestRelayEventsDeliveredIsIdempotent(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	lane := taskLane(t)
	_, event := postRelayEvent(t, h.URL, "node-token", relayPayload(lane, "relay-delivered-"+lane))
	path := fmt.Sprintf("%s/v1/relay/events/%d/delivered", h.URL, event.ID)
	resp := request(t, h.Client(), http.MethodPost, path, "node-token", map[string]string{"machine": "host-a", "pane": "w1:p2"})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("first status=%d", resp.StatusCode)
	}
	var first store.RelayEvent
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if first.DeliveredAt == nil || first.DeliveredTo != "host-a/w1:p2" {
		t.Fatalf("first=%+v", first)
	}
	resp = request(t, h.Client(), http.MethodPost, path, "node-token", map[string]string{"machine": "host-b", "pane": "w1:p3"})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("second status=%d", resp.StatusCode)
	}
	var second store.RelayEvent
	if err := json.NewDecoder(resp.Body).Decode(&second); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if second.DeliveredAt == nil || !second.DeliveredAt.Equal(*first.DeliveredAt) || second.DeliveredTo != first.DeliveredTo {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
	missing := request(t, h.Client(), http.MethodPost, h.URL+"/v1/relay/events/9999999999999/delivered", "node-token", map[string]string{"machine": "host-a", "pane": "w1:p2"})
	defer missing.Body.Close()
	var missingBody map[string]string
	if err := json.NewDecoder(missing.Body).Decode(&missingBody); err != nil {
		t.Fatal(err)
	}
	if missing.StatusCode != http.StatusNotFound || missingBody["error"] != "not_found" {
		t.Fatalf("missing status=%d body=%v", missing.StatusCode, missingBody)
	}
}

func TestRelayEventsListUndeliveredByLane(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	laneA, laneB := taskLane(t), taskLane(t)
	_, firstA := postRelayEvent(t, h.URL, "node-token", relayPayload(laneA, "relay-list-a1-"+laneA))
	_, secondA := postRelayEvent(t, h.URL, "node-token", relayPayload(laneA, "relay-list-a2-"+laneA))
	_, eventB := postRelayEvent(t, h.URL, "node-token", relayPayload(laneB, "relay-list-b1-"+laneB))
	path := fmt.Sprintf("%s/v1/relay/events/%d/delivered", h.URL, firstA.ID)
	resp := request(t, h.Client(), http.MethodPost, path, "node-token", map[string]string{"machine": "host-a", "pane": "w1:p2"})
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("delivery status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = request(t, h.Client(), http.MethodGet, h.URL+"/v1/relay/events?undelivered=1&lane="+laneA, "node-token", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d", resp.StatusCode)
	}
	var listed struct {
		Events []store.RelayEvent `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Events) != 1 || listed.Events[0].ID != secondA.ID || listed.Events[0].ID <= firstA.ID {
		t.Fatalf("events=%+v first=%d second=%d", listed.Events, firstA.ID, secondA.ID)
	}
	all, err := s.ListRelayEvents(t.Context(), "", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].ID >= all[i].ID {
			t.Fatalf("not ascending: %d then %d", all[i-1].ID, all[i].ID)
		}
	}
	if eventB.ID == 0 {
		t.Fatal("lane B event was not created")
	}
}

func TestRelayEventsGuardOnlyQuestionAndReason(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	body := relayPayload("fable-wb-p2r", "captain-3-r20-20260905")
	body["epoch"] = int(time.Now().UnixNano() % 1000000000)
	body["report_path"] = "/home/x/herdr-inbox/jobs/captain-3-r20-20260905/report.md"
	body["pr"] = "https://github.com/o/r/pull/34"
	body["question"] = "sk-abcdefghijklmnopqrstuvwxyz"
	resp := request(t, h.Client(), http.MethodPost, h.URL+"/v1/relay/events", "node-token", body)
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("guard status=%d", resp.StatusCode)
	}
	var rejected map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&rejected); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if rejected["error"] != "secret_like_content" {
		t.Fatalf("rejected=%v", rejected)
	}
	body["question"] = ""
	body["reason"] = "sk-abcdefghijklmnopqrstuvwxyz"
	resp = request(t, h.Client(), http.MethodPost, h.URL+"/v1/relay/events", "node-token", body)
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("reason guard status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	body["reason"] = ""
	status, event := postRelayEvent(t, h.URL, "node-token", body)
	if status != http.StatusCreated || event.ID == 0 {
		t.Fatalf("status=%d event=%+v", status, event)
	}
}

func TestRelayEventsRequireAuthentication(t *testing.T) {
	s := taskTestStore(t)
	h := taskHTTP(s)
	defer h.Close()
	requests := []struct {
		method string
		url    string
		body   any
	}{
		{http.MethodPost, h.URL + "/v1/relay/events", relayPayload(taskLane(t), "relay-auth")},
		{http.MethodPost, h.URL + "/v1/relay/events/1/delivered", map[string]string{"machine": "host-a", "pane": "w1:p2"}},
		{http.MethodGet, h.URL + "/v1/relay/events", nil},
	}
	for _, test := range requests {
		resp := request(t, h.Client(), test.method, test.url, "", test.body)
		if resp.StatusCode != http.StatusUnauthorized {
			resp.Body.Close()
			t.Fatalf("%s %s status=%d", test.method, test.url, resp.StatusCode)
		}
		resp.Body.Close()
	}
}
