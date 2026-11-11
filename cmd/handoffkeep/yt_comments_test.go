package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

func TestYTCommentsModeFromEnv(t *testing.T) {
	for value, want := range map[string]store.YTCommentsMode{
		"": store.YTCommentsOff, "off": store.YTCommentsOff, " off ": store.YTCommentsOff,
		"canary": store.YTCommentsCanary, " canary ": store.YTCommentsCanary,
		"on": store.YTCommentsOn, " on\n": store.YTCommentsOn,
	} {
		t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", value)
		if got, err := ytCommentsModeFromEnv(); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", value, got, err, want)
		}
	}
	// A value the binary does not know fails closed: an error, never a mode.
	for _, value := range []string{"ON", "On", "CANARY", "true", "all", "1", "o n"} {
		t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", value)
		if got, err := ytCommentsModeFromEnv(); err == nil {
			t.Errorf("%q accepted as %q", value, got)
		}
	}
}

// HANDOFFKEEP_YT_COMMENTS_CATCHUP is off unless it is exactly "on" (hk
// 3897: the catch-up stays off until hk 3907); any other value fails closed.
func TestYTCommentsCatchUpFromEnv(t *testing.T) {
	for value, want := range map[string]bool{"": false, "off": false, " off ": false, "on": true, " on\n": true} {
		t.Setenv("HANDOFFKEEP_YT_COMMENTS_CATCHUP", value)
		if got, err := ytCommentsCatchUpFromEnv(); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"ON", "true", "1", "yes"} {
		t.Setenv("HANDOFFKEEP_YT_COMMENTS_CATCHUP", value)
		if got, err := ytCommentsCatchUpFromEnv(); err == nil {
			t.Errorf("%q accepted as %v", value, got)
		}
	}
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "on")
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_CATCHUP", "bogus")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_COMMENTS_CATCHUP") {
		t.Fatalf("an unknown catch-up value: %v", err)
	}
}

// canary or on without YouTrack credentials is a startup error, never a
// silently idle writer; off needs no YouTrack configuration at all.
func TestYTCommentsWorkerNeedsCredentialsOnlyInCanaryAndOn(t *testing.T) {
	t.Setenv("YOUTRACK_URL", "")
	t.Setenv("YOUTRACK_BOT_API_KEY", "")
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "off")
	if w, err := ytCommentsWorkerFromEnv(nil); err != nil || w != nil {
		t.Fatalf("off: worker %v err %v", w, err)
	}
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "canary")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "YOUTRACK_URL") {
		t.Fatalf("canary without credentials: %v", err)
	}
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "on")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "YOUTRACK_URL") || !strings.Contains(err.Error(), "=on") {
		t.Fatalf("on without credentials: %v", err)
	}
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "bogus")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil {
		t.Fatal("an unknown mode started")
	}
}

