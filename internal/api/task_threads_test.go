package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type forumCall struct {
	Kind       string // "create", "line" or "get"
	Path       string
	ThreadID   string // the thread a line goes to; "" for a create
	ThreadName string
	Content    string
	Users      []any
	Flags      int
	Status     int
}

const (
	fakeForumID    = "700000000000000001"
	fakeGuildID    = "700000000000000002"
	fakeBotUserID  = "700000000000000003"
	fakeOtherUser  = "700000000000000004"
	fakeOtherForum = "700000000000000005"
)

// fakeForum stands in for Discord's REST API as the bot sees one forum:
// POST /channels/{forum}/threads creates a thread the bot owns, POST
// /channels/{thread}/messages adds a message, and the lookup GETs list the
// threads. Every request must carry the bot token. answer, when set, picks a
// failing status for call n (0-based) before it takes effect; 0 answers
// normally. lose answers 502 to that many creates after making the thread.
type fakeForum struct {
	*httptest.Server
	mu       sync.Mutex
	calls    []forumCall
	nextID   int
	answer   func(n int, w http.ResponseWriter) int
	lose     int
	active   []DiscordThread
	archived []DiscordThread
	// page is the archived page size (0 means Discord's own 100); endless
	// answers has_more on every page.
	page    int
	endless bool
}

// archivedAt is the archive time of archived thread i, newest first.
func archivedAt(i int) string {
	return time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Second).Format(time.RFC3339)
}

// archivedPage is the page of f.archived Discord answers for before.
func (f *fakeForum) archivedPage(before string) map[string]any {
	start := 0
	if before != "" {
		start = len(f.archived)
		for i := range f.archived {
			if archivedAt(i) < before {
				start = i
				break
			}
		}
	}
	size := f.page
	if size == 0 {
		size = 100
	}
	end := min(start+size, len(f.archived))
	threads := []map[string]any{}
	for i := start; i < end; i++ {
		t := f.archived[i]
		threads = append(threads, map[string]any{"id": t.ID, "parent_id": t.ParentID, "owner_id": t.OwnerID, "name": t.Name,
			"thread_metadata": map[string]any{"archived": true, "archive_timestamp": archivedAt(i)}})
	}
	if f.endless && len(threads) == 0 {
		threads = append(threads, map[string]any{"id": "1", "parent_id": fakeForumID, "owner_id": fakeOtherUser, "name": "filler",
			"thread_metadata": map[string]any{"archived": true, "archive_timestamp": before}})
	}
	return map[string]any{"threads": threads, "members": []any{}, "has_more": f.endless || end < len(f.archived)}
}

