package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// HANDOFFKEEP_YT_BIND and the automatic binder (hk 4492,
// design/2026-11-13/yt-comment-binding-lifecycle §6.6, §6.7): c1 arrives once,
// c2 goes by the protocol, an unbound task stays excluded, a no_issue task
// backs off and is retried, an ambiguous history refuses, and a crash between
// the publication and the enqueue finishes on the next pass.

// ytbTestRemote is the YouTrack the binder sees: the fake comment store and
// the preflight fake's issue reads.
type ytbTestRemote struct {
	*ytwritetest.FakeYouTrack
	issues youtrack.IssueReader
}

func (r ytbTestRemote) GetIssue(ctx context.Context, id string) (youtrack.Issue, error) {
	return r.issues.GetIssue(ctx, id)
}

// ytbEnv is one binder test's server, store and fakes.
type ytbEnv struct {
	st     *store.Store
	conn   *pgx.Conn
	remote ytbTestRemote
	fake   *ytwritetest.FakeYouTrack
	issues *ytpFake
	worker ytCommentsWorker
	tenant string
}

func newYTBEnv(t *testing.T, prefix string) ytbEnv {
	t.Helper()
	ctx := t.Context()
	_, dbURL := testdb.Schema(t, prefix)
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	issues := &ytpFake{issues: map[string]map[string]any{}, fail: map[string]int{}, hits: map[string]int{}}
	ts := httptest.NewServer(issues)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := youtrack.New(youtrack.Config{BaseURL: ts.URL, Token: "test-token", UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	fake := ytwritetest.NewFake()
	w, err := newYTCommentsWorker(st, ytbTestRemote{FakeYouTrack: fake, issues: client}, u.Host, store.YTCommentsOn, true, true, ytpBucketID, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ytbEnv{st: st, conn: conn, remote: ytbTestRemote{FakeYouTrack: fake, issues: client}, fake: fake, issues: issues, worker: w, tenant: u.Host}
}

// ytbTask creates a task in the mapped project with a verified-looking issue
// in the fake YouTrack.
func (e ytbEnv) task(t *testing.T, withIssue bool) int64 {
	t.Helper()
	ctx := t.Context()
	const project = "ytb"
	if _, err := e.st.AddTaskProject(ctx, project, "test"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: project, ProjectID: "0-2", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	task, err := e.st.CreateTask(ctx, store.Task{Lane: "ytb-lane", Title: "binder", Kind: "implement", CreatedBy: "test", Project: new(project)})
	if err != nil {
		t.Fatal(err)
	}
	if withIssue {
		e.issues.add(task.ID)
	}
	return task.ID
}

// comment inserts an hk comment the way a writer that does not enqueue does,
// so the test controls exactly which comments the binder sees.
func (e ytbEnv) comment(t *testing.T, task int64, body string) int64 {
	t.Helper()
	var id int64
	if err := e.conn.QueryRow(t.Context(), `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,'worker',$3) RETURNING id`,
		task, body, time.Now().UTC()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e ytbEnv) intent(t *testing.T, task int64) (state, reason string, attempts int, ok bool) {
	t.Helper()
	err := e.conn.QueryRow(t.Context(), `SELECT state, reason, attempts FROM yt_binding_intents WHERE task_id=$1`, task).Scan(&state, &reason, &attempts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", 0, false
		}
		t.Fatal(err)
	}
	return state, reason, attempts, true
}

func (e ytbEnv) binding(t *testing.T, task int64) (store.YTTaskBinding, bool) {
	t.Helper()
	b, found, err := e.st.YTTaskBinding(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	return b, found
}

func ytbPass(t *testing.T, w ytCommentsWorker, now time.Time) ytBindReport {
	t.Helper()
	rep, err := w.binder.BindPass(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestYTBindingBinderPass(t *testing.T) {
	e := newYTBEnv(t, "ytb_bind")
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)

	// c1: written before its task is bound.
	fixture := e.task(t, true)
	c1 := e.comment(t, fixture, "c1")
	// An operator unbind outlives the binder (I7).
	excluded := e.task(t, true)
	verified, err := e.st.YTCanaryCandidate(ctx, excluded)
	if err != nil {
		t.Fatal(err)
	}
	excludedBinding := store.YTTaskBinding{TaskID: excluded, Tenant: e.tenant, IssueID: "2-" + strconv.FormatInt(excluded, 10),
		IssueReadable: "HK-" + strconv.FormatInt(excluded, 10), ProjectID: "0-2", BoundBy: "operator"}
	if _, err := e.st.AddYTTaskBinding(ctx, excludedBinding, verified); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.UnbindYTTaskBinding(ctx, excluded, store.YTUnbindOptions{By: "operator", Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	e.comment(t, excluded, "after the unbind")
	// A task with no issue at all.
	noIssue := e.task(t, false)
	e.comment(t, noIssue, "no issue yet")
	// A task whose issue holds two copies of one hk comment.
	ambiguous := e.task(t, true)
	e.comment(t, ambiguous, "dup")
	e.fake.Inject("2-"+strconv.FormatInt(ambiguous, 10), "dup")
	e.fake.Inject("2-"+strconv.FormatInt(ambiguous, 10), "dup")

	rep := ytbPass(t, e.worker, now)
	if rep.Bound != 1 || rep.Enqueued != 1 || rep.Refused["no_issue"] != 1 || rep.Refused["history_ambiguous"] != 1 || rep.Errors != 0 {
		t.Fatalf("first pass %+v", rep)
	}
	if b, found := e.binding(t, fixture); !found || b.Origin != store.YTBindingAutoVerified || b.CommentsFrom != 0 || b.IssueID != "2-"+strconv.FormatInt(fixture, 10) {
		t.Fatalf("c1 task binding %+v %v", b, found)
	}
	if _, found := e.binding(t, excluded); found {
		t.Fatal("the excluded task was bound")
	}
	if state, _, _, ok := e.intent(t, excluded); !ok || state != "excluded" {
		t.Fatalf("excluded intent %q %v", state, ok)
	}
	if state, reason, attempts, ok := e.intent(t, noIssue); !ok || state != "refused" || reason != "no_issue" || attempts != 1 {
		t.Fatalf("no_issue intent %q %q %d %v", state, reason, attempts, ok)
	}
	if state, reason, _, ok := e.intent(t, ambiguous); !ok || state != "refused" || reason != "history_ambiguous" {
		t.Fatalf("ambiguous intent %q %q %v", state, reason, ok)
	}
	if hits := e.issues.hitsOf(ytpIssuePath(excluded)); hits != 0 {
		t.Fatalf("the excluded task cost %d YouTrack reads", hits)
	}

	// The c1 op is the comment's, and Drive copies it once.
	op, ok := ytbOpFor(t, e.conn, c1)
	if !ok || op.State != ytwrite.StatePrepared || op.Tenant != e.tenant || op.YTTargetID != "2-"+strconv.FormatInt(fixture, 10) {
		t.Fatalf("c1 op %+v %v", op, ok)
	}
	if _, err := e.worker.protocol.Drive(ctx, ytCommentsPassLimit); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Comments("2-" + strconv.FormatInt(fixture, 10)); len(got) != 1 {
		t.Fatalf("the issue holds %d comments after the c1 copy, want 1", len(got))
	}

	// c2, written after the binding, goes by the protocol.
	c2, err := e.st.CreateTaskComment(ctx, fixture, "worker", "c2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.worker.protocol.Drive(ctx, ytCommentsPassLimit); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.Comments("2-" + strconv.FormatInt(fixture, 10)); len(got) != 2 {
		t.Fatalf("the issue holds %d comments after c2, want 2", len(got))
	}
	if op, ok := ytbOpFor(t, e.conn, c2.ID); !ok || op.State != ytwrite.StateApplied {
		t.Fatalf("c2 op %+v %v", op, ok)
	}
	// Neither a later pass nor a restart copies c1 again. This pass is before
	// the refusals' 1 h backoff, so no intent is retried yet.
	rep = ytbPass(t, e.worker, now.Add(30*time.Minute))
	if rep.Bound != 0 || rep.Enqueued != 0 {
		t.Fatalf("a later pass %+v", rep)
	}
	if got := e.fake.Comments("2-" + strconv.FormatInt(fixture, 10)); len(got) != 2 {
		t.Fatalf("a later pass changed the issue: %d comments", len(got))
	}

	// The no_issue task gets its issue and its one retry after the backoff.
	e.issues.add(noIssue)
	rep = ytbPass(t, e.worker, now.Add(50*time.Minute))
	if rep.Bound != 0 {
		t.Fatalf("a retry came before the backoff: %+v", rep)
	}
	rep = ytbPass(t, e.worker, now.Add(time.Hour+time.Second))
	if rep.Bound != 1 || rep.Enqueued != 1 {
		t.Fatalf("the backoff retry %+v", rep)
	}
	if b, found := e.binding(t, noIssue); !found || b.Origin != store.YTBindingAutoVerified {
		t.Fatalf("no_issue binding %+v %v", b, found)
	}
}

// ytbOp is the comment's op as stored.
type ytbOp struct {
	ID         int64
	State      ytwrite.State
	Tenant     string
	YTTargetID string
}

func ytbOpFor(t *testing.T, conn *pgx.Conn, commentID int64) (ytbOp, bool) {
	t.Helper()
	var op ytbOp
	var state string
	err := conn.QueryRow(t.Context(), `SELECT o.id, o.state, o.tenant, o.yt_target_id
		FROM yt_comment_write_links w JOIN yt_write_ops o ON o.id = w.op_id WHERE w.comment_id=$1`, commentID).
		Scan(&op.ID, &state, &op.Tenant, &op.YTTargetID)
	op.State = ytwrite.State(state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ytbOp{}, false
		}
		t.Fatal(err)
	}
	return op, true
}

// A history larger than one publication copies is refused before anything is
// written; the intent keeps the class and the operator path handles it.
func TestYTBindingBinderRefusesLargeHistory(t *testing.T) {
	e := newYTBEnv(t, "ytb_large")
	now := time.Now().UTC().Truncate(time.Second)
	task := e.task(t, true)
	for i := range ytBindCopyMax + 1 {
		e.comment(t, task, "c"+strconv.Itoa(i))
	}
	rep := ytbPass(t, e.worker, now)
	if rep.Bound != 0 || rep.Enqueued != 0 || rep.Refused["history_large"] != 1 {
		t.Fatalf("the large pass %+v", rep)
	}
	if state, reason, _, ok := e.intent(t, task); !ok || state != "refused" || reason != "history_large" {
		t.Fatalf("the intent %q %q %v", state, reason, ok)
	}
	if _, found := e.binding(t, task); found {
		t.Fatal("a refused history was published")
	}
}

// An ambiguous history refuses before publication and keeps the intent; once
// the operator removes the duplicate, the publication copies the older
// comment in the same transaction as the binding.
func TestYTBindingBinderKeepsIntentWhileAmbiguous(t *testing.T) {
	e := newYTBEnv(t, "ytb_ambiguous")
	now := time.Now().UTC().Truncate(time.Second)
	task := e.task(t, true)
	older := e.comment(t, task, "c0")
	e.comment(t, task, "c1")
	issue := "2-" + strconv.FormatInt(task, 10)
	e.fake.Inject(issue, "c1")
	duplicate := e.fake.Inject(issue, "c1")
	rep := ytbPass(t, e.worker, now)
	if rep.Enqueued != 0 || rep.Refused["history_ambiguous"] != 1 {
		t.Fatalf("the ambiguous pass %+v", rep)
	}
	if state, reason, attempts, ok := e.intent(t, task); !ok || state != "refused" || reason != "history_ambiguous" || attempts != 1 {
		t.Fatalf("the intent was not kept as a durable refusal: %q %q %d %v", state, reason, attempts, ok)
	}
	if _, found := e.binding(t, task); found {
		t.Fatal("the ambiguous task was bound")
	}
	// The operator removes the duplicate; the next attempt publishes and
	// copies c0 with the binding.
	e.fake.Delete(issue, duplicate)
	rep = ytbPass(t, e.worker, now.Add(time.Hour+2*time.Minute))
	if rep.Bound != 1 || rep.Enqueued != 1 {
		t.Fatalf("the resumed pass %+v, want the binding and c0", rep)
	}
	if op, ok := ytbOpFor(t, e.conn, older); !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("c0 op %+v %v", op, ok)
	}
	if state, _, _, ok := e.intent(t, task); ok {
		t.Fatalf("the intent survived: %q", state)
	}
}

// A deferred intent that outlives an operator backfill is dropped: the
// publisher owns its comments_from and everything below it, so the binder
// never copies that history.
func TestYTBindingBinderLeavesOtherOriginsAlone(t *testing.T) {
	e := newYTBEnv(t, "ytb_origin")
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	task := e.task(t, true)
	c := e.comment(t, task, "history the backfill excludes")
	if _, err := e.st.ScanYTBindingCandidates(ctx, e.tenant, ytBindScanLimit, now); err != nil {
		t.Fatal(err)
	}
	verified, err := e.st.YTCanaryCandidate(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	b := store.YTTaskBinding{TaskID: task, Tenant: e.tenant, IssueID: "2-" + strconv.FormatInt(task, 10),
		IssueReadable: "HK-" + strconv.FormatInt(task, 10), ProjectID: "0-2", BoundBy: "operator"}
	if _, err := e.st.AddYTTaskBinding(ctx, b, verified); err != nil {
		t.Fatal(err)
	}
	rep := ytbPass(t, e.worker, now.Add(time.Minute))
	if rep.Enqueued != 0 {
		t.Fatalf("the binder copied across a backfill's watermark: %+v", rep)
	}
	if _, ok := ytbOpFor(t, e.conn, c); ok {
		t.Fatal("a comment below the backfill's watermark got an op")
	}
	if state, _, _, ok := e.intent(t, task); ok {
		t.Fatalf("the intent survived: %q", state)
	}
}

// A crash between the publication's commit and dropping the intent is
// harmless: the next pass sees the binding and drops the intent, and the
// copies committed with the binding are not written twice.
func TestYTBindingBinderDropsIntentAfterCommit(t *testing.T) {
	e := newYTBEnv(t, "ytb_crash")
	ctx := t.Context()
	now := time.Now().UTC().Truncate(time.Second)
	task := e.task(t, true)
	c1 := e.comment(t, task, "c1")
	if _, err := e.st.ScanYTBindingCandidates(ctx, e.tenant, ytBindScanLimit, now); err != nil {
		t.Fatal(err)
	}
	candidate, err := e.st.YTCanaryCandidate(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := youtrack.VerifyBinding(ctx, e.remote, e.tenant, ytpBucketID, candidate)
	if err != nil {
		t.Fatal(err)
	}
	binding.BoundBy = ytBindActor
	got, err := e.st.PublishYTAutoBinding(ctx, binding, candidate,
		store.YTAutoBindEvidence{Authority: store.YTAuthorityHK, Missing: []int64{c1}}, ytBindActor)
	if err != nil || got.Prepared != 1 {
		t.Fatalf("the publication %+v %v", got, err)
	}
	// The publication committed; the process crashed before the intent drop.
	rep := ytbPass(t, e.worker, now.Add(time.Minute))
	if rep.Bound != 0 || rep.Enqueued != 0 {
		t.Fatalf("the recovery pass %+v, want the intent dropped and nothing written", rep)
	}
	if state, _, _, ok := e.intent(t, task); ok {
		t.Fatalf("the intent survived: %q", state)
	}
	if op, ok := ytbOpFor(t, e.conn, c1); !ok || op.State != ytwrite.StatePrepared {
		t.Fatalf("c1 op %+v %v", op, ok)
	}
	var links int
	if err := e.conn.QueryRow(ctx, `SELECT count(*) FROM yt_comment_write_links WHERE comment_id=$1`, c1).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 1 {
		t.Fatalf("c1 has %d links, want exactly one", links)
	}
}

func TestYTBindModeFromEnv(t *testing.T) {
	for value, want := range map[string]bool{"": false, "off": false, " off ": false, "on": true, " on\n": true} {
		t.Setenv("HANDOFFKEEP_YT_BIND", value)
		if got, err := ytBindModeFromEnv(); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", value, got, err, want)
		}
	}
	for _, value := range []string{"ON", "true", "1", "yes"} {
		t.Setenv("HANDOFFKEEP_YT_BIND", value)
		if got, err := ytBindModeFromEnv(); err == nil {
			t.Errorf("%q accepted as %v", value, got)
		}
	}
	// The binder needs the comment gate on: it publishes bindings that gate
	// reads, so with the gate off it would bind tasks whose comments nothing
	// copies.
	t.Setenv("HANDOFFKEEP_YT_BIND", "on")
	t.Setenv("HANDOFFKEEP_YT_COMMENTS_MODE", "canary")
	t.Setenv("YOUTRACK_URL", "https://example.invalid")
	t.Setenv("YOUTRACK_BOT_API_KEY", "token")
	if _, err := ytCommentsWorkerFromEnv(nil); err == nil || !strings.Contains(err.Error(), "HANDOFFKEEP_YT_BIND") {
		t.Fatalf("bind on with comments canary: %v", err)
	}
}
