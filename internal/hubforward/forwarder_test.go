package hubforward

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

type fakeOutbox struct {
	rows       []store.NotificationOutbox
	marks      map[string]int64
	markErrs   int
	skipped    map[string]bool
	skipCounts []int64
	skipErr    error
	listSince  []time.Time
}

func (f *fakeOutbox) ListUnsentNotifications(_ context.Context, _ int, since time.Time) ([]store.NotificationOutbox, error) {
	f.listSince = append(f.listSince, since)
	out := []store.NotificationOutbox{}
	for _, row := range f.rows {
		// A zero CreatedAt keeps the older fixtures simple: it means "queued
		// now", never "queued before the cutoff".
		if !row.CreatedAt.IsZero() && row.CreatedAt.Before(since) {
			continue
		}
		if _, sent := f.marks[row.EventID]; sent {
			continue
		}
		if f.skipped[row.EventID] {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

func (f *fakeOutbox) SkipNotificationsBefore(_ context.Context, before time.Time, _ string) (int64, error) {
	if f.skipErr != nil {
		return 0, f.skipErr
	}
	if f.skipped == nil {
		f.skipped = map[string]bool{}
	}
	var n int64
	for _, row := range f.rows {
		// A zero CreatedAt keeps the older fixtures simple: it means "queued
		// now", never "queued before the cutoff".
		if row.CreatedAt.IsZero() || !row.CreatedAt.Before(before) || f.skipped[row.EventID] {
			continue
		}
		if _, sent := f.marks[row.EventID]; sent {
			continue
		}
		f.skipped[row.EventID] = true
		n++
	}
	f.skipCounts = append(f.skipCounts, n)
	return n, nil
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
	return &Forwarder{Store: st, URL: srv.URL + "/", Token: "hub-fake", Now: func() time.Time { return now }, Cutoff: now.Add(-time.Hour)}, hub, &now
}

func TestForwardSendsOneEventAndRecordsTheReceipt(t *testing.T) {
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{
		{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "dots-1", Text: "a\nb"},
		{Kind: store.OutboxKindDotsDecision, TargetLane: "operator-desk", EventID: "dots-2", Text: "q"},
		{Kind: store.OutboxKindDotsFollowup, TargetLane: "operator-desk", EventID: "dots-followup-3", Text: "f"},
		{Kind: store.OutboxKindDecisionAnswered, TargetLane: "operator-desk", EventID: "dr-1-1-answered", Text: "d"},
		{Kind: store.OutboxKindChatAnswer, TargetLane: "other-lane", EventID: "Q-1-rev1-answered", Text: "c"},
		{Kind: store.OutboxKindDotsBlocked, TargetLane: "operator-desk", EventID: "dots-4", Text: "b"},
		{Kind: store.OutboxKindTaskOrder, TargetLane: "design-lane", EventID: "task-order-ord-aaaaaaaaaa", Text: "o"},
		{Kind: store.OutboxKindTaskOrder, TargetLane: "design-lane", EventID: "task-order-withdrawn-ord-aaaaaaaaaa", Text: "w"},
		{Kind: store.OutboxKindTaskDrift, TargetLane: "lane-a", EventID: "drift-lane-a-2026-11-18", Text: "d"},
	}}
	f, hub, _ := forwarderFixture(t, outbox)
	sent, failed, err := f.Once(t.Context())
	if err != nil || sent != 9 || failed != 0 {
		t.Fatalf("sent=%d failed=%d err=%v", sent, failed, err)
	}
	want := []struct{ lane, event, typ string }{
		{"operator-desk", "dots-1", "ready"},
		{"operator-desk", "dots-2", "decision"},
		{"operator-desk", "dots-followup-3", "info"},
		{"operator-desk", "dr-1-1-answered", "decision"},
		{"other-lane", "Q-1-rev1-answered", "decision"},
		{"operator-desk", "dots-4", "blocked"},
		{"design-lane", "task-order-ord-aaaaaaaaaa", "order"},
		{"design-lane", "task-order-withdrawn-ord-aaaaaaaaaa", "order"},
		{"lane-a", "drift-lane-a-2026-11-18", "info"},
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

// The first start settles every row queued before the cutoff instead of
// posting it; the rows at or after the cutoff go out. A second run with the
// same cutoff skips and sends nothing: a restart neither re-skips nor
// re-sends (hk 4986).
func TestForwardSkipsRowsBeforeTheCutoff(t *testing.T) {
	cutoff := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{
		{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "backlog", Text: "old", CreatedAt: cutoff.Add(-time.Hour)},
		{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "at-cutoff", Text: "new", CreatedAt: cutoff},
	}}
	f, hub, _ := forwarderFixture(t, outbox)
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 1 || failed != 0 {
		t.Fatalf("sent=%d failed=%d err=%v", sent, failed, err)
	}
	if len(hub.posts) != 1 || hub.posts[0]["event"] != "at-cutoff" {
		t.Fatalf("posts=%v, want only the row at the cutoff", hub.posts)
	}
	if len(outbox.listSince) != 1 || !outbox.listSince[0].Equal(f.Cutoff) {
		t.Fatalf("list cutoff=%v, want %v (a row committed between settle and list must not be listed)", outbox.listSince, f.Cutoff)
	}
	if !outbox.skipped["backlog"] || outbox.skipped["at-cutoff"] {
		t.Fatalf("skipped=%v", outbox.skipped)
	}
	if outbox.marks["backlog"] != 0 {
		t.Fatalf("a skipped row recorded a receipt: %v", outbox.marks)
	}
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 0 || failed != 0 || len(hub.posts) != 1 {
		t.Fatalf("restart sent=%d failed=%d posts=%d err=%v", sent, failed, len(hub.posts), err)
	}
	if len(outbox.skipCounts) != 2 || outbox.skipCounts[0] != 1 || outbox.skipCounts[1] != 0 {
		t.Fatalf("skip counts=%v, want [1 0]", outbox.skipCounts)
	}
}

// The pre-cutoff settle runs every tick because a caller can commit a row with
// an older created_at after an earlier settle returned empty (hk 6418).
func TestForwarderResettlesLatePreCutoffRows(t *testing.T) {
	cutoff := time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC)
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{
		{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "backlog", Text: "old", CreatedAt: cutoff.Add(-time.Hour)},
		{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "live", Text: "new", CreatedAt: cutoff.Add(time.Minute)},
	}}
	f, hub, _ := forwarderFixture(t, outbox)
	if !f.Cutoff.Equal(cutoff) {
		t.Fatalf("fixture cutoff=%v, want %v", f.Cutoff, cutoff)
	}
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 1 || failed != 0 {
		t.Fatalf("first sent=%d failed=%d err=%v", sent, failed, err)
	}
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 0 || failed != 0 {
		t.Fatalf("second sent=%d failed=%d err=%v", sent, failed, err)
	}
	if got := len(outbox.skipCounts); got != 2 {
		t.Fatalf("skip calls=%d, want a settle on each tick", got)
	}
	outbox.rows = append(outbox.rows, store.NotificationOutbox{
		Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "late-backlog",
		Text: "old", CreatedAt: cutoff.Add(-time.Minute),
	})
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 0 || failed != 0 {
		t.Fatalf("late-row tick sent=%d failed=%d err=%v", sent, failed, err)
	}
	if !outbox.skipped["late-backlog"] {
		t.Fatalf("late pre-cutoff row remained owed: skipped=%v", outbox.skipped)
	}
	for i := range 3 {
		if sent, failed, err := f.Once(t.Context()); err != nil || sent != 0 || failed != 0 {
			t.Fatalf("tick %d sent=%d failed=%d err=%v", i, sent, failed, err)
		}
	}
	if got := len(outbox.skipCounts); got != 6 {
		t.Fatalf("skip calls=%d, want one settle per tick", got)
	}
	if len(hub.posts) != 1 || hub.posts[0]["event"] != "live" {
		t.Fatalf("posts=%v, want only the row at or after the cutoff", hub.posts)
	}
	if outbox.marks["backlog"] != 0 {
		t.Fatalf("a pre-cutoff row recorded a receipt: %v", outbox.marks)
	}
	for _, since := range outbox.listSince {
		if !since.Equal(f.Cutoff) {
			t.Fatalf("list cutoff=%v, want %v", since, f.Cutoff)
		}
	}
}

func TestForwarderResettlesWhileCutoffIsInTheFuture(t *testing.T) {
	outbox := &fakeOutbox{}
	f, _, now := forwarderFixture(t, outbox)
	f.Cutoff = (*now).Add(time.Minute)
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 0 || failed != 0 {
		t.Fatalf("initial tick sent=%d failed=%d err=%v", sent, failed, err)
	}
	outbox.rows = append(outbox.rows, store.NotificationOutbox{
		Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "future-cutoff-backlog",
		Text: "old", CreatedAt: (*now).Add(30 * time.Second),
	})
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 0 || failed != 0 {
		t.Fatalf("late tick sent=%d failed=%d err=%v", sent, failed, err)
	}
	if !outbox.skipped["future-cutoff-backlog"] {
		t.Fatalf("row before future cutoff remained owed: skipped=%v", outbox.skipped)
	}
}

// A store that cannot settle the backlog must not let an old row through:
// the run fails instead of posting it (hk 4986).
func TestForwardRefusesToPostWhenTheBacklogCannotBeSkipped(t *testing.T) {
	outbox := &fakeOutbox{
		skipErr: errors.New("skip failed"),
		rows:    []store.NotificationOutbox{{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "backlog", Text: "old", CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)}},
	}
	f, hub, _ := forwarderFixture(t, outbox)
	if sent, failed, err := f.Once(t.Context()); err == nil || sent != 0 || failed != 0 || len(hub.posts) != 0 {
		t.Fatalf("sent=%d failed=%d posts=%d err=%v", sent, failed, len(hub.posts), err)
	}
}