func newFakeForum(t *testing.T) *fakeForum {
	t.Helper()
	f := &fakeForum{nextID: 5000}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name            string         `json:"name"`
			Content         string         `json:"content"`
			Flags           int            `json:"flags"`
			AllowedMentions map[string]any `json:"allowed_mentions"`
			Message         *struct {
				Content         string         `json:"content"`
				Flags           int            `json:"flags"`
				AllowedMentions map[string]any `json:"allowed_mentions"`
			} `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Message != nil {
			body.Content, body.Flags, body.AllowedMentions = body.Message.Content, body.Message.Flags, body.Message.AllowedMentions
		}
		users, _ := body.AllowedMentions["users"].([]any)
		f.mu.Lock()
		defer f.mu.Unlock()
		call := forumCall{Kind: "get", Path: r.URL.Path, ThreadName: body.Name, Content: body.Content, Users: users, Flags: body.Flags, Status: http.StatusOK}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v10/"), "/")
		switch {
		case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "channels" && parts[2] == "threads":
			call.Kind = "create"
		case r.Method == http.MethodPost && len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages":
			call.Kind, call.ThreadID = "line", parts[1]
		}
		if f.answer != nil {
			if code := f.answer(len(f.calls), w); code != 0 {
				call.Status = code
				f.calls = append(f.calls, call)
				w.WriteHeader(code)
				return
			}
		}
		f.calls = append(f.calls, call)
		if r.Header.Get("Authorization") != "Bot "+fakeDiscordSecret || !strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot (") {
			f.calls[len(f.calls)-1].Status = http.StatusUnauthorized
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		path := strings.Join(parts, "/")
		switch {
		case call.Kind == "create":
			f.nextID++
			id := fmt.Sprint(f.nextID)
			f.active = append(f.active, DiscordThread{ID: id, ParentID: parts[1], OwnerID: fakeBotUserID, Name: body.Name})
			if f.lose > 0 {
				f.lose--
				f.calls[len(f.calls)-1].Status = http.StatusBadGateway
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			reply(map[string]any{"id": id, "parent_id": parts[1], "type": 11})
		case call.Kind == "line":
			f.nextID++
			reply(map[string]any{"id": fmt.Sprint(f.nextID), "channel_id": parts[1]})
		case r.Method == http.MethodGet && path == "users/@me":
			reply(map[string]any{"id": fakeBotUserID})
		case r.Method == http.MethodGet && path == "channels/"+fakeForumID:
			reply(map[string]any{"id": fakeForumID, "guild_id": fakeGuildID})
		case r.Method == http.MethodGet && path == "guilds/"+fakeGuildID+"/threads/active":
			reply(map[string]any{"threads": f.active, "members": []any{}})
		case r.Method == http.MethodGet && path == "channels/"+fakeForumID+"/threads/archived/public":
			reply(f.archivedPage(r.URL.Query().Get("before")))
		default:
			f.calls[len(f.calls)-1].Status = http.StatusNotFound
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeForum) bot() *DiscordBot {
	return &DiscordBot{Token: fakeDiscordSecret, BaseURL: f.URL + "/api/v10", HTTP: f.Client()}
}

// threads is the forum's active threads.
func (f *fakeForum) threads() []DiscordThread {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DiscordThread(nil), f.active...)
}

// lastThread is the newest thread's id.
func (f *fakeForum) lastThread() string {
	t := f.threads()
	return t[len(t)-1].ID
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

// ofKind is the calls of one kind.
func ofKind(calls []forumCall, kind string) []forumCall {
	var out []forumCall
	for _, c := range calls {
		if c.Kind == kind {
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

const threadOperator = "1234567890" + "12345678"

func threadPoster(st TaskThreadStore, f *fakeForum, clock *testClock) *TaskThreadPoster {
	return &TaskThreadPoster{Store: st, Discord: f.bot(), ForumID: fakeForumID, Projects: []string{"handoffkeep"}, ConsoleURL: "https://console.example",
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
	// A 429 without Retry-After on the first line post: Discord certainly did
	// not take it, and it is retried after the backoff.
	failed := false
	f.answer = func(int, http.ResponseWriter) int {
		if !failed {
			failed = true
			return http.StatusTooManyRequests
		}
		return 0
	}
	p.Drain(ctx)
	if calls := f.take(); len(calls) != 1 || calls[0].Status != http.StatusTooManyRequests {
		t.Fatalf("first attempt %+v", calls)
	}
	state, _, _ := st.GetTaskThread(ctx, x.ID)
	if state.Attempts != 1 || !state.NextAttemptAt.After(clock.Now()) || state.ClaimKey != "" {
		t.Fatalf("after the 429: %+v", state)
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

// The worker dies after Discord made the thread: once the claim expires the
// lookup finds that thread and adopts it, with no second create, and the
// task's lines go to it.
func TestTaskThreadCrashAfterCreateMakesNoSecondThread(t *testing.T) {
	st, _ := notifyStoreFixture(t)
	ctx := t.Context()
	f := newFakeForum(t)
	clock := &testClock{at: time.Now().UTC()}
	p := threadPoster(&crashOnCreate{Store: st}, f, clock)
	p.Drain(ctx)
	x := threadTask(t, st, "handoffkeep", "crash after the create")
	p.Drain(ctx)
	calls := delivered(f.take())
	if len(calls) != 1 || calls[0].Kind != "create" || calls[0].ThreadName == "" {
		t.Fatalf("create %+v", calls)
	}
	made := f.threads()[0].ID
	// Within the lease the claim stands and nothing is sent.
	p.Drain(ctx)
	if calls := f.take(); len(calls) != 0 {
		t.Fatalf("sent under a standing claim: %+v", calls)
	}
	clock.Advance(store.TaskThreadClaimLease + time.Second)
	pr := "https://github.com/example/app/pull/9"
	taskEvent(t, st, store.TaskSourceEvent{Source: store.SourceMergewakePR, SourceRef: pr, TaskID: x.ID})
	p.Drain(ctx)
	p.Drain(ctx)
	calls = f.take()
	if creates := ofKind(calls, "create"); len(creates) != 0 {
		t.Fatalf("after the crash the poster created again: %+v", creates)
	}
	lines := ofKind(calls, "line")
	if len(lines) != 1 || lines[0].ThreadID != made || !strings.HasSuffix(lines[0].Content, "PR 열림: "+pr) {
		t.Fatalf("lines after recovery %+v, want one in %s", lines, made)
	}
	state, _, err := st.GetTaskThread(ctx, x.ID)
	if err != nil || state.State != store.TaskThreadActive || state.ThreadID == nil || *state.ThreadID != made || state.ClaimKey != "" {
		t.Fatalf("after recovery: %+v err=%v", state, err)
	}
}

// A create answered 502 is looked up only after the settle delay: before
// it nothing is sent; after it a thread Discord did make is adopted, and a
// thread it did not make is created exactly once.
func TestTaskThreadUnknownCreateWaitsThenLooksUp(t *testing.T) {
	for _, made := range []bool{true, false} {
		t.Run(fmt.Sprintf("made=%v", made), func(t *testing.T) {
			st, _ := notifyStoreFixture(t)
			ctx := t.Context()
			f := newFakeForum(t)
			clock := &testClock{at: time.Now().UTC()}
			p := threadPoster(st, f, clock)
			p.Drain(ctx)
			x := threadTask(t, st, "handoffkeep", fmt.Sprintf("lost create %v", made))
			if made {
				f.lose = 1
			} else {
				failed := false
				f.answer = func(int, http.ResponseWriter) int {
					if !failed {
						failed = true
						return http.StatusBadGateway
					}
					return 0
				}
			}
			p.Drain(ctx)
			if calls := f.take(); len(calls) != 1 || calls[0].Kind != "create" || calls[0].Status != http.StatusBadGateway {
				t.Fatalf("first create %+v", calls)
			}
			state, _, _ := st.GetTaskThread(ctx, x.ID)
			if state.State != store.TaskThreadCreating || state.ClaimKey != "" || state.ThreadID != nil {
				t.Fatalf("after the 502: %+v", state)
			}
			clock.Advance(store.TaskThreadLookupDelay - time.Second)
			p.Drain(ctx)
			if calls := f.take(); len(calls) != 0 {
				t.Fatalf("sent before the settle delay: %+v", calls)
			}
			clock.Advance(2 * time.Second)
			p.Drain(ctx)
			p.Drain(ctx)
			calls := f.take()
			creates := ofKind(calls, "create")
			if made && len(creates) != 0 || !made && len(creates) != 1 {
				t.Fatalf("made=%v: creates after the lookup %+v", made, creates)
			}
			if gets := ofKind(calls, "get"); len(gets) != 4 {
				t.Fatalf("lookup calls %+v, want users/@me, the forum, active and archived", gets)
			}
			state, _, _ = st.GetTaskThread(ctx, x.ID)
			if state.State != store.TaskThreadActive || state.ThreadID == nil || *state.ThreadID != f.lastThread() || len(f.threads()) != 1 {
				t.Fatalf("after the lookup: %+v threads %+v", state, f.threads())
			}
		})
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
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "#1 x"})
	}))
	t.Cleanup(srv.Close)
	p := &TaskThreadPoster{Discord: &DiscordBot{Token: fakeDiscordSecret, BaseURL: srv.URL, HTTP: srv.Client()}, ForumID: fakeForumID}
	res, _ := p.send(t.Context(), store.TaskThreadItem{TaskID: 1, Type: store.TaskThreadCreate, Key: store.TaskThreadCreate, ThreadName: "#1 x"})
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
		{discordStatusError{status: http.StatusBadGateway}, store.TaskThreadUnknown},
		{discordStatusError{status: http.StatusInternalServerError}, store.TaskThreadUnknown},
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

// A 5xx may come from a gateway after Discord made the post: a line is not
// sent again, and a create is never repeated.
func TestTaskThread5xxIsNeverResent(t *testing.T) {
	f := newFakeForum(t)
	f.answer = func(int, http.ResponseWriter) int { return http.StatusBadGateway }
	clock := &testClock{at: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}
	create := store.TaskThreadItem{TaskID: 2, Destination: store.TaskThreadDestinationDiscord, Key: store.TaskThreadCreate, Type: store.TaskThreadCreate, ThreadName: "#2 x"}
	fs := &fakeThreadStore{items: []store.TaskThreadItem{lineItem(1), create}}
	p := threadPoster(fs, f, clock)
	p.Drain(t.Context())
	if len(fs.settled) != 2 || fs.settled[0].Outcome != store.TaskThreadUnknown || fs.settled[1].Outcome != store.TaskThreadUnknown {
		t.Fatalf("settled %+v", fs.settled)
	}
	if calls := f.take(); len(calls) != 2 {
		t.Fatalf("calls %+v, want one each", calls)
	}
}

// lookupItem is a create whose earlier attempt has an unknown outcome.
func lookupItem(id int64) store.TaskThreadItem {
	return store.TaskThreadItem{TaskID: id, Destination: store.TaskThreadDestinationDiscord, Key: store.TaskThreadCreate, Type: store.TaskThreadCreate,
		ThreadName: fmt.Sprintf("#%d lost", id), Lookup: true}
}

func TestTaskThreadBotRequests(t *testing.T) {
	f := newFakeForum(t)
	clock := &testClock{at: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
	create := store.TaskThreadItem{TaskID: 3, Destination: store.TaskThreadDestinationDiscord, Key: store.TaskThreadCreate, Type: store.TaskThreadCreate, ThreadName: "#3 bot", Project: "handoffkeep"}
	line := lineItem(3)
	line.Type, line.GateState, line.Mention = store.TaskLineReview, "blocked", true
	fs := &fakeThreadStore{items: []store.TaskThreadItem{create, line}}
	p := threadPoster(fs, f, clock)
	p.Drain(t.Context())
	calls := f.take()
	if len(calls) != 2 || len(delivered(calls)) != 2 {
		t.Fatalf("calls %+v", calls)
	}
	if c := calls[0]; c.Kind != "create" || c.Path != "/api/v10/channels/"+fakeForumID+"/threads" || c.ThreadName != "#3 bot" || c.Flags != discordSuppressEmbeds || !strings.Contains(c.Content, "프로젝트 handoffkeep") || len(c.Users) != 0 {
		t.Fatalf("create %+v", c)
	}
	if c := calls[1]; c.Kind != "line" || c.Path != "/api/v10/channels/7000/messages" || c.Flags != discordSuppressEmbeds || !strings.HasPrefix(c.Content, "<@"+threadOperator+"> ⛔") || len(c.Users) != 1 {
		t.Fatalf("line %+v", c)
	}
	if len(fs.settled) != 2 || fs.settled[0].Outcome != store.TaskThreadSent || fs.settled[0].ThreadID != f.threads()[0].ID || fs.settled[1].Outcome != store.TaskThreadSent {
		t.Fatalf("settled %+v", fs.settled)
	}
}

func TestTaskThreadBot4xxFails(t *testing.T) {
	f := newFakeForum(t)
	f.answer = func(int, http.ResponseWriter) int { return http.StatusForbidden }
	clock := &testClock{at: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
	create := store.TaskThreadItem{TaskID: 4, Destination: store.TaskThreadDestinationDiscord, Key: store.TaskThreadCreate, Type: store.TaskThreadCreate, ThreadName: "#4 x"}
	fs := &fakeThreadStore{items: []store.TaskThreadItem{create, lineItem(4)}}
	p := threadPoster(fs, f, clock)
	p.Drain(t.Context())
	if len(fs.settled) != 2 || fs.settled[0].Outcome != store.TaskThreadRejected || fs.settled[1].Outcome != store.TaskThreadRejected {
		t.Fatalf("settled %+v", fs.settled)
	}
	for _, s := range fs.settled {
		if s.Err == nil || strings.Contains(s.Err.Error(), fakeDiscordSecret) || !strings.Contains(s.Err.Error(), "discord bot status 403") {
			t.Fatalf("error %v", s.Err)
		}
	}
}

// A create with an unknown earlier outcome searches the forum first.
func TestTaskThreadLookupOnTheBot(t *testing.T) {
	theirs := DiscordThread{ID: "8001", ParentID: fakeForumID, OwnerID: fakeOtherUser, Name: "#5 lost"}
	elsewhere := DiscordThread{ID: "8002", ParentID: fakeOtherForum, OwnerID: fakeBotUserID, Name: "#5 lost"}
	mine := DiscordThread{ID: "8003", ParentID: fakeForumID, OwnerID: fakeBotUserID, Name: "#5 lost"}
	mine2 := DiscordThread{ID: "8004", ParentID: fakeForumID, OwnerID: fakeBotUserID, Name: "#5 lost"}
	for _, tc := range []struct {
		name             string
		active, archived []DiscordThread
		forbid           bool
		want             store.TaskThreadOutcome
		creates          int
		thread           string
	}{
		{name: "one candidate is adopted", active: []DiscordThread{theirs, mine}, want: store.TaskThreadSent, thread: "8003"},
		{name: "an archived candidate is adopted", archived: []DiscordThread{mine}, want: store.TaskThreadSent, thread: "8003"},
		{name: "none creates once", want: store.TaskThreadSent, creates: 1},
		{name: "another owner or forum is ignored", active: []DiscordThread{theirs, elsewhere}, want: store.TaskThreadSent, creates: 1},
		{name: "two candidates need the operator", active: []DiscordThread{mine}, archived: []DiscordThread{mine2}, want: store.TaskThreadAmbiguous},
		{name: "a forbidden lookup needs the operator", forbid: true, want: store.TaskThreadAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeForum(t)
			f.active, f.archived = tc.active, tc.archived
			if tc.forbid {
				f.answer = func(n int, _ http.ResponseWriter) int {
					if n == 2 {
						return http.StatusForbidden // the active-threads list
					}
					return 0
				}
			}
			clock := &testClock{at: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
			fs := &fakeThreadStore{items: []store.TaskThreadItem{lookupItem(5)}}
			p := threadPoster(fs, f, clock)
			p.Drain(t.Context())
			calls := f.take()
			if got := len(ofKind(calls, "create")); got != tc.creates {
				t.Fatalf("creates %d, want %d: %+v", got, tc.creates, calls)
			}
			if len(fs.settled) != 1 || fs.settled[0].Outcome != tc.want {
				t.Fatalf("settled %+v, want %v", fs.settled, tc.want)
			}
			if tc.thread != "" && fs.settled[0].ThreadID != tc.thread {
				t.Fatalf("adopted %q, want %q", fs.settled[0].ThreadID, tc.thread)
			}
			if tc.creates == 1 && fs.settled[0].ThreadID != f.lastThread() {
				t.Fatalf("created thread %q not settled", fs.settled[0].ThreadID)
			}
		})
	}
}

// The bot's user id and the forum's guild are read once per process.
func TestDiscordBotCachesUserAndGuild(t *testing.T) {
	f := newFakeForum(t)
	b := f.bot()
	for range 2 {
		if _, _, err := b.ForumThreads(t.Context(), fakeForumID); err != nil {
			t.Fatal(err)
		}
	}
	var paths []string
	for _, c := range f.take() {
		paths = append(paths, c.Path)
	}
	want := []string{"/api/v10/users/@me", "/api/v10/channels/" + fakeForumID, "/api/v10/guilds/" + fakeGuildID + "/threads/active", "/api/v10/channels/" + fakeForumID + "/threads/archived/public",
		"/api/v10/guilds/" + fakeGuildID + "/threads/active", "/api/v10/channels/" + fakeForumID + "/threads/archived/public"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Fatalf("paths %v\nwant %v", paths, want)
	}
}

func TestDecideLostCreate(t *testing.T) {
	const forum, bot, name = "100", "200", "#9 task"
	mine := func(id string) DiscordThread { return DiscordThread{ID: id, ParentID: forum, OwnerID: bot, Name: name} }
	lookupErr := discordStatusError{status: http.StatusForbidden, api: discordBotAPI}
	for _, tc := range []struct {
		name    string
		bot     string
		threads []DiscordThread
		err     error
		want    lostCreateAction
		id      string
	}{
		{"none", bot, nil, nil, lostCreateCreate, ""},
		{"one", bot, []DiscordThread{mine("301")}, nil, lostCreateAdopt, "301"},
		{"the same thread listed twice", bot, []DiscordThread{mine("301"), mine("301")}, nil, lostCreateAdopt, "301"},
		{"two", bot, []DiscordThread{mine("301"), mine("302")}, nil, lostCreateNeedsOperator, ""},
		{"another owner", bot, []DiscordThread{{ID: "303", ParentID: forum, OwnerID: "999", Name: name}}, nil, lostCreateCreate, ""},
		{"another forum", bot, []DiscordThread{{ID: "304", ParentID: "101", OwnerID: bot, Name: name}}, nil, lostCreateCreate, ""},
		{"another name", bot, []DiscordThread{{ID: "305", ParentID: forum, OwnerID: bot, Name: name + " (2)"}}, nil, lostCreateCreate, ""},
		{"decoys and one", bot, []DiscordThread{{ID: "303", ParentID: forum, OwnerID: "999", Name: name}, mine("306")}, nil, lostCreateAdopt, "306"},
		{"lookup failed", bot, nil, lookupErr, lostCreateNeedsOperator, ""},
		{"lookup failed despite a candidate", bot, []DiscordThread{mine("301")}, lookupErr, lostCreateNeedsOperator, ""},
		{"no bot id", "", []DiscordThread{mine("301")}, nil, lostCreateNeedsOperator, ""},
		{"candidate without a valid id", bot, []DiscordThread{mine("x1")}, nil, lostCreateNeedsOperator, ""},
	} {
		got := decideLostCreate(forum, name, tc.bot, tc.threads, tc.err)
		if got.action != tc.want || got.threadID != tc.id || (got.action == lostCreateNeedsOperator) != (got.err != nil) {
			t.Errorf("%s: %+v, want action %d id %q", tc.name, got, tc.want, tc.id)
		}
	}
}

// No log line or settled error quotes the bot token, whatever Discord
// answers.
func TestTaskThreadLogsNeverQuoteToken(t *testing.T) {
	var buf strings.Builder
	var mu sync.Mutex
	log.SetOutput(writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(b)
	}))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	// A 429 retries forever against this fake store; its errors are the
	// same discordStatusError as the 403's.
	for _, code := range []int{0, http.StatusForbidden, http.StatusBadGateway} {
		f := newFakeForum(t)
		f.answer = func(int, http.ResponseWriter) int { return code }
		clock := &testClock{at: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
		create := store.TaskThreadItem{TaskID: 6, Destination: store.TaskThreadDestinationDiscord, Key: store.TaskThreadCreate, Type: store.TaskThreadCreate, ThreadName: "#6 x"}
		fs := &fakeThreadStore{items: []store.TaskThreadItem{create, lookupItem(7), lineItem(8)}}
		p := threadPoster(fs, f, clock)
		p.Drain(t.Context())
		for _, s := range fs.settled {
			if s.Err != nil && strings.Contains(s.Err.Error(), fakeDiscordSecret) {
				t.Fatalf("status %d: settled error quotes the token: %v", code, s.Err)
			}
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(buf.String(), "discord.task_thread_") {
		t.Fatalf("no task thread log lines captured: %q", buf.String())
	}
	if strings.Contains(buf.String(), fakeDiscordSecret) {
		t.Fatalf("a log line quotes the token: %q", buf.String())
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(b []byte) (int, error) { return w(b) }

// A lookup that Discord rate-limits did not run: it waits Retry-After and is
// tried again, instead of stopping for the operator.
func TestTaskThreadLookup429Retries(t *testing.T) {
	f := newFakeForum(t)
	f.active = []DiscordThread{{ID: "8003", ParentID: fakeForumID, OwnerID: fakeBotUserID, Name: "#5 lost"}}
	limited := false
	f.answer = func(_ int, w http.ResponseWriter) int {
		if !limited {
			limited = true
			w.Header().Set("Retry-After", "2")
			return http.StatusTooManyRequests
		}
		return 0
	}
	clock := &testClock{at: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
	fs := &fakeThreadStore{items: []store.TaskThreadItem{lookupItem(5)}}
	p := threadPoster(fs, f, clock)
	p.Drain(t.Context())
	if len(fs.settled) != 2 || fs.settled[0].Outcome != store.TaskThreadRetry || fs.settled[0].RetryAfter != 2*time.Second ||
		fs.settled[1].Outcome != store.TaskThreadSent || fs.settled[1].ThreadID != "8003" {
		t.Fatalf("settled %+v", fs.settled)
	}
	if len(clock.sleeps) != 1 || clock.sleeps[0] != 2*time.Second {
		t.Fatalf("sleeps %v, want one of 2s", clock.sleeps)
	}
	if creates := ofKind(f.take(), "create"); len(creates) != 0 {
		t.Fatalf("created %+v", creates)
	}
}

// The archived list is read to its end: a candidate past the first page is
// found, and a list that does not end is no proof the thread is absent.
func TestTaskThreadLookupReadsEveryArchivedPage(t *testing.T) {
	mine := DiscordThread{ID: "8003", ParentID: fakeForumID, OwnerID: fakeBotUserID, Name: "#5 lost"}
	other := DiscordThread{ID: "8100", ParentID: fakeForumID, OwnerID: fakeOtherUser, Name: "other"}
	for _, tc := range []struct {
		name    string
		endless bool
		want    store.TaskThreadOutcome
		creates int
	}{
		{"candidate on the third page", false, store.TaskThreadSent, 0},
		{"a list that never ends", true, store.TaskThreadAmbiguous, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeForum(t)
			f.archived = []DiscordThread{other, other, other, other, mine}
			if tc.endless {
				f.archived = []DiscordThread{other, other}
			}
			f.page, f.endless = 2, tc.endless
			clock := &testClock{at: time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)}
			fs := &fakeThreadStore{items: []store.TaskThreadItem{lookupItem(5)}}
			p := threadPoster(fs, f, clock)
			p.Drain(t.Context())
			calls := f.take()
			if got := len(ofKind(calls, "create")); got != tc.creates {
				t.Fatalf("creates %d", got)
			}
			if len(fs.settled) != 1 || fs.settled[0].Outcome != tc.want {
				t.Fatalf("settled %+v", fs.settled)
			}
			if !tc.endless && fs.settled[0].ThreadID != "8003" {
				t.Fatalf("adopted %q", fs.settled[0].ThreadID)
			}
			if tc.endless && !errors.Is(fs.settled[0].Err, errDiscordLookupIncomplete) {
				t.Fatalf("err %v", fs.settled[0].Err)
			}
		})
	}
}
