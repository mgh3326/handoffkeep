package hubforward

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

type fakeOutbox struct {
	rows     []store.NotificationOutbox
	marks    map[string]int64
	markErrs int
}

func (f *fakeOutbox) ListUnsentNotifications(context.Context, int) ([]store.NotificationOutbox, error) {
	out := []store.NotificationOutbox{}
	for _, row := range f.rows {
		if _, sent := f.marks[row.EventID]; !sent {
			out = append(out, row)
		}
	}
	return out, nil
}

func (f *fakeOutbox) MarkNotificationSent(_ context.Context, id string, hub int64) (store.NotificationOutbox, error) {
	if f.markErrs > 0 {
		f.markErrs--
		return store.NotificationOutbox{}, errors.New("mark failed")
	}
	if f.marks == nil {
		f.marks = map[string]int64{}
	}
	f.marks[id] = hub
	return store.NotificationOutbox{}, nil
}

// fakeHub answers like panewire's relay ingress: one row per lane and event id,
// 409 with that row's id for a repeat, and 400 for a field it does not know.
type fakeHub struct {
	t        *testing.T
	failures int
	posts    []map[string]string
	rows     map[string]int64
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/relay/events" || r.Header.Get("Authorization") != "Bearer hub-fake" {
		h.t.Errorf("request %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}
	var body struct {
		Kind       string `json:"kind"`
		Lane       string `json:"lane"`
		EventID    string `json:"event_id"`
		Text       string `json:"text"`
		Label      string `json:"label"`
		NotifyType string `json:"notify_type"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h.posts = append(h.posts, map[string]string{"kind": body.Kind, "lane": body.Lane, "event": body.EventID, "text": body.Text, "type": body.NotifyType, "label": body.Label})
	if h.failures > 0 {
		h.failures--
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if h.rows == nil {
		h.rows = map[string]int64{}
	}
	key := body.Lane + "/" + body.EventID
	if id, dup := h.rows[key]; dup {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "duplicate_event_id", "id": id})
		return
	}
	h.rows[key] = int64(100 + len(h.rows))
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{"id": h.rows[key], "event_id": body.EventID, "lane": body.Lane, "routed": true})
}

func forwarderFixture(t *testing.T, st Store) (*Forwarder, *fakeHub, *time.Time) {
	t.Helper()
	hub := &fakeHub{t: t}
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	return &Forwarder{Store: st, URL: srv.URL + "/", Token: "hub-fake", Now: func() time.Time { return now }}, hub, &now
}

func TestForwardSendsOneEventAndRecordsTheReceipt(t *testing.T) {
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{
		{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "dots-1", Text: "a\nb"},
		{Kind: store.OutboxKindDotsDecision, TargetLane: "operator-desk", EventID: "dots-2", Text: "q"},
		{Kind: store.OutboxKindDotsFollowup, TargetLane: "operator-desk", EventID: "dots-followup-3", Text: "f"},
		{Kind: store.OutboxKindDecisionAnswered, TargetLane: "operator-desk", EventID: "dr-1-1-answered", Text: "d"},
		{Kind: store.OutboxKindChatAnswer, TargetLane: "other-lane", EventID: "Q-1-rev1-answered", Text: "c"},
	}}
	f, hub, _ := forwarderFixture(t, outbox)
	sent, failed, err := f.Once(t.Context())
	if err != nil || sent != 5 || failed != 0 {
		t.Fatalf("sent=%d failed=%d err=%v", sent, failed, err)
	}
	want := []struct{ lane, event, typ string }{
		{"operator-desk", "dots-1", "report"},
		{"operator-desk", "dots-2", "decision"},
		{"operator-desk", "dots-followup-3", "info"},
		{"operator-desk", "dr-1-1-answered", "decision"},
		{"other-lane", "Q-1-rev1-answered", "info"},
	}
	if len(hub.posts) != len(want) {
		t.Fatalf("posts=%v", hub.posts)
	}
	for i, w := range want {
		p := hub.posts[i]
		if p["lane"] != w.lane || p["event"] != w.event || p["type"] != w.typ || p["kind"] != "lane.event" || p["label"] != label {
			t.Errorf("post %d = %v, want %+v", i, p, w)
		}
	}
	if hub.posts[0]["text"] != "a b" {
		t.Errorf("control character reached the hub: %q", hub.posts[0]["text"])
	}
	if outbox.marks["dots-1"] != hub.rows["operator-desk/dots-1"] || outbox.marks["dots-1"] < 1 {
		t.Fatalf("receipt %d, hub row %d", outbox.marks["dots-1"], hub.rows["operator-desk/dots-1"])
	}
	sent, failed, err = f.Once(t.Context())
	if err != nil || sent != 0 || failed != 0 || len(hub.posts) != len(want) {
		t.Fatalf("second pass sent=%d failed=%d posts=%d err=%v", sent, failed, len(hub.posts), err)
	}
}

func TestForwardRetriesAfterFailureWithBackoff(t *testing.T) {
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "dots-1", Text: "t"}}}
	f, hub, now := forwarderFixture(t, outbox)
	hub.failures = 2
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 0 || failed != 1 || len(outbox.marks) != 0 {
		t.Fatalf("sent=%d failed=%d marks=%v err=%v", sent, failed, outbox.marks, err)
	}
	if _, _, err := f.Once(t.Context()); err != nil || len(hub.posts) != 1 {
		t.Fatalf("retried inside the backoff: posts=%d err=%v", len(hub.posts), err)
	}
	*now = now.Add(baseBackoff)
	if _, failed, _ := f.Once(t.Context()); failed != 1 || len(hub.posts) != 2 {
		t.Fatalf("failed=%d posts=%d", failed, len(hub.posts))
	}
	*now = now.Add(baseBackoff)
	if _, _, _ = f.Once(t.Context()); len(hub.posts) != 2 {
		t.Fatalf("backoff did not double: posts=%d", len(hub.posts))
	}
	*now = now.Add(baseBackoff)
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 1 || failed != 0 || outbox.marks["dots-1"] < 1 {
		t.Fatalf("sent=%d failed=%d marks=%v err=%v", sent, failed, outbox.marks, err)
	}
	if len(f.retries) != 0 {
		t.Fatalf("retries=%v", f.retries)
	}
}

// A delivered event whose receipt failed to save goes round again; the hub's
// duplicate answer supplies the same id and the lane hears once.
func TestForwardLostReceiptDoesNotDeliverTwice(t *testing.T) {
	outbox := &fakeOutbox{markErrs: 1, rows: []store.NotificationOutbox{{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "dots-1", Text: "t"}}}
	f, hub, now := forwarderFixture(t, outbox)
	if _, failed, _ := f.Once(t.Context()); failed != 1 || len(outbox.marks) != 0 {
		t.Fatalf("failed=%d marks=%v", failed, outbox.marks)
	}
	*now = now.Add(baseBackoff)
	if sent, _, _ := f.Once(t.Context()); sent != 1 {
		t.Fatalf("sent=%d", sent)
	}
	if len(hub.rows) != 1 || outbox.marks["dots-1"] != hub.rows["operator-desk/dots-1"] {
		t.Fatalf("rows=%v marks=%v", hub.rows, outbox.marks)
	}
}

func TestForwardOneBadRowDoesNotBlockTheNext(t *testing.T) {
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{
		{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "bad", Text: "t"},
		{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "good", Text: "t"},
	}}
	f, hub, _ := forwarderFixture(t, outbox)
	hub.failures = 1
	if sent, failed, _ := f.Once(t.Context()); sent != 1 || failed != 1 || outbox.marks["good"] < 1 {
		t.Fatalf("sent=%d failed=%d marks=%v", sent, failed, outbox.marks)
	}
}

func TestForwardDuplicateWithoutAnIDIsNotAReceipt(t *testing.T) {
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "dots-1", Text: "t"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"duplicate_event_id","id":0}`))
	}))
	defer srv.Close()
	f := &Forwarder{Store: outbox, URL: srv.URL, Token: "hub-fake"}
	if sent, failed, _ := f.Once(t.Context()); sent != 0 || failed != 1 || len(outbox.marks) != 0 {
		t.Fatalf("sent=%d failed=%d marks=%v", sent, failed, outbox.marks)
	}
}