// A forwarder with no cutoff refuses to run: a zero value must never mean
// "send the whole backlog" (hk 4986).
func TestForwardWithoutACutoffSendsNothing(t *testing.T) {
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "dots-1", Text: "t"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }))
	defer srv.Close()
	f := &Forwarder{Store: outbox, URL: srv.URL, Token: "hub-fake"}
	if sent, failed, err := f.Once(t.Context()); !errors.Is(err, errNoCutoff) || sent != 0 || failed != 0 || len(outbox.marks) != 0 {
		t.Fatalf("sent=%d failed=%d marks=%v err=%v", sent, failed, outbox.marks, err)
	}
}

// logSink collects log lines from the global logger and signals each one, so a
// test can wait for the start line without polling.
type logSink struct {
	mu   sync.Mutex
	line string
	seen chan string
}

func (s *logSink) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	s.mu.Lock()
	s.line = line
	s.mu.Unlock()
	select {
	case s.seen <- line:
	default:
	}
	return len(p), nil
}

// The start line reports that forwarding is on, the cutoff, and how many rows
// the start settled; nothing older than the cutoff is posted (hk 4986).
func TestRunLogsStartCutoffAndSkippedRows(t *testing.T) {
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{
		{Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "backlog", Text: "old", CreatedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)},
	}}
	f, hub, _ := forwarderFixture(t, outbox)
	f.Interval = time.Hour
	sink := &logSink{seen: make(chan string, 8)}
	old := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(old) })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); f.Run(ctx) }()
	var start string
	deadline := time.After(5 * time.Second)
	for start == "" {
		select {
		case line := <-sink.seen:
			if strings.Contains(line, "notification_forward.start ") {
				start = line
			}
		case <-deadline:
			t.Fatal("no notification_forward.start line")
		}
	}
	cancel()
	<-done
	if !strings.Contains(start, "cutoff=2026-11-01T23:00:00Z") || !strings.Contains(start, "skipped=1") {
		t.Fatalf("start line = %q", start)
	}
	if len(hub.posts) != 0 {
		t.Fatalf("start posted the backlog: %v", hub.posts)
	}
}

