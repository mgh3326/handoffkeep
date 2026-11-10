package main

import (
	"bytes"
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
		"":         store.YTCommentsOff,
		"off":      store.YTCommentsOff,
		"canary":   store.YTCommentsCanary,
		" canary ": store.YTCommentsCanary,
		"on":       store.YTCommentsOn,
		" on ":     store.YTCommentsOn,
	} {
		t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", value)
		if got, err := ytCommentsModeFromEnv(); err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", value, got, err, want)
		}
	}
	// A value the binary does not know fails closed: an error, never a mode.
	for _, value := range []string{"CANARY", "true", "all", "1", "ON"} {
		t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", value)
		if got, err := ytCommentsModeFromEnv(); err == nil {
			t.Errorf("%q accepted as %q", value, got)
		}
	}
}

// canary and on without YouTrack credentials is a startup error, never a silently
// idle writer; off needs no YouTrack configuration at all.
func TestYTCommentsWorkerNeedsCredentialsInCanaryAndOn(t *testing.T) {
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
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "YOUTRACK_URL") {
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
	w, err := newYTCommentsWorker(st, remote, tenant, store.YTCommentsCanary, nil)
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

	restarted, err := newYTCommentsWorker(st, remote, tenant, store.YTCommentsCanary, func() time.Time { return time.Now().UTC().Add(time.Minute) })
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

func TestCommentsPreflightAndBackfill(t *testing.T) {
	_, url := testdb.Schema(t, "ytc_preflight")
	ctx := t.Context()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)

	if _, err := st.AddTaskProject(ctx, "handoffkeep", "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK", ProjectID: "0-2"}); err != nil {
		t.Fatal(err)
	}

	// Task 1: verified issue on YouTrack
	t1, err := st.CreateTask(ctx, store.Task{Lane: "lane1", Title: "task 1", Kind: "implement", CreatedBy: "test", Project: stringPtr("handoffkeep")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTaskComment(ctx, t1.ID, "worker", "comment on task 1"); err != nil {
		t.Fatal(err)
	}

	// Task 2: issue 404 on YouTrack (no issue)
	t2, err := st.CreateTask(ctx, store.Task{Lane: "lane1", Title: "task 2", Kind: "implement", CreatedBy: "test", Project: stringPtr("handoffkeep")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTaskComment(ctx, t2.ID, "worker", "comment on task 2"); err != nil {
		t.Fatal(err)
	}

	// Task 3: ambiguous (task_origins names old mirror issue)
	t3, err := st.CreateTask(ctx, store.Task{Lane: "lane1", Title: "task 3", Kind: "implement", CreatedBy: "test", Project: stringPtr("handoffkeep")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTaskComment(ctx, t3.ID, "worker", "comment on task 3"); err != nil {
		t.Fatal(err)
	}

	// Task 4: unmapped project (no issue)
	if _, err := st.AddTaskProject(ctx, "unmapped", "test"); err != nil {
		t.Fatal(err)
	}
	t4, err := st.CreateTask(ctx, store.Task{Lane: "lane1", Title: "task 4", Kind: "implement", CreatedBy: "test", Project: stringPtr("unmapped")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTaskComment(ctx, t4.ID, "worker", "comment on task 4"); err != nil {
		t.Fatal(err)
	}

	id1 := strconv.FormatInt(t1.ID, 10)
	id3 := strconv.FormatInt(t3.ID, 10)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issues/HK-" + id1:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "2-" + id1,
				"idReadable": "HK-" + id1,
				"summary":    "#" + id1 + " summary of task 1",
				"project":    map[string]any{"id": "0-2", "shortName": "HK"},
			})
		case "/api/issues/HK-" + id3:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "2-" + id3,
				"idReadable": "HK-" + id3,
				"summary":    "#" + id3 + " summary of task 3",
				"project":    map[string]any{"id": "0-2", "shortName": "HK"},
			})
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	defer ts.Close()

	tenant := strings.TrimPrefix(ts.URL, "http://")
	if _, err := st.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{
		TaskID:     t3.ID,
		IssueID:    "9-old",
		DisplayRef: "OLD-3",
		Tenant:     tenant,
		Role:       "mirror",
		SyncedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HANDOFFKEEP_DB_URL", url)
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "perm:fake-token")

	// 1. Run comments-preflight
	var buf bytes.Buffer
	if err := run([]string{"yt", "comments-preflight", "--days", "30"}, &buf, io.Discard); err != nil {
		t.Fatalf("comments-preflight failed: %v", err)
	}
	var preflightOut struct {
		Days     int                      `json:"days"`
		Projects []projectPreflightReport `json:"projects"`
	}
	if err := json.Unmarshal(buf.Bytes(), &preflightOut); err != nil {
		t.Fatalf("unmarshal preflight output: %v (%s)", err, buf.String())
	}
	if preflightOut.Days != 30 {
		t.Fatalf("days = %d, want 30", preflightOut.Days)
	}
	hkRep := findProjectReport(preflightOut.Projects, "handoffkeep")
	if hkRep == nil {
		t.Fatalf("missing project handoffkeep in preflight output: %+v", preflightOut.Projects)
	}
	if hkRep.ResolveVerify != 1 || len(hkRep.ResolveVerifyIDs) != 1 || hkRep.ResolveVerifyIDs[0] != t1.ID {
		t.Errorf("hk resolve_verify = %d %v, want 1 [%d]", hkRep.ResolveVerify, hkRep.ResolveVerifyIDs, t1.ID)
	}
	if hkRep.NoIssue != 1 || len(hkRep.NoIssueIDs) != 1 || hkRep.NoIssueIDs[0] != t2.ID {
		t.Errorf("hk no_issue = %d %v, want 1 [%d]", hkRep.NoIssue, hkRep.NoIssueIDs, t2.ID)
	}
	if hkRep.Ambiguous != 1 || len(hkRep.AmbiguousIDs) != 1 || hkRep.AmbiguousIDs[0] != t3.ID {
		t.Errorf("hk ambiguous = %d %v, want 1 [%d]", hkRep.Ambiguous, hkRep.AmbiguousIDs, t3.ID)
	}
	unmappedRep := findProjectReport(preflightOut.Projects, "unmapped")
	if unmappedRep == nil || unmappedRep.NoIssue != 1 || len(unmappedRep.NoIssueIDs) != 1 || unmappedRep.NoIssueIDs[0] != t4.ID {
		t.Errorf("unmapped project report = %+v", unmappedRep)
	}

	// 2. Run comments-backfill
	var bufBackfill bytes.Buffer
	if err := run([]string{"yt", "comments-backfill", "--days", "30"}, &bufBackfill, io.Discard); err != nil {
		t.Fatalf("comments-backfill failed: %v", err)
	}
	var backfillOut struct {
		Days             int     `json:"days"`
		Verified         int     `json:"verified"`
		SkippedNoIssue   int     `json:"skipped_no_issue"`
		SkippedAmbiguous int     `json:"skipped_ambiguous"`
		VerifiedIDs      []int64 `json:"verified_ids"`
		AmbiguousIDs     []int64 `json:"ambiguous_ids"`
		NoIssueIDs       []int64 `json:"no_issue_ids"`
	}
	if err := json.Unmarshal(bufBackfill.Bytes(), &backfillOut); err != nil {
		t.Fatalf("unmarshal backfill output: %v (%s)", err, bufBackfill.String())
	}
	if backfillOut.Verified != 1 || len(backfillOut.VerifiedIDs) != 1 || backfillOut.VerifiedIDs[0] != t1.ID {
		t.Errorf("backfill verified = %d %v, want 1 [%d]", backfillOut.Verified, backfillOut.VerifiedIDs, t1.ID)
	}
	if backfillOut.SkippedNoIssue != 2 || backfillOut.SkippedAmbiguous != 1 {
		t.Errorf("backfill skipped: no_issue=%d ambiguous=%d, want 2 and 1", backfillOut.SkippedNoIssue, backfillOut.SkippedAmbiguous)
	}

	// 3. Confirm Task 1 is recorded in yt_entity_map
	row, ok, err := st.YTWriteLedger().MapGet(ctx, "task", id1)
	if err != nil || !ok || row.YTID != "2-"+id1 {
		t.Fatalf("yt_entity_map for task 1: ok=%v row=%+v err=%v", ok, row, err)
	}
}

func stringPtr(s string) *string { return &s }

func findProjectReport(reps []projectPreflightReport, name string) *projectPreflightReport {
	for i := range reps {
		if reps[i].Project == name {
			return &reps[i]
		}
	}
	return nil
}