func TestForwardErrorNeverCarriesTheToken(t *testing.T) {
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{{Kind: store.OutboxKindDotsReport, TargetLane: "l", EventID: "e", Text: "t"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	f := &Forwarder{Store: outbox, URL: srv.URL, Token: "hub-fake"}
	_, err := f.post(t.Context(), outbox.rows[0])
	if err == nil || strings.Contains(err.Error(), "hub-fake") {
		t.Fatalf("err=%v", err)
	}
}

// The same flow against a real outbox: a row goes out once and leaves the list.
func TestForwardAgainstStore(t *testing.T) {
	_, url := testdb.Schema(t, "hkforward")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.RecordDotsFollowup(t.Context(), store.DotsIssue{Identifier: "ZZZ-1", Title: "t", URL: "u", IssueUUID: "0a1b2c3d-0000-4000-8000-0000000000f1"}, "0a1b2c3d-0000"); err != nil {
		t.Fatal(err)
	}
	f, hub, now := forwarderFixture(t, st)
	hub.failures = 1
	if _, failed, err := f.Once(t.Context()); err != nil || failed != 1 {
		t.Fatalf("failed=%d err=%v", failed, err)
	}
	if rows, err := st.ListUnsentNotifications(t.Context(), 10); err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	*now = now.Add(baseBackoff)
	if sent, _, err := f.Once(t.Context()); err != nil || sent != 1 {
		t.Fatalf("sent=%d err=%v", sent, err)
	}
	rows, err := st.ListUnsentNotifications(t.Context(), 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 0 || failed != 0 {
		t.Fatalf("sent=%d failed=%d err=%v", sent, failed, err)
	}
	if len(hub.rows) != 1 {
		t.Fatalf("hub rows=%v", hub.rows)
	}
}