func TestForwardTruncatesOnlyTheHubCopy(t *testing.T) {
	fullText := strings.Repeat("한글 lane event ", 300)
	if len(fullText) <= hubLaneTextMaxBytes {
		t.Fatalf("fixture bytes=%d, want more than the hub limit %d", len(fullText), hubLaneTextMaxBytes)
	}
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{{
		Kind: store.OutboxKindDotsReport, TargetLane: "operator-desk", EventID: "long-lane-text", Text: fullText,
	}}}
	f, hub, _ := forwarderFixture(t, outbox)
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 1 || failed != 0 {
		t.Fatalf("sent=%d failed=%d err=%v", sent, failed, err)
	}
	if len(hub.posts) != 1 {
		t.Fatalf("hub posts=%d", len(hub.posts))
	}
	forwarded := hub.posts[0]["text"]
	if len(forwarded) > hubLaneTextMaxBytes || !utf8.ValidString(forwarded) || !strings.HasSuffix(forwarded, laneTextMarker) {
		t.Fatalf("forwarded bytes=%d valid=%t suffix=%t", len(forwarded), utf8.ValidString(forwarded), strings.HasSuffix(forwarded, laneTextMarker))
	}
	if outbox.rows[0].Text != fullText {
		t.Fatalf("outbox text changed: got %d bytes, want %d", len(outbox.rows[0].Text), len(fullText))
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
	f := &Forwarder{Store: outbox, URL: srv.URL, Token: "hub-fake", Cutoff: time.Now().UTC().Add(-time.Hour)}
	if sent, failed, _ := f.Once(t.Context()); sent != 0 || failed != 1 || len(outbox.marks) != 0 {
		t.Fatalf("sent=%d failed=%d marks=%v", sent, failed, outbox.marks)
	}
}

