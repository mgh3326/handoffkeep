package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

// The webhook path carries the credential; every test that sends looks for it
// in what could leak.
const fakeDiscordSecret = "fake-webhook-credential-s6a"

type discordCall struct {
	Method, Path, Query string
	Content             string
	Parse               []any
	HasParse            bool
	Users               []any
}

// fakeDiscord stands in for a Discord incoming webhook. A post answers with a
// new message id when ?wait=true is set; status, when set, fails every call.
type fakeDiscord struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []discordCall
	nextID int
	status func(method string) int
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	t.Helper()
	f := &fakeDiscord{nextID: 1000}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content         string         `json:"content"`
			AllowedMentions map[string]any `json:"allowed_mentions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		parse, hasParse := body.AllowedMentions["parse"].([]any)
		users, _ := body.AllowedMentions["users"].([]any)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, discordCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Content: body.Content, Parse: parse, HasParse: hasParse && len(body.AllowedMentions) == 1, Users: users})
		if f.status != nil {
			if code := f.status(r.Method); code != 0 {
				w.WriteHeader(code)
				return
			}
		}
		if r.Method == http.MethodPost && r.URL.Query().Get("wait") == "true" {
			f.nextID++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": fmt.Sprint(f.nextID), "content": body.Content})
			return
		}
		if r.Method == http.MethodPatch {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.TrimPrefix(r.URL.Path[strings.LastIndex(r.URL.Path, "/"):], "/")})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDiscord) webhook() *DiscordWebhook {
	return &DiscordWebhook{URL: f.URL + "/api/webhooks/1/" + fakeDiscordSecret, HTTP: f.Client()}
}

// fail sets how the fake answers from now on; nil answers normally.
func (f *fakeDiscord) fail(status func(method string) int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = status
}

func (f *fakeDiscord) take() []discordCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// notifyStoreFixture is a real store in a throwaway schema, plus a raw
// connection for what no store method writes (expires_at, created_at).
func notifyStoreFixture(t *testing.T) (*store.Store, *pgx.Conn) {
	t.Helper()
	_, url := testdb.Schema(t, "hknotify")
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	conn, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return st, conn
}

func chatDecision(t *testing.T, st *store.Store, id, body string) store.DecisionRecord {
	t.Helper()
	if _, _, err := st.UpsertChatQuestion(t.Context(), store.ChatQuestion{ID: id, Lane: "notify-lane", Body: body}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListDecisionRecords(t.Context(), store.DecisionRecordFilter{Source: store.DecisionSourceChat, IncludeSnoozed: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range rows {
		if x.SourceRef == id {
			return x
		}
	}
	t.Fatalf("no decision record for %s", id)
	return store.DecisionRecord{}
}

func noticeOf(t *testing.T, st *store.Store, id int64) store.DecisionNoticeState {
	t.Helper()
	x, found, err := st.GetDecisionNotice(t.Context(), id)
	if err != nil || !found {
		t.Fatalf("notice %d: found=%v err=%v", id, found, err)
	}
	return x
}

func clockAt(t time.Time) func() time.Time { return func() time.Time { return t } }

func mentionsOff(t *testing.T, calls []discordCall) {
	t.Helper()
	for _, c := range calls {
		if !c.HasParse || len(c.Parse) != 0 {
			t.Errorf("%s %s: allowed_mentions not empty: %+v", c.Method, c.Path, c)
		}
	}
}

// Opening posts once with ?wait=true and stores the id; answering,
// superseding and expiring each edit that message once; redelivered writes
// send nothing.
func TestDecisionDeliveryWorkerPostsOnceAndEditsEachClose(t *testing.T) {
	st, conn := notifyStoreFixture(t)
	discord := newFakeDiscord(t)
	answered := chatDecision(t, st, "Q-20261105-61", "@everyone ship the migration?")
	withdrawn := chatDecision(t, st, "Q-20261105-62", "rename the lane?")
	expiring := chatDecision(t, st, "Q-20261105-63", "keep the old table?")
	// A redelivered open finds its key and queues nothing more.
	chatDecision(t, st, "Q-20261105-61", "@everyone ship the migration?")
	start := time.Now().UTC()
	// The expiry is set before the first post: after it, nothing writes the
	// request, and the edit comes from the clock alone.
	if _, err := conn.Exec(t.Context(), `UPDATE decision_requests SET expires_at=$2 WHERE id=$1`, expiring.ID, start.Add(150*time.Second)); err != nil {
		t.Fatal(err)
	}
	n := DecisionNotifier{Store: st, Discord: discord.webhook(), ConsoleURL: "https://console.example/hk"}

	n.Now = clockAt(start)
	n.Drain(t.Context())
	if calls := discord.take(); len(calls) != 0 {
		t.Fatalf("posted before the post delay: %+v", calls)
	}

	n.Now = clockAt(start.Add(2 * time.Minute))
	n.Drain(t.Context())
	posts := discord.take()
	if len(posts) != 3 {
		t.Fatalf("posts=%+v", posts)
	}
	mentionsOff(t, posts)
	ids := map[int64]string{}
	for i, x := range []store.DecisionRecord{answered, withdrawn, expiring} {
		p := posts[i]
		if p.Method != http.MethodPost || p.Query != "wait=true" || !strings.HasSuffix(p.Path, "/"+fakeDiscordSecret) {
			t.Fatalf("post %d: %+v", i, p)
		}
		for _, want := range []string{fmt.Sprintf("Decision request #%d", x.ID), x.Question, "source chat", "lane notify-lane", "open 2m", fmt.Sprintf("https://console.example/hk/ui/decisions/%d", x.ID)} {
			if !strings.Contains(p.Content, want) {
				t.Errorf("post %d lacks %q:\n%s", i, want, p.Content)
			}
		}
		state := noticeOf(t, st, x.ID)
		if state.AppliedVersion != store.DecisionStatusOpen || state.DesiredVersion != store.DecisionStatusOpen || state.State != store.DecisionDeliveryPending ||
			state.MessageID == "" || state.PostedAt == nil {
			t.Fatalf("after post: %+v", state)
		}
		ids[x.ID] = state.MessageID
	}
	n.Drain(t.Context())
	if calls := discord.take(); len(calls) != 0 {
		t.Fatalf("second drain sent: %+v", calls)
	}

	if _, err := st.TransitionChatQuestion(t.Context(), "Q-20261105-61", "resolved", "test-answerer"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionChatQuestion(t.Context(), "Q-20261105-62", "withdrawn", "test-answerer"); err != nil {
		t.Fatal(err)
	}
	// A redelivered answer is refused and changes nothing.
	if _, err := st.TransitionChatQuestion(t.Context(), "Q-20261105-61", "resolved", "test-answerer"); !errors.Is(err, store.ErrChatQuestionConflict) {
		t.Fatalf("redelivered answer: %v", err)
	}
	n.Now = clockAt(start.Add(3 * time.Minute))
	n.Drain(t.Context())
	edits := discord.take()
	if len(edits) != 3 {
		t.Fatalf("edits=%+v", edits)
	}
	mentionsOff(t, edits)
	byPath := map[string]discordCall{}
	for _, e := range edits {
		if e.Method != http.MethodPatch || e.Query != "" {
			t.Fatalf("edit: %+v", e)
		}
		byPath[e.Path] = e
	}
	for x, want := range map[int64]string{answered.ID: "answered by test-answerer", withdrawn.ID: "superseded (withdrawn)", expiring.ID: "expired unanswered"} {
		e, ok := byPath["/api/webhooks/1/"+fakeDiscordSecret+"/messages/"+ids[x]]
		if !ok {
			t.Fatalf("no edit of message %s: %+v", ids[x], edits)
		}
		if !strings.Contains(e.Content, want) || !strings.Contains(e.Content, fmt.Sprintf("https://console.example/hk/ui/decisions/%d", x)) || strings.Contains(e.Content, "❓") {
			t.Errorf("edit of #%d lacks %q or the link:\n%s", x, want, e.Content)
		}
		state := noticeOf(t, st, x)
		if state.EditedAt == nil || state.MessageID != ids[x] || state.AppliedVersion != state.DesiredVersion {
			t.Errorf("after edit: %+v", state)
		}
	}
	if s := noticeOf(t, st, expiring.ID); s.AppliedVersion != store.DecisionStatusExpired {
		t.Errorf("expired row: %+v", s)
	}
	n.Now = clockAt(start.Add(4 * time.Minute))
	n.Drain(t.Context())
	if calls := discord.take(); len(calls) != 0 {
		t.Fatalf("drain after the edits sent: %+v", calls)
	}
}

// A request asked through Linear is announced by the Linear flow only: its
// task_request row is superseded before the post delay ends, and the linear
// row is never queued. A plain request on another task still posts.
func TestDecisionNotifierLeavesLinearRequestsToTheLinearFlow(t *testing.T) {
	st, task := decisionStoreFixture(t)
	discord := newFakeDiscord(t)
	if _, err := (LinearDecisions{Team: "ROB", Linear: newFakeLinear()}).Ask(t.Context(), st, task.ID, "test-answerer", askInput()); err != nil {
		t.Fatal(err)
	}
	plainTask, err := st.CreateTask(t.Context(), store.Task{Lane: "decision-lane", Title: "plain ask", Kind: "implement", CreatedBy: "t", Project: task.Project})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordDecisionRequest(t.Context(), plainTask.ID, "test-answerer", askInput()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rows := map[string][]store.DecisionRecord{}
	for _, status := range []string{store.DecisionStatusOpen, store.DecisionStatusSuperseded} {
		list, err := st.ListDecisionRecords(t.Context(), store.DecisionRecordFilter{Status: status}, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range list {
			rows[x.Source] = append(rows[x.Source], x)
		}
	}
	if len(rows[store.DecisionSourceLinear]) != 1 || len(rows[store.DecisionSourceTaskRequest]) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	DecisionNotifier{Store: st, Discord: discord.webhook(), Now: clockAt(now.Add(2 * time.Minute))}.Drain(t.Context())
	posts := discord.take()
	if len(posts) != 1 || !strings.Contains(posts[0].Content, fmt.Sprintf("task #%d: plain ask", plainTask.ID)) {
		t.Fatalf("posts=%+v", posts)
	}
	if _, found, err := st.GetDecisionNotice(t.Context(), rows[store.DecisionSourceLinear][0].ID); err != nil || found {
		t.Fatalf("linear row queued: found=%v err=%v", found, err)
	}
	for _, x := range rows[store.DecisionSourceTaskRequest] {
		state := noticeOf(t, st, x.ID)
		switch x.TaskID != nil && *x.TaskID == task.ID {
		case true:
			if state.AppliedVersion != store.DecisionStatusSuperseded || state.MessageID != "" {
				t.Errorf("the Linear request's task_request row: %+v", state)
			}
		default:
			if state.AppliedVersion != store.DecisionStatusOpen || state.MessageID == "" {
				t.Errorf("plain request row: %+v", state)
			}
		}
	}
}

// The webhook URL is a credential: no failure may put it in an error, a log
// line or last_error_code.
func TestDecisionDeliveryWorkerRetriesWithoutLeakingTheWebhookURL(t *testing.T) {
	st, _ := notifyStoreFixture(t)
	logs := captureLog(t)
	discord := newFakeDiscord(t)
	discord.fail(func(string) int { return http.StatusInternalServerError })
	x := chatDecision(t, st, "Q-20261105-71", "retry me")
	start := time.Now().UTC()
	n := DecisionNotifier{Store: st, Discord: discord.webhook(), Now: clockAt(start.Add(2 * time.Minute))}
	n.Drain(t.Context())
	state := noticeOf(t, st, x.ID)
	if state.Attempts != 1 || state.AppliedVersion != "" || state.State != store.DecisionDeliveryPending || state.LastErrorCode != "discord webhook status 500" {
		t.Fatalf("after a 500: %+v", state)
	}
	// Backoff: the same clock does not retry.
	n.Drain(t.Context())
	if calls := discord.take(); len(calls) != 1 {
		t.Fatalf("calls=%+v", calls)
	}

	// Unreachable: the transport error names the host, never the path.
	closed := newFakeDiscord(t)
	unreachable := closed.webhook()
	closed.Close()
	n.Discord = unreachable
	n.Now = clockAt(start.Add(time.Hour))
	n.Drain(t.Context())
	state = noticeOf(t, st, x.ID)
	if state.Attempts != 2 || state.LastErrorCode == "" {
		t.Fatalf("after unreachable: %+v", state)
	}
	unreachableErr := state.LastErrorCode

	// A posted message whose edit Discord refuses (deleted) gives up at once.
	discord.fail(nil)
	n.Discord = discord.webhook()
	n.Now = clockAt(start.Add(2 * time.Hour))
	n.Drain(t.Context())
	if state = noticeOf(t, st, x.ID); state.AppliedVersion != store.DecisionStatusOpen || state.Attempts != 0 || state.LastErrorCode != "" {
		t.Fatalf("after the retry: %+v", state)
	}
	discord.fail(func(method string) int {
		if method == http.MethodPatch {
			return http.StatusNotFound
		}
		return 0
	})
	if _, err := st.TransitionChatQuestion(t.Context(), "Q-20261105-71", "resolved", "test-answerer"); err != nil {
		t.Fatal(err)
	}
	n.Drain(t.Context())
	state = noticeOf(t, st, x.ID)
	if state.State != store.DecisionNoticeFailed || state.LastErrorCode != "decision_notice_rejected" {
		t.Fatalf("after a 404 edit: %+v", state)
	}
	for name, text := range map[string]string{"log": logs.String(), "unreachable last_error_code": unreachableErr, "last_error_code": state.LastErrorCode} {
		if strings.Contains(text, fakeDiscordSecret) || strings.Contains(text, "/api/webhooks/") {
			t.Errorf("%s carries the webhook URL: %s", name, text)
		}
	}
	for _, want := range []string{"decision_notify.failed", "decision_notify.gave_up"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %s:\n%s", want, logs.String())
		}
	}
}

// Without the webhook nothing is sent and what falls due settles unsent, so
// setting the webhook later replays nothing. Without the console URL the
// message says where to look instead of linking.
func TestDecisionDeliveryWorkerUnsentWithoutWebhookOrConsole(t *testing.T) {
	st, _ := notifyStoreFixture(t)
	x := chatDecision(t, st, "Q-20261105-81", "unsent question")
	start := time.Now().UTC()
	day := start.Add(48 * time.Hour)
	DecisionNotifier{Store: st, Now: clockAt(day)}.Drain(t.Context())
	if state := noticeOf(t, st, x.ID); state.State != store.DecisionNoticeUnsent || state.MessageID != "" {
		t.Fatalf("unset webhook: %+v", state)
	}
	if digest, found, err := st.GetDecisionDigest(t.Context(), day); err != nil || !found || digest.State != "unsent" {
		t.Fatalf("digest without webhook: %+v found=%v err=%v", digest, found, err)
	}
	discord := newFakeDiscord(t)
	DecisionNotifier{Store: st, Discord: discord.webhook(), Now: clockAt(day.Add(time.Minute))}.Drain(t.Context())
	if calls := discord.take(); len(calls) != 0 {
		t.Fatalf("replayed after the webhook was set: %+v", calls)
	}

	y := chatDecision(t, st, "Q-20261105-82", "no console")
	DecisionNotifier{Store: st, Discord: discord.webhook(), Now: clockAt(time.Now().Add(2 * time.Minute))}.Drain(t.Context())
	posts := discord.take()
	if len(posts) != 1 || strings.Contains(posts[0].Content, "/ui/decisions") || strings.Contains(posts[0].Content, "https://") ||
		!strings.Contains(posts[0].Content, fmt.Sprintf("Find request #%d", y.ID)) {
		t.Fatalf("posts=%+v", posts)
	}
}

// One digest per UTC day, listing only requests open over 24 hours, oldest
// first; a second run the same day sends nothing.
func TestDecisionDigestOncePerUTCDay(t *testing.T) {
	st, conn := notifyStoreFixture(t)
	discord := newFakeDiscord(t)
	// Two days ahead, so every row below is in the past.
	day := time.Now().UTC().Truncate(24 * time.Hour).Add(48 * time.Hour)
	at := day.Add(time.Hour)
	ages := map[string]time.Duration{"Q-20261105-91": 30 * time.Hour, "Q-20261105-92": 72 * time.Hour, "Q-20261105-93": 48 * time.Hour, "Q-20261105-94": 23 * time.Hour, "Q-20261105-95": 96 * time.Hour, "Q-20261105-96": 24 * time.Hour}
	ids := map[string]int64{}
	for ref, age := range ages {
		x := chatDecision(t, st, ref, "question "+ref)
		ids[ref] = x.ID
		if _, err := conn.Exec(t.Context(), `UPDATE decision_requests SET created_at=$2 WHERE id=$1`, x.ID, at.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	// The oldest is answered, so it is no longer open.
	if _, err := st.TransitionChatQuestion(t.Context(), "Q-20261105-95", "resolved", "test-answerer"); err != nil {
		t.Fatal(err)
	}
	digests := func() []discordCall {
		out := []discordCall{}
		for _, c := range discord.take() {
			if strings.HasPrefix(c.Content, "🗓️") {
				out = append(out, c)
			}
		}
		return out
	}
	n := DecisionNotifier{Store: st, Discord: discord.webhook(), Now: clockAt(at)}
	n.Drain(t.Context())
	got := digests()
	if len(got) != 1 || got[0].Method != http.MethodPost || got[0].Query != "wait=true" {
		t.Fatalf("digests=%+v", got)
	}
	mentionsOff(t, got)
	text := got[0].Content
	order := []string{"Q-20261105-92", "Q-20261105-93", "Q-20261105-91"}
	last := -1
	for _, ref := range order {
		i := strings.Index(text, fmt.Sprintf("#%d ·", ids[ref]))
		if i < 0 || i < last {
			t.Fatalf("%s missing or out of order:\n%s", ref, text)
		}
		last = i
	}
	// Q-20261105-96 is exactly 24 hours old: not yet open longer than that.
	for _, ref := range []string{"Q-20261105-94", "Q-20261105-95", "Q-20261105-96"} {
		if strings.Contains(text, fmt.Sprintf("#%d ·", ids[ref])) {
			t.Errorf("%s listed:\n%s", ref, text)
		}
	}
	if !strings.Contains(text, day.Format(time.DateOnly)+" (UTC) · 3") || strings.Contains(text, "more") {
		t.Errorf("header or count:\n%s", text)
	}
	if digest, _, err := st.GetDecisionDigest(t.Context(), at); err != nil || digest.State != "sent" || digest.Listed != 3 || digest.MessageID == "" {
		t.Fatalf("digest row: %+v %v", digest, err)
	}

	n.Now = clockAt(day.Add(23*time.Hour + 59*time.Minute))
	n.Drain(t.Context())
	if got := digests(); len(got) != 0 {
		t.Fatalf("second digest the same day: %+v", got)
	}
	n.Now = clockAt(day.Add(25 * time.Hour))
	n.Drain(t.Context())
	if got := digests(); len(got) != 1 || !strings.Contains(got[0].Content, day.Add(24*time.Hour).Format(time.DateOnly)+" (UTC) · 5") {
		t.Fatalf("next day: %+v", got)
	}
}

// A digest with more than fits keeps oldest first and ends with "+N more".
func TestDecisionDigestTextCapsWithMore(t *testing.T) {
	now := time.Date(2026, 11, 5, 1, 0, 0, 0, time.UTC)
	d := store.DecisionDigest{Day: time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC), Total: 120}
	for i := range store.DecisionDigestMaxItems {
		d.Items = append(d.Items, store.DecisionNotice{Record: store.DecisionRecord{
			ID: int64(i + 1), Source: store.DecisionSourceChat, Lane: "notify-lane",
			Question: strings.Repeat("long question ", 20), CreatedAt: now.Add(-time.Duration(200-i) * time.Hour),
		}})
	}
	for _, console := range []string{"", "https://console.example"} {
		text := decisionDigestText(d, console, now)
		if n := utf8.RuneCountInString(text); n > discordContentMaxRunes {
			t.Fatalf("digest is %d runes", n)
		}
		shown := strings.Count(text, "\n• #")
		if shown == 0 || shown >= store.DecisionDigestMaxItems || !strings.Contains(text, fmt.Sprintf("\n+%d more\n", d.Total-shown)) {
			t.Fatalf("shown=%d:\n%s", shown, text)
		}
		if !strings.Contains(text, "\n• #1 ·") || strings.Index(text, "\n• #1 ·") > strings.Index(text, "\n• #2 ·") {
			t.Fatalf("not oldest first:\n%s", text)
		}
		if console != "" && !strings.HasSuffix(text, "\n"+console+"/ui/decisions?tab=open") {
			t.Fatalf("link:\n%s", text)
		}
	}
	few := store.DecisionDigest{Day: d.Day, Total: 2, Items: d.Items[:2]}
	if text := decisionDigestText(few, "", now); strings.Contains(text, "more") || strings.Count(text, "\n• #") != 2 {
		t.Fatalf("few:\n%s", text)
	}
}

func TestDecisionNoticeTextFitsAndShowsEachClose(t *testing.T) {
	now := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	task, due, answeredAt := int64(45), now.Add(24*time.Hour), now.Add(time.Hour)
	open := store.DecisionNotice{TaskTitle: "decide the thing", Record: store.DecisionRecord{
		ID: 12, Source: store.DecisionSourceTaskRequest, Lane: "notify-lane", TaskID: &task, Status: store.DecisionStatusOpen,
		Question: strings.Repeat("가", 3000), CreatedAt: now.Add(-5 * time.Hour), DueAt: &due,
	}}
	text := decisionNoticeText(open, "https://console.example", now, "")
	if n := utf8.RuneCountInString(text); n > discordContentMaxRunes {
		t.Fatalf("open message is %d runes", n)
	}
	for _, want := range []string{"**Decision request #12**", "source task_request", "lane notify-lane", "task #45: decide the thing", "open 5h", "<t:", "due 2026-11-06 12:00 KST", "…"} {
		if !strings.Contains(text, want) {
			t.Errorf("open message lacks %q", want)
		}
	}
	if !strings.HasSuffix(text, "\nhttps://console.example/ui/decisions/12") {
		t.Errorf("open message does not end with its own link:\n%s", text)
	}
	answered := open
	answered.Record.Status, answered.Record.AnsweredBy, answered.Record.AnsweredAt, answered.Record.Answer = store.DecisionStatusAnswered, "operator", &answeredAt, "A"
	superseded := open
	superseded.Record.Status, superseded.Record.SupersededBy, superseded.Record.UpdatedAt = store.DecisionStatusSuperseded, "task:45:done", now
	expired := open
	expired.Record.Status, expired.Record.ExpiresAt = store.DecisionStatusExpired, &now
	for want, x := range map[string]store.DecisionNotice{
		"✅ **Decision request #12** answered by operator at 2026-11-05 13:00 KST: A":    answered,
		"♻️ **Decision request #12** superseded (task:45:done) at 2026-11-05 12:00 KST": superseded,
		"⌛ **Decision request #12** expired unanswered at 2026-11-05 12:00 KST":         expired,
	} {
		text := decisionNoticeText(x, "", now, "")
		if !strings.HasPrefix(text, want) || strings.Contains(text, "가") || !strings.Contains(text, "Find request #12") || !strings.Contains(text, "task #45") {
			t.Errorf("closed message:\n%s\nwant prefix %q", text, want)
		}
	}
}

func TestDiscordWebhookPostAndEditMessage(t *testing.T) {
	discord := newFakeDiscord(t)
	w := discord.webhook()
	w.URL += "?thread_id=77"
	id, err := w.PostMessage(t.Context(), "@everyone hi")
	if err != nil || id == "" {
		t.Fatalf("post: %q %v", id, err)
	}
	if err := w.EditMessage(t.Context(), id, "@here closed"); err != nil {
		t.Fatal(err)
	}
	calls := discord.take()
	if len(calls) != 2 || calls[0].Query != "thread_id=77&wait=true" || calls[1].Method != http.MethodPatch ||
		calls[1].Path != "/api/webhooks/1/"+fakeDiscordSecret+"/messages/"+id || calls[1].Query != "thread_id=77" {
		t.Fatalf("calls=%+v", calls)
	}
	mentionsOff(t, calls)
	if err := w.EditMessage(t.Context(), "../../x", "x"); err == nil {
		t.Fatal("edit accepted a message id that is not a snowflake")
	}

	// Errors from both calls never quote the URL.
	discord.fail(func(string) int { return http.StatusTooManyRequests })
	_, postErr := w.PostMessage(t.Context(), "x")
	editErr := w.EditMessage(t.Context(), id, "x")
	closed := newFakeDiscord(t)
	gone := closed.webhook()
	closed.Close()
	_, goneErr := gone.PostMessage(t.Context(), "x")
	for name, err := range map[string]error{"post status": postErr, "edit status": editErr, "unreachable": goneErr} {
		if err == nil || strings.Contains(err.Error(), fakeDiscordSecret) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !errors.Is(decisionNoticeError(editErr), editErr) || errors.Is(decisionNoticeError(editErr), store.ErrDecisionNoticeRejected) {
		t.Errorf("429 must retry: %v", decisionNoticeError(editErr))
	}
	if !errors.Is(decisionNoticeError(discordStatusError{status: http.StatusNotFound}), store.ErrDecisionNoticeRejected) {
		t.Error("404 must not retry")
	}
}

func TestParseConsoleURL(t *testing.T) {
	if got, err := ParseConsoleURL("https://console.example/hk/"); err != nil || got != "https://console.example/hk" {
		t.Fatalf("https: %q %v", got, err)
	}
	for _, raw := range []string{"http://console.example", "console.example", "https://", "https://console.example/?x=1", "https://user:pw@console.example"} {
		if _, err := ParseConsoleURL(raw); err == nil {
			t.Errorf("%q accepted", raw)
		} else if strings.Contains(err.Error(), "console.example") {
			t.Errorf("%q: error quotes the value: %v", raw, err)
		}
	}
}

// A first post is retried only when Discord certainly did not create the
// message; an outcome the client never heard back is settled, not re-posted.
func TestDecisionPostErrorRetriesOnlyWhenNothingWasCreated(t *testing.T) {
	dial := fmt.Errorf("discord webhook: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")})
	reset := fmt.Errorf("discord webhook: %w", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")})
	for _, tc := range []struct {
		name  string
		err   error
		retry bool
	}{
		{"429", discordStatusError{status: http.StatusTooManyRequests}, true},
		{"500", discordStatusError{status: http.StatusInternalServerError}, true},
		{"dial refused", dial, true},
		{"404", discordStatusError{status: http.StatusNotFound}, false},
		{"no message id", errDiscordNoMessageID, false},
		{"read reset", reset, false},
		{"timeout", fmt.Errorf("discord webhook: %w", context.DeadlineExceeded), false},
		{"EOF", fmt.Errorf("discord webhook: %w", io.EOF), false},
		{"unreadable 2xx", errors.New("discord webhook response could not be read"), false},
	} {
		got := decisionPostError(tc.err)
		if rejected := errors.Is(got, store.ErrDecisionNoticeRejected); rejected == tc.retry {
			t.Errorf("%s: rejected=%v, want retry=%v (%v)", tc.name, rejected, tc.retry, got)
		}
	}
	if decisionPostError(nil) != nil {
		t.Error("nil must stay nil")
	}
}

// Discord reads the post and drops the connection without answering: the
// row settles as failed and a later drain does not post again.
func TestDecisionNotifierDoesNotRepostAfterALostAnswer(t *testing.T) {
	st, _ := notifyStoreFixture(t)
	var mu sync.Mutex
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		posts++
		mu.Unlock()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	t.Cleanup(srv.Close)
	x := chatDecision(t, st, "Q-20261105-81", "lost answer")
	start := time.Now().UTC()
	n := DecisionNotifier{Store: st, Discord: &DiscordWebhook{URL: srv.URL + "/api/webhooks/1/" + fakeDiscordSecret, HTTP: srv.Client()}, Now: clockAt(start.Add(2 * time.Minute))}
	n.Drain(t.Context())
	state := noticeOf(t, st, x.ID)
	if state.State != store.DecisionNoticeFailed || state.MessageID != "" {
		t.Fatalf("after a lost answer: %+v", state)
	}
	n.Now = clockAt(start.Add(3 * time.Hour))
	n.Drain(t.Context())
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatalf("posts=%d, want 1", posts)
	}
}

// The outcome of a send is recorded even when the worker context is canceled
// right after Discord answered (shutdown), so the next start does not post
// the same request again.
func TestDecisionNoticeSettlesAfterCancel(t *testing.T) {
	st, _ := notifyStoreFixture(t)
	x := chatDecision(t, st, "Q-20261105-82", "cancel after send")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	at := time.Now().UTC().Add(2 * time.Minute)
	found, sendErr, err := st.ProcessNextDecisionNotice(ctx, at, func(context.Context, store.DecisionNotice) (string, error) {
		cancel()
		return "123456789012345678", nil
	})
	if !found || sendErr != nil || err != nil {
		t.Fatalf("found=%v sendErr=%v err=%v", found, sendErr, err)
	}
	if state := noticeOf(t, st, x.ID); state.MessageID != "123456789012345678" || state.AppliedVersion != store.DecisionStatusOpen {
		t.Fatalf("not settled after cancel: %+v", state)
	}
}

// testOperatorID is a Discord user id built at run time, so no id-shaped
// literal sits in the source.
func testOperatorID() string { return "1234567890" + "12345678" }

// A first post the store marked for mention starts with the operator's
// mention and lets Discord ping that user only. Any other post or edit, a
// notifier without an operator id, and the Linear flow's send ping nobody.
func TestDecisionNoticeMentionsTheOperatorOnPressingFirstPosts(t *testing.T) {
	discord := newFakeDiscord(t)
	op := testOperatorID()
	now := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	open := store.DecisionNotice{Record: store.DecisionRecord{ID: 42, Source: store.DecisionSourceTaskRequest, Status: store.DecisionStatusOpen, Question: "ship?", CreatedAt: now}}
	pressing := open
	pressing.Mention = true
	edit := pressing
	edit.AppliedVersion, edit.MessageID = store.DecisionStatusOpen, "1001"
	edit.Record.Status = store.DecisionStatusAnswered
	with := DecisionNotifier{Discord: discord.webhook(), ConsoleURL: "https://console.example", OperatorUserID: op}
	without := DecisionNotifier{Discord: discord.webhook(), ConsoleURL: "https://console.example"}

	pressing.Record.Question = strings.Repeat("가", 3000)
	if _, err := with.sendNotice(t.Context(), pressing, now); err != nil {
		t.Fatal(err)
	}
	calls := discord.take()
	if len(calls) == 1 && (utf8.RuneCountInString(calls[0].Content) > discordContentMaxRunes || !strings.HasSuffix(calls[0].Content, "\nhttps://console.example/ui/decisions/42")) {
		t.Fatalf("long pressing post lost its link or overflowed (%d runes):\n%s", utf8.RuneCountInString(calls[0].Content), calls[0].Content)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0].Content, "<@"+op+"> ") || len(calls[0].Parse) != 0 ||
		len(calls[0].Users) != 1 || calls[0].Users[0] != op {
		t.Fatalf("pressing first post: %+v", calls)
	}
	for name, tc := range map[string]struct {
		n DecisionNotifier
		x store.DecisionNotice
	}{
		"ordinary post":     {with, open},
		"edit":              {with, edit},
		"no operator id":    {without, pressing},
		"no operator, edit": {without, edit},
	} {
		if _, err := tc.n.sendNotice(t.Context(), tc.x, now); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		calls := discord.take()
		if len(calls) != 1 || strings.Contains(calls[0].Content, "<@") || calls[0].Users != nil {
			t.Errorf("%s: %+v", name, calls)
		}
		mentionsOff(t, calls)
	}
	if err := discord.webhook().Post(t.Context(), "<@"+op+"> linear flow"); err != nil {
		t.Fatal(err)
	}
	calls = discord.take()
	if len(calls) != 1 || calls[0].Users != nil {
		t.Fatalf("linear flow send: %+v", calls)
	}
	mentionsOff(t, calls)
}

func TestParseDiscordOperatorUserID(t *testing.T) {
	op := testOperatorID()
	if got, err := ParseDiscordOperatorUserID(op); err != nil || got != op {
		t.Fatalf("%q %v", got, err)
	}
	for _, raw := range []string{"1234", op + "123", "<@" + op + ">", "12345678901234567x"} {
		if _, err := ParseDiscordOperatorUserID(raw); err == nil {
			t.Errorf("%q accepted", raw)
		} else if strings.Contains(err.Error(), raw) {
			t.Errorf("%q: error quotes the value: %v", raw, err)
		}
	}
}

// The store marks a first post for mention when the request blocks its task,
// is human_only, or is due within DecisionNoticeNearDue (or past due); an
// ordinary or far-due request is posted without one.
func TestDecisionDeliveryWorkerMentionsPressingRequests(t *testing.T) {
	st, task := decisionStoreFixture(t)
	discord := newFakeDiscord(t)
	op := testOperatorID()
	now := time.Now().UTC()
	soon, far, past := now.Add(time.Hour), now.Add(5*time.Hour), now.Add(-time.Hour)
	want := map[string]bool{}
	for _, tc := range []struct {
		title   string
		in      store.DecisionRequestInput
		mention bool
	}{
		{"ordinary", store.DecisionRequestInput{}, false},
		{"far due", store.DecisionRequestInput{DueAt: &far}, false},
		{"blocking", store.DecisionRequestInput{Block: true}, true},
		{"human only", store.DecisionRequestInput{HumanOnly: true}, true},
		{"near due", store.DecisionRequestInput{DueAt: &soon}, true},
		{"past due", store.DecisionRequestInput{DueAt: &past}, true},
	} {
		x, err := st.CreateTask(t.Context(), store.Task{Lane: "decision-lane", Title: tc.title, Kind: "implement", CreatedBy: "t", Project: task.Project})
		if err != nil {
			t.Fatal(err)
		}
		in := askInput()
		in.Question, in.DueAt, in.Block, in.HumanOnly = "ship "+tc.title+"?", tc.in.DueAt, tc.in.Block, tc.in.HumanOnly
		if _, err := st.RecordDecisionRequest(t.Context(), x.ID, "test-answerer", in); err != nil {
			t.Fatalf("%s: %v", tc.title, err)
		}
		want[fmt.Sprintf("task #%d: %s", x.ID, tc.title)] = tc.mention
	}
	n := DecisionNotifier{Store: st, Discord: discord.webhook(), OperatorUserID: op, Now: clockAt(now.Add(2 * time.Minute))}
	n.Drain(t.Context())
	posts := discord.take()
	if len(posts) != len(want) {
		t.Fatalf("posts=%+v", posts)
	}
	for _, p := range posts {
		matched := false
		for key, mention := range want {
			if !strings.Contains(p.Content, key) {
				continue
			}
			matched = true
			got := strings.HasPrefix(p.Content, "<@"+op+"> ") && len(p.Users) == 1 && p.Users[0] == op
			if got != mention || (!mention && p.Users != nil) {
				t.Errorf("%s: mention=%v want %v: %+v", key, got, mention, p)
			}
		}
		if !matched {
			t.Errorf("unexpected post: %+v", p)
		}
	}
}

// A send that keeps failing retries with backoff and the 32nd failure gives
// up: the row settles as failed and is never sent again.
func TestDecisionDeliveryWorkerGivesUpAfterMaxAttempts(t *testing.T) {
	st, conn := notifyStoreFixture(t)
	discord := newFakeDiscord(t)
	discord.fail(func(string) int { return http.StatusInternalServerError })
	x := chatDecision(t, st, "Q-20261105-91", "give up")
	start := time.Now().UTC()
	n := DecisionNotifier{Store: st, Discord: discord.webhook(), Now: clockAt(start.Add(2 * time.Minute))}
	n.Drain(t.Context())
	if state := noticeOf(t, st, x.ID); state.State != store.DecisionDeliveryPending || state.Attempts != 1 || !state.NextAttemptAt.After(start.Add(2*time.Minute)) {
		t.Fatalf("after the first failure: %+v", state)
	}
	if _, err := conn.Exec(t.Context(), `UPDATE decision_deliveries SET attempts=30 WHERE request_id=$1`, x.ID); err != nil {
		t.Fatal(err)
	}
	n.Now = clockAt(start.Add(3 * time.Hour))
	n.Drain(t.Context())
	if state := noticeOf(t, st, x.ID); state.State != store.DecisionDeliveryPending || state.Attempts != 31 {
		t.Fatalf("31st failure: %+v", state)
	}
	n.Now = clockAt(start.Add(6 * time.Hour))
	n.Drain(t.Context())
	state := noticeOf(t, st, x.ID)
	if state.State != store.DecisionNoticeFailed || state.Attempts != 32 || state.LastErrorCode != "discord webhook status 500" || state.AppliedVersion != "" {
		t.Fatalf("32nd failure: %+v", state)
	}
	discord.take()
	n.Now = clockAt(start.Add(48 * time.Hour))
	n.Drain(t.Context())
	for _, c := range discord.take() {
		if strings.HasPrefix(c.Content, "❓") {
			t.Fatalf("failed row sent again: %+v", c)
		}
	}
}

// A request message links its own console page, the digest the open tab;
// without a console URL both say where to look instead.
func TestDecisionLinkLine(t *testing.T) {
	now := time.Date(2026, 11, 5, 3, 0, 0, 0, time.UTC)
	x := store.DecisionNotice{Record: store.DecisionRecord{ID: 42, Source: store.DecisionSourceChat, Status: store.DecisionStatusOpen, Question: "q", CreatedAt: now}}
	d := store.DecisionDigest{Day: now, Total: 1, Items: []store.DecisionNotice{x}}
	if text := decisionNoticeText(x, "https://console.example", now, ""); !strings.Contains(text, "https://console.example/ui/decisions/42") {
		t.Errorf("request:\n%s", text)
	}
	if text := decisionDigestText(d, "https://console.example", now); !strings.Contains(text, "https://console.example/ui/decisions?tab=open") {
		t.Errorf("digest:\n%s", text)
	}
	if text := decisionNoticeText(x, "", now, ""); !strings.Contains(text, "Find request #42 on the console's Decisions page or with `handoffkeep decisions list`.") || strings.Contains(text, "/ui/") {
		t.Errorf("request without console:\n%s", text)
	}
	if text := decisionDigestText(d, "", now); !strings.Contains(text, "Find them on the console's Decisions page or with `handoffkeep decisions list`.") || strings.Contains(text, "/ui/") {
		t.Errorf("digest without console:\n%s", text)
	}
}

func TestDecisionDiscordTemplate(t *testing.T) {
	st, task := decisionStoreFixture(t)
	q := `Approve deployment {"identity":{"environment_known":true,"config":"x","payload":"y"},"host":"mbp","repo":"mgh3326/handoffkeep","previous":"unknown","release":"d4b91c069","paths":[],"reasons":["missing or invalid previous deployment receipt"]}`
	_, err := st.RecordDecisionRequest(t.Context(), task.ID, "mergewake-design", store.DecisionRequestInput{Question: q, Options: store.DecisionOptions{Options: []store.DecisionOption{{Key: "A", Label: "Approve this deployment"}, {Key: "B", Label: "Keep the current version"}}}, DefaultAction: "hold"})
	if err != nil {
		t.Fatal(err)
	}
	discord := newFakeDiscord(t)
	n := DecisionNotifier{Store: st, Discord: discord.webhook(), Now: clockAt(time.Now().UTC().Add(2 * time.Minute))}
	n.Drain(t.Context())
	posts := discord.take()
	if len(posts) != 1 {
		t.Fatalf("posts=%+v", posts)
	}
	for _, want := range []string{"handoffkeep d4b91c0", "대상 호스트: mbp", "이전 배포 기록이 없음", "A: 이 배포 승인 (권고)", "hk 규칙"} {
		if !strings.Contains(posts[0].Content, want) {
			t.Errorf("missing %q in %q", want, posts[0].Content)
		}
	}
}
