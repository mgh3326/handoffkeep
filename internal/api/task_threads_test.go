package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type forumCall struct {
	ThreadID   string // the thread_id query; "" for a create
	ThreadName string
	Content    string
	Users      []any
	Flags      int
	Status     int
}

// fakeForum stands in for a forum channel webhook: a post with thread_name
// creates a thread whose id is its first message's; a post with ?thread_id=
// adds a message to it. answer, when set, picks a failing status for call n
// (0-based); 0 answers normally.
type fakeForum struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []forumCall
	nextID int
	answer func(n int, w http.ResponseWriter) int
}

func newFakeForum(t *testing.T) *fakeForum {
	t.Helper()
	f := &fakeForum{nextID: 5000}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Content         string         `json:"content"`
			ThreadName      string         `json:"thread_name"`
			Flags           int            `json:"flags"`
			AllowedMentions map[string]any `json:"allowed_mentions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		users, _ := body.AllowedMentions["users"].([]any)
		f.mu.Lock()
		defer f.mu.Unlock()
		call := forumCall{ThreadID: r.URL.Query().Get("thread_id"), ThreadName: body.ThreadName, Content: body.Content, Users: users, Flags: body.Flags, Status: http.StatusOK}
		if f.answer != nil {
			if code := f.answer(len(f.calls), w); code != 0 {
				call.Status = code
				f.calls = append(f.calls, call)
				w.WriteHeader(code)
				return
			}
		}
		f.calls = append(f.calls, call)
		if r.URL.Query().Get("wait") != "true" || !strings.HasSuffix(r.URL.Path, fakeDiscordSecret) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.nextID++
		id := fmt.Sprint(f.nextID)
		channel := call.ThreadID
		if channel == "" {
			channel = id
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "channel_id": channel})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeForum) webhook() *DiscordWebhook {
	return &DiscordWebhook{URL: f.URL + "/api/webhooks/1/" + fakeDiscordSecret, HTTP: f.Client()}
}

func (f *fakeForum) take() []forumCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// delivered is the calls Discord accepted.
func delivered(calls []forumCall) []forumCall {
	var out []forumCall
	for _, c := range calls {
		if c.Status == http.StatusOK {
			out = append(out, c)
		}
	}
	return out
}

// testClock is a clock tests advance; Sleep advances it too.
type testClock struct {
	mu     sync.Mutex
	at     time.Time
	sleeps []time.Duration
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func (c *testClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.at = c.at.Add(d)
	return nil
}

const threadOperator = "123456789012345678"

func threadPoster(st TaskThreadStore, f *fakeForum, clock *testClock) *TaskThreadPoster {
	return &TaskThreadPoster{Store: st, Discord: f.webhook(), Projects: []string{"handoffkeep"}, ConsoleURL: "https://console.example",
		OperatorUserID: threadOperator, Now: clock.Now, Sleep: clock.Sleep}
}

// threadTask creates a task in project and starts it.
func threadTask(t *testing.T, st *store.Store, project, title string) store.Task {
	t.Helper()
	ctx := t.Context()
	if _, err := st.AddTaskProject(ctx, project, "tester"); err != nil {
		t.Fatal(err)
	}
	p := project
	x, err := st.CreateTask(ctx, store.Task{Lane: "thread-lane", Title: title, Kind: "implement", Priority: 5, CreatedBy: "tester", Project: &p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.TransitionTask(ctx, x.ID, store.StateDeveloping, "tester", "start", nil, "started by the thread test"); err != nil {
		t.Fatal(err)
	}
	return x
}

func taskEvent(t *testing.T, st *store.Store, in store.TaskSourceEvent) {
	t.Helper()
	if _, err := st.ApplyTaskSourceEvent(t.Context(), in); err != nil {
		t.Fatalf("event %+v: %v", in, err)
	}
}

const threadHead = "2222222222222222222222222222222222222222"

func TestTaskThreadFlowOneThreadLinesInOrder(t *testing.T) {
	st, _ := notifyStoreFixture(t)
	ctx := t.Context()
	f := newFakeForum(t)
	clock := &testClock{at: time.Now().UTC()}
	p := threadPoster(st, f, clock)
	p.Drain(ctx) // the first pass sets the epoch: nothing older gets a thread
	if calls := f.take(); len(calls) != 0 {
		t.Fatalf("first pass sent %+v", calls)
	}

	x := threadTask(t, st, "handoffkeep", "Discord threads for tasks")
	outside := threadTask(t, st, "elsewhere", "not in the project list")
	if _, err := st.CreateTaskComment(ctx, x.ID, "tester", "COMMENT-BODY-NEVER-POSTED"); err != nil {
		t.Fatal(err)
	}
	p.Drain(ctx)
	calls := f.take()
	if len(calls) != 1 || calls[0].ThreadID != "" || calls[0].ThreadName != fmt.Sprintf("#%d Discord threads for tasks", x.ID) {
		t.Fatalf("create calls %+v", calls)
	}
	if !strings.Contains(calls[0].Content, "프로젝트 handoffkeep") || !strings.Contains(calls[0].Content, fmt.Sprintf("https://console.example/ui/tasks/%d", x.ID)) || calls[0].Flags != discordSuppressEmbeds {
		t.Fatalf("first message %+v", calls[0])
	}
	state, found, err := st.GetTaskThread(ctx, x.ID)
	if err != nil || !found || state.State != store.TaskThreadActive || state.ThreadID == nil {
		t.Fatalf("delivery %+v found=%v err=%v", state, found, err)
	}
	thread := *state.ThreadID

	pr := "https://github.com/example/app/pull/7"
	taskEvent(t, st, store.TaskSourceEvent{Source: store.SourceMergewakePR, SourceRef: pr, TaskID: x.ID, HeadSHA: threadHead})
	ref := store.ChangeRef{Repo: "example/app", PR: 7}
	now := time.Now().UTC()
	if _, err = st.ApplyChangeFindings(ctx, ref, store.ChangeFindingsWrite{ReviewedHead: threadHead, Round: 1, Pass: 1, Lens: "correctness", ObservedAt: now,
		Findings: []store.ChangeFindingIn{{ID: "f1", Severity: "high", Body: "FINDING-BODY-NEVER-POSTED"}, {ID: "f2", Severity: "low"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApplyChangeGate(ctx, ref, store.ChangeGateWrite{HeadSHA: threadHead, State: "blocked", Reasons: []string{"open_high"}, DecidedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = st.PutDocument(ctx, store.Document{Key: "deploy/svc/20261111T000000Z", Kind: "report", Session: "thread-test", CreatedBy: "tester",
		Body: fmt.Sprintf(`{"schema":"deploy-record/v0","service":"svc","target":"test","result":"success","included_prs":[%q],"source":"test"}`, pr)}); err != nil {
		t.Fatal(err)
	}
	taskEvent(t, st, store.TaskSourceEvent{Source: store.SourceMergewakeMerge, SourceRef: pr, TaskID: x.ID})
	if got, _, _ := st.GetTask(ctx, x.ID); got.State != store.StateDeployed {
		t.Fatalf("task state %s, want deployed", got.State)
	}
	p.Drain(ctx)
	calls = f.take()
	want := []string{"🔀 PR 열림: " + pr, "⛔ 리뷰 판정: 차단 · 미해결 지적 critical 0 · high 1 · medium 0 · low 1: " + pr, "🟣 머지됨: " + pr, "🚀 배포됨: " + pr}
	if len(calls) != len(want) {
		t.Fatalf("lines %+v, want %d", calls, len(want))
	}
	for i, c := range calls {
		if c.ThreadID != thread || !strings.HasSuffix(c.Content, want[i]) {
			t.Fatalf("line %d = %+v, want %q in thread %s", i, c, want[i], thread)
		}
		for _, leak := range []string{"NEVER-POSTED", fakeDiscordSecret} {
			if strings.Contains(c.Content, leak) {
				t.Fatalf("line %d leaks %q: %q", i, leak, c.Content)
			}
		}
	}
	// Only the blocked review mentions the operator.
	for i, c := range calls {
		mentions := strings.HasPrefix(c.Content, "<@"+threadOperator+"> ")
		if mentions != (i == 1) || (len(c.Users) == 1) != mentions {
			t.Fatalf("line %d mention=%v users=%v", i, mentions, c.Users)
		}
	}
	p.Drain(ctx)
	if again := f.take(); len(again) != 0 {
		t.Fatalf("a second drain sent %+v", again)
	}
	if _, found, err := st.GetTaskThread(ctx, outside.ID); err != nil || found {
		t.Fatalf("task outside the projects got a delivery: found=%v err=%v", found, err)
	}
}

func TestTaskThreadRetriedLineIsNotDuplicated(t *testing.T) {
	st, _ := notifyStoreFixture(t)
	ctx := t.Context()
	f := newFakeForum(t)
	clock := &testClock{at: time.Now().UTC()}
	p := threadPoster(st, f, clock)
	p.Drain(ctx)
	x := threadTask(t, st, "handoffkeep", "retry a line")
	p.Drain(ctx)
	f.take()
	pr := "https://github.com/example/app/pull/8"
	taskEvent(t, st, store.TaskSourceEvent{Source: store.SourceMergewakePR, SourceRef: pr, TaskID: x.ID})
	// Discord answers 500 to the first line post: it certainly did not take it.
	f.answer = func(n int, _ http.ResponseWriter) int {
		if n == 0 {
			return http.StatusInternalServerError
		}
		return 0
	}
	p.Drain(ctx)
	if calls := f.take(); len(calls) != 1 || calls[0].Status != http.StatusInternalServerError {
		t.Fatalf("first attempt %+v", calls)
	}
	state, _, _ := st.GetTaskThread(ctx, x.ID)
	if state.Attempts != 1 || !state.NextAttemptAt.After(clock.Now()) || state.ClaimKey != "" {
		t.Fatalf("after the 500: %+v", state)
	}
	clock.Advance(time.Hour)
	p.Drain(ctx)
	p.Drain(ctx)
	calls := delivered(f.take())
	if len(calls) != 1 || !strings.HasSuffix(calls[0].Content, "PR 열림: "+pr) {
		t.Fatalf("retried line delivered %+v, want once", calls)
	}
}

// crashOnCreate is the real store, except that the settle of the first
// thread create never commits: the worker died after Discord's 2xx.
type crashOnCreate struct {
	*store.Store
	crashed bool
}

func (c *crashOnCreate) SettleTaskThread(ctx context.Context, now time.Time, x store.TaskThreadItem, res store.TaskThreadResult) (bool, error) {
	if x.Type == store.TaskThreadCreate && !c.crashed {
		c.crashed = true
		return false, errors.New("injected crash before commit")
	}
	return c.Store.SettleTaskThread(ctx, now, x, res)
}

func TestTaskThreadCrashAfterCreateMakesNoSecondThread(t *testing.T) {
	st, _ := notifyStoreFixture(t)
	ctx := t.Context()
	f := newFakeForum(t)
	clock := &testClock{at: time.Now().UTC()}
	p := threadPoster(&crashOnCreate{Store: st}, f, clock)
	p.Drain(ctx)
	x := threadTask(t, st, "handoffkeep", "crash after the create")
	p.Drain(ctx)
	if calls := delivered(f.take()); len(calls) != 1 || calls[0].ThreadName == "" {
		t.Fatalf("create %+v", calls)
	}
	// Within the lease the claim stands and nothing is sent.
	p.Drain(ctx)
	if calls := f.take(); len(calls) != 0 {
		t.Fatalf("sent under a standing claim: %+v", calls)
	}
	clock.Advance(store.TaskThreadClaimLease + time.Second)
	taskEvent(t, st, store.TaskSourceEvent{Source: store.SourceMergewakePR, SourceRef: "https://github.com/example/app/pull/9", TaskID: x.ID})
	p.Drain(ctx)
	p.Drain(ctx)
	if calls := f.take(); len(calls) != 0 {
		t.Fatalf("after the crash the poster sent %+v", calls)
	}
	state, _, err := st.GetTaskThread(ctx, x.ID)
	if err != nil || state.State != store.TaskThreadNeedsOperator || state.ThreadID != nil || state.ClaimKey != "" {
		t.Fatalf("after recovery: %+v err=%v", state, err)
	}
}

func TestTaskThread429WaitsRetryAfter(t *testing.T) {
	st, _ := notifyStoreFixture(t)
	ctx := t.Context()
	f := newFakeForum(t)
	clock := &testClock{at: time.Now().UTC()}
	p := threadPoster(st, f, clock)
	p.Drain(ctx)
	x := threadTask(t, st, "handoffkeep", "rate limited")
	f.answer = func(n int, w http.ResponseWriter) int {
		if n == 0 {
			w.Header().Set("Retry-After", "2.5")
			return http.StatusTooManyRequests
		}
		return 0
	}
	p.Drain(ctx)
	calls := f.take()
	if len(calls) != 2 || calls[0].Status != http.StatusTooManyRequests || calls[1].Status != http.StatusOK {
		t.Fatalf("calls %+v", calls)
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != 2500*time.Millisecond {
		t.Fatalf("sleeps %v, want one of 2.5s", clock.sleeps)
	}
	state, _, _ := st.GetTaskThread(ctx, x.ID)
	if state.State != store.TaskThreadActive || state.Attempts != 0 {
		t.Fatalf("after the 429: %+v", state)
	}
}

// ---- without a database ----

// fakeThreadStore serves queued items and records settles.
type fakeThreadStore struct {
	items   []store.TaskThreadItem
	settled []store.TaskThreadResult
}

func (f *fakeThreadStore) QueueTaskThreads(context.Context, time.Time, []string) (int, error) {
	return 0, nil
}

func (f *fakeThreadStore) RecoverTaskThreadClaims(context.Context, time.Time) ([]store.TaskThreadRecovery, error) {
	return nil, nil
}

func (f *fakeThreadStore) ClaimTaskThread(context.Context, time.Time) (store.TaskThreadItem, bool, error) {
	if len(f.items) == 0 {
		return store.TaskThreadItem{}, false, nil
	}
	x := f.items[0]
	return x, true, nil
}

func (f *fakeThreadStore) SettleTaskThread(_ context.Context, _ time.Time, _ store.TaskThreadItem, res store.TaskThreadResult) (bool, error) {
	f.settled = append(f.settled, res)
	if res.Outcome != store.TaskThreadRetry {
		f.items = f.items[1:]
	}
	return false, nil
}

func lineItem(id int64) store.TaskThreadItem {
	return store.TaskThreadItem{TaskID: id, Destination: store.TaskThreadDestinationDiscord, Key: fmt.Sprintf("e:%d", id), Type: store.TaskLineMerged, ThreadID: "7000"}
}

func TestTaskThreadRateCap(t *testing.T) {
	f := newFakeForum(t)
	clock := &testClock{at: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	fs := &fakeThreadStore{items: []store.TaskThreadItem{lineItem(1), lineItem(2), lineItem(3)}}
	p := threadPoster(fs, f, clock)
	p.PerMinute = 2
	p.Drain(t.Context())
	if calls := delivered(f.take()); len(calls) != 3 {
		t.Fatalf("delivered %d, want 3", len(calls))
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != time.Minute {
		t.Fatalf("sleeps %v, want one wait of a minute before the third", clock.sleeps)
	}
}

func TestTaskThread429WithoutDatabase(t *testing.T) {
	f := newFakeForum(t)
	f.answer = func(n int, w http.ResponseWriter) int {
		if n == 0 {
			w.Header().Set("Retry-After", "3")
			return http.StatusTooManyRequests
		}
		return 0
	}
	clock := &testClock{at: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	fs := &fakeThreadStore{items: []store.TaskThreadItem{lineItem(1)}}
	p := threadPoster(fs, f, clock)
	p.Drain(t.Context())
	if len(fs.settled) != 2 || fs.settled[0].Outcome != store.TaskThreadRetry || fs.settled[0].RetryAfter != 3*time.Second || fs.settled[1].Outcome != store.TaskThreadSent {
		t.Fatalf("settled %+v", fs.settled)
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != 3*time.Second {
		t.Fatalf("sleeps %v", clock.sleeps)
	}
	for _, s := range fs.settled {
		if s.Err != nil && strings.Contains(s.Err.Error(), fakeDiscordSecret) {
			t.Fatalf("error quotes the webhook: %v", s.Err)
		}
	}
}

func TestTaskThreadCreateNeedsThreadID(t *testing.T) {
	// A forum post answered without a thread id cannot be followed up, and
	// must not be created again.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "123"})
	}))
	t.Cleanup(srv.Close)
	p := &TaskThreadPoster{Discord: &DiscordWebhook{URL: srv.URL + "/api/webhooks/1/" + fakeDiscordSecret, HTTP: srv.Client()}}
	res := p.send(t.Context(), store.TaskThreadItem{TaskID: 1, Type: store.TaskThreadCreate, Key: store.TaskThreadCreate, ThreadName: "#1 x"})
	if res.Outcome != store.TaskThreadUnknown {
		t.Fatalf("outcome %v, want unknown", res.Outcome)
	}
}

func TestTaskThreadOutcome(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want store.TaskThreadOutcome
	}{
		{nil, store.TaskThreadSent},
		{discordStatusError{status: http.StatusTooManyRequests}, store.TaskThreadRetry},
		{discordStatusError{status: http.StatusBadGateway}, store.TaskThreadRetry},
		{discordStatusError{status: http.StatusNotFound}, store.TaskThreadRejected},
		{errDiscordNoMessageID, store.TaskThreadUnknown},
		{context.DeadlineExceeded, store.TaskThreadUnknown},
	} {
		if got := taskThreadOutcome(tc.err); got != tc.want {
			t.Errorf("%v: %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestTaskThreadLineText(t *testing.T) {
	at := time.Date(2026, 11, 11, 6, 4, 0, 0, time.UTC)
	for _, tc := range []struct {
		x    store.TaskThreadItem
		want string
	}{
		{store.TaskThreadItem{Type: store.TaskLineReview, GateState: "ready", PR: "https://github.com/a/b/pull/1"}, "✅ 리뷰 판정: 통과 · 미해결 지적 critical 0 · high 0 · medium 0 · low 0: https://github.com/a/b/pull/1"},
		{store.TaskThreadItem{Type: store.TaskLineDeployFailed, Service: "hk", DeployID: 4}, "💥 배포 실패: hk (배포 #4)"},
		{store.TaskThreadItem{Type: store.TaskLineDecisionOpened, DecisionID: 12}, "❓ 결정 요청 #12 열림: https://console.example/ui/decisions/12"},
		{store.TaskThreadItem{Type: store.TaskLineDecisionAnswered, DecisionID: 12, AnsweredBy: "operator", Channel: "mcp", At: at},
			"🗳️ 결정 요청 #12 답변됨 · 답변자 operator · 경로 mcp · 2026-11-11 15:04 KST\nhttps://console.example/ui/decisions/12"},
		{store.TaskThreadItem{Type: store.TaskLineDecisionAnswered, DecisionID: 12, AnsweredBy: "operator:someone@example.com", At: at},
			"🗳️ 결정 요청 #12 답변됨 · 답변자 operator · 2026-11-11 15:04 KST\nhttps://console.example/ui/decisions/12"},
		{store.TaskThreadItem{Type: store.TaskLineMerged}, "🟣 머지됨"},
	} {
		if got := taskThreadLineText(tc.x, "https://console.example"); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.x.Type, got, tc.want)
		}
	}
	start := taskThreadStartText(store.TaskThreadItem{TaskID: 9, Project: "handoffkeep", Kind: "implement", Grade: "A", Tier: "T2"}, "")
	if start != "📌 작업 #9 시작 · 프로젝트 handoffkeep · 종류 implement · 등급 A · 티어 T2\n`handoffkeep tasks show 9`" {
		t.Fatalf("start text %q", start)
	}
}

func TestTaskThreadName(t *testing.T) {
	if got := store.TaskThreadName(42, "fix\nthe   thing"); got != "#42 fix the thing" {
		t.Fatalf("name %q", got)
	}
	long := store.TaskThreadName(42, strings.Repeat("가", 200))
	if n := len([]rune(long)); n != store.TaskThreadNameMaxRunes {
		t.Fatalf("long name has %d runes", n)
	}
}