func TestForwardErrorNeverCarriesTheToken(t *testing.T) {
	outbox := &fakeOutbox{rows: []store.NotificationOutbox{{Kind: store.OutboxKindDotsReport, TargetLane: "l", EventID: "e", Text: "t"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	f := &Forwarder{Store: outbox, URL: srv.URL, Token: "hub-fake", Cutoff: time.Now().UTC().Add(-time.Hour)}
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
	seedOutboxRow(t, url)
	f, hub, now := forwarderFixture(t, st)
	hub.failures = 1
	if _, failed, err := f.Once(t.Context()); err != nil || failed != 1 {
		t.Fatalf("failed=%d err=%v", failed, err)
	}
	if rows, err := st.ListUnsentNotifications(t.Context(), 10, f.Cutoff); err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	*now = now.Add(baseBackoff)
	if sent, _, err := f.Once(t.Context()); err != nil || sent != 1 {
		t.Fatalf("sent=%d err=%v", sent, err)
	}
	rows, err := st.ListUnsentNotifications(t.Context(), 10, f.Cutoff)
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

// The cutoff against a real outbox: an old row is settled as skipped and never
// reaches the hub, the newer row goes out, and a second run posts nothing more
// (hk 4986).
func TestForwardSkipsBacklogAgainstStore(t *testing.T) {
	_, url := testdb.Schema(t, "hkforwardskip")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	cutoff := time.Now().UTC().Add(-2 * time.Hour)
	seedOutboxRowAt(t, url, "forward-skip-old", cutoff.Add(-time.Hour))
	seedOutboxRowAt(t, url, "forward-skip-new", cutoff.Add(time.Minute))
	f, hub, _ := forwarderFixture(t, st)
	f.Cutoff = cutoff
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 1 || failed != 0 {
		t.Fatalf("sent=%d failed=%d err=%v", sent, failed, err)
	}
	if len(hub.posts) != 1 || hub.posts[0]["event"] != "forward-skip-new" {
		t.Fatalf("posts=%v, want only the row after the cutoff", hub.posts)
	}
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	var skipped *time.Time
	var reason *string
	if err := conn.QueryRow(t.Context(), `SELECT skipped_at,skip_reason FROM notification_outbox WHERE event_id='forward-skip-old'`).Scan(&skipped, &reason); err != nil {
		t.Fatal(err)
	}
	if skipped == nil || reason == nil || *reason != store.OutboxSkipBeforeForwarder {
		t.Fatalf("skipped=%v reason=%v", skipped, reason)
	}
	var sentAt *time.Time
	if err := conn.QueryRow(t.Context(), `SELECT sent_at FROM notification_outbox WHERE event_id='forward-skip-old'`).Scan(&sentAt); err != nil {
		t.Fatal(err)
	}
	if sentAt != nil {
		t.Fatalf("a skipped row gained sent_at: %v", sentAt)
	}
	if rows, err := st.ListUnsentNotifications(t.Context(), 10, cutoff); err != nil || len(rows) != 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if sent, failed, err := f.Once(t.Context()); err != nil || sent != 0 || failed != 0 || len(hub.posts) != 1 {
		t.Fatalf("restart sent=%d failed=%d posts=%d err=%v", sent, failed, len(hub.posts), err)
	}
}

// seedOutboxRow queues one lane notification in the schema at url, the row
// a dots follow-up writes.
func seedOutboxRow(t *testing.T, url string) {
	t.Helper()
	seedOutboxRowAt(t, url, "forward-test-1", time.Now().UTC())
}

// seedOutboxRowAt queues one lane notification with an explicit creation time,
// so a test can place it on either side of the forwarder's cutoff.
func seedOutboxRowAt(t *testing.T, url, eventID string, at time.Time) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(t.Context())
	if _, err := conn.Exec(t.Context(), `INSERT INTO notification_outbox(kind,target_lane,event_id,text,created_at) VALUES('dots_followup','operator-desk',$1,'[dots-watch] test',$2)`, eventID, at); err != nil {
		t.Fatal(err)
	}
}