// `handoffkeep yt …` is the youtrack command under the name the brief uses.
func TestYTIsTheYouTrackCommand(t *testing.T) {
	err := run([]string{"yt", "nonsense"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "ops") || !strings.Contains(err.Error(), "canary") {
		t.Fatalf("yt usage: %v", err)
	}
	err = run([]string{"yt", "ops", "--state", "done"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("yt ops with a bad state: %v", err)
	}
	err = run([]string{"yt", "ops", "resolve", "4", "--not-applied"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("yt ops resolve without actor/note: %v", err)
	}
	err = run([]string{"yt", "ops", "resolve", "4", "--applied", "c-4", "--not-applied"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("yt ops resolve with both outcomes: %v", err)
	}
	err = run([]string{"yt", "ops", "resolve", "4", "--not-applied", "--adopted", "--by", "a", "--note", "n"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--adopted") {
		t.Fatalf("yt ops resolve --adopted --not-applied: %v", err)
	}
}

// ytOpsServer answers the op listing with one op of kind and records the
// resolve bodies; kind "" lists nothing.
func ytOpsServer(t *testing.T, kind string, bodies *[]map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v1/yt/ops" {
			if r.URL.Query().Get("state") != "all" || r.URL.Query().Get("after_id") != "3" || r.URL.Query().Get("limit") != "1" {
				t.Errorf("listing query %v", r.URL.Query())
			}
			ops := []any{}
			if kind != "" {
				ops = append(ops, map[string]any{"id": 4, "kind": kind, "state": "remote_unknown", "status": "pending"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ops": ops, "next_after_id": nil})
			return
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode request: %v", err)
		}
		*bodies = append(*bodies, in)
		_ = json.NewEncoder(w).Encode(map[string]any{"op_id": 4, "status": "applied", "state": "applied", "reason": "", "retry_after": "0s", "yt_id": "2-4"})
	}))
	t.Cleanup(server.Close)
	return server
}

// An issue_create resolution always states the provenance ("adopted": true
// with --adopted, false without); a comment resolution keeps the request
// shape a server that predates the field accepts (no "adopted").
func TestYTOpsResolveStatesProvenance(t *testing.T) {
	t.Setenv("HANDOFFKEEP_URL", "")
	t.Setenv("HANDOFFKEEP_TOKEN", "")
	resolve := func(server *httptest.Server, extra ...string) {
		t.Helper()
		args := append([]string{"yt", "ops", "resolve", "4", "--applied", "2-4", "--url", server.URL, "--token", "t"}, extra...)
		if err := run(args, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	var issue []map[string]any
	server := ytOpsServer(t, "issue_create", &issue)
	resolve(server, "--adopted")
	resolve(server)
	if len(issue) != 2 || issue[0]["adopted"] != true || issue[1]["adopted"] != false {
		t.Fatalf("issue bodies %v", issue)
	}
	for _, kind := range []string{"comment_append", ""} {
		var comment []map[string]any
		resolve(ytOpsServer(t, kind, &comment))
		if _, sent := comment[0]["adopted"]; len(comment) != 1 || sent {
			t.Fatalf("%q op body %v: adopted must not be sent", kind, comment)
		}
	}
}

func TestYTOpsResolveAcceptsURLAndTokenFlags(t *testing.T) {
	t.Setenv("HANDOFFKEEP_URL", "")
	t.Setenv("HANDOFFKEEP_TOKEN", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/yt/ops" {
			// The kind lookup before an --applied resolution.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ops": []any{map[string]any{"id": 4, "kind": "comment_append"}}, "next_after_id": nil})
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/yt/ops/4/resolve" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		var in map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if _, sent := in["adopted"]; in["applied"] != true || in["yt_id"] != "c-4" || sent {
			t.Errorf("request body = %v", in)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"op_id": 4, "status": "applied", "state": "applied", "reason": "", "retry_after": "0s", "yt_id": "c-4"})
	}))
	t.Cleanup(server.Close)

	var out strings.Builder
	err := run([]string{"yt", "ops", "resolve", "4", "--applied", "c-4", "--url", server.URL, "--token", "test-token"}, &out, io.Discard)
	if err != nil {
		t.Fatalf("resolve with --url/--token: %v", err)
	}
	if !strings.Contains(out.String(), "\"state\":\"applied\"") {
		t.Fatalf("resolve output = %s", out.String())
	}
}

// A call in flight at shutdown plus its settlement fits serve's stop budget.
func TestYTCommentsStopFitsServeBudget(t *testing.T) {
	if ytCommentsDispatchTimeout+ytCommentsSettleTimeout >= workerStopBudget {
		t.Fatalf("dispatch %v + settle %v is not under the %v stop budget", ytCommentsDispatchTimeout, ytCommentsSettleTimeout, workerStopBudget)
	}
}

// ytStopRemote is the fake YouTrack behind a worker that is being stopped:
// after the POST commits, afterPost runs before the reply returns.
type ytStopRemote struct {
	inner        *ytwritetest.FakeYouTrack
	posts, lists atomic.Int64
	afterPost    func()
}

func (r *ytStopRemote) CreateComment(ctx context.Context, issueID, text string) (string, error) {
	r.posts.Add(1)
	id, err := r.inner.CreateComment(ctx, issueID, text)
	if r.afterPost != nil {
		r.afterPost()
	}
	return id, err
}

func (r *ytStopRemote) ListComments(ctx context.Context, issueID string, pageSize int) ([]ytwrite.RemoteComment, error) {
	r.lists.Add(1)
	return r.inner.ListComments(ctx, issueID, pageSize)
}

// A worker stopped while it records a POST (its op row is locked by another
// transaction) returns within serve's stop budget and leaves the dispatch
// durable; the next worker finds it by the marker and never POSTs again.
func TestYTCommentsWorkerStopIsBoundedWhileSettling(t *testing.T) {
	_, url := testdb.Schema(t, "ytc_stop")
	ctx := t.Context()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	project := "ytc-stop"
	if _, err := st.AddTaskProject(ctx, project, "test"); err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, store.Task{Lane: "ytc-stop-lane", Title: "stop while settling", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	const tenant = "stop.youtrack.cloud"
	id := strconv.FormatInt(task.ID, 10)
	verified, err := st.YTCanaryCandidate(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddYTCommentCanary(ctx, store.YTCommentCanary{TaskID: task.ID, Tenant: tenant, IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: "0-2", AddedBy: "test"}, verified); err != nil {
		t.Fatal(err)
	}
	st.SetYTCommentsMode(store.YTCommentsCanary)
	if _, err := st.CreateTaskComment(ctx, task.ID, "worker", "posted while the server stops"); err != nil {
		t.Fatal(err)
	}
	ops, err := st.ListYTWriteOps(ctx, store.YTOpFilter{State: "all", TaskID: task.ID})
	if err != nil || len(ops) != 1 {
		t.Fatalf("ops %+v %v", ops, err)
	}
	opID := ops[0].ID

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	workerCtx, stop := context.WithCancel(ctx)
	defer stop()
	locked := make(chan pgx.Tx, 1)
	var stoppedAt time.Time
	remote := &ytStopRemote{inner: ytwritetest.NewFake()}
	remote.afterPost = func() {
		tx, err := conn.Begin(ctx)
		if err == nil {
			_, err = tx.Exec(ctx, `SELECT 1 FROM yt_write_ops WHERE id=$1 FOR UPDATE`, opID)
		}
		if err != nil {
			t.Errorf("lock op row: %v", err)
		}
		locked <- tx
		stoppedAt = time.Now()
		stop()
	}
	w, err := newYTCommentsWorker(st, remote, tenant, store.YTCommentsCanary, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		w.Run(workerCtx)
		close(done)
	}()
	var lockTx pgx.Tx
	select {
	case lockTx = <-locked:
	case <-time.After(30 * time.Second):
		t.Fatal("the worker never sent the op")
	}
	select {
	case <-done:
	case <-time.After(workerStopBudget + 5*time.Second):
		_ = lockTx.Rollback(context.Background())
		<-done
		t.Fatalf("the worker did not stop within %v while its settlement waited on a lock", workerStopBudget)
	}
	if took := time.Since(stoppedAt); took >= workerStopBudget {
		t.Fatalf("the worker stopped %v after its context ended, over the %v budget", took, workerStopBudget)
	}
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	op, err := st.YTWriteLedger().ByID(ctx, opID)
	if err != nil || op.State != ytwrite.StateDispatched {
		t.Fatalf("op after a stop during settlement: %+v %v; want dispatched (durable, unrecorded)", op, err)
	}

	restarted, err := newYTCommentsWorker(st, remote, tenant, store.YTCommentsCanary, false, func() time.Time { return time.Now().UTC().Add(time.Minute) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.protocol.Drive(ctx, ytCommentsPassLimit); err != nil {
		t.Fatal(err)
	}
	op, err = st.YTWriteLedger().ByID(ctx, opID)
	if err != nil || op.State != ytwrite.StateApplied || remote.posts.Load() != 1 || remote.lists.Load() != 1 {
		t.Fatalf("after restart: op %+v %v, %d POSTs, %d listings; want applied by one search and no second POST", op, err, remote.posts.Load(), remote.lists.Load())
	}
}
