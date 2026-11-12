package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// ytpClass is one class of the comments-preflight report.
type ytpClass struct {
	Count   int     `json:"count"`
	TaskIDs []int64 `json:"task_ids"`
}

type ytpProject struct {
	Project        string   `json:"project"`
	Bound          ytpClass `json:"bound"`
	BoundAmbiguous ytpClass `json:"bound_ambiguous"`
	Verifies       ytpClass `json:"verifies"`
	NoIssue        ytpClass `json:"no_issue"`
	Ambiguous      ytpClass `json:"ambiguous"`
	Error          ytpClass `json:"error"`
}

type ytpReport struct {
	Tenant             string       `json:"tenant"`
	Days               int          `json:"days"`
	CommentsTotal      int          `json:"comments_total"`
	MaxCommentsPerHour int          `json:"max_comments_per_hour"`
	Projects           []ytpProject `json:"projects"`
	Uncovered          struct {
		Count int     `json:"count"`
		OpIDs []int64 `json:"op_ids"`
	} `json:"unresolved_ops_uncovered"`
	BoundNow *int `json:"bound_now"`
}

func (r ytpReport) project(t *testing.T, name string) ytpProject {
	t.Helper()
	for _, p := range r.Projects {
		if p.Project == name {
			return p
		}
	}
	t.Fatalf("no project %q in %+v", name, r.Projects)
	return ytpProject{}
}

func ytpIDs(t *testing.T, name string, got ytpClass, want ...int64) {
	t.Helper()
	if want == nil {
		want = []int64{}
	}
	if got.Count != len(want) || !slices.Equal(got.TaskIDs, want) {
		t.Errorf("%s = %d %v, want %v", name, got.Count, got.TaskIDs, want)
	}
}

// ytpFake is the YouTrack the commands verify against: one issue per task,
// keyed by readable id, the project list (projects; nil answers the one
// bucket project HK, id 0-1), and a hit counter per path.
type ytpFake struct {
	mu       sync.Mutex
	issues   map[string]map[string]any
	fail     map[string]int
	hits     map[string]int
	onGet    func(path string)
	projects []map[string]any
}

func (f *ytpFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.hits[r.URL.Path]++
	issue, ok := f.issues[r.URL.Path]
	status := f.fail[r.URL.Path]
	hook := f.onGet
	projects := f.projects
	f.mu.Unlock()
	if r.Method == http.MethodGet && r.URL.Path == "/api/admin/projects" {
		if projects == nil {
			projects = []map[string]any{{"id": ytpBucketID, "shortName": "HK", "name": "Unassigned hk tasks"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(projects)
		return
	}
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/api/issues/") {
		http.Error(w, `{"error":"unexpected"}`, http.StatusMethodNotAllowed)
		return
	}
	if hook != nil {
		hook(r.URL.Path)
	}
	switch {
	case status != 0:
		http.Error(w, `{"error":"unavailable"}`, status)
	case !ok:
		http.Error(w, `{"error":"Entity with id not found"}`, http.StatusNotFound)
	default:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(issue)
	}
}

func (f *ytpFake) hitsOf(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

func (f *ytpFake) resetHits() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits = map[string]int{}
}

func ytpIssuePath(task int64) string { return "/api/issues/HK-" + strconv.FormatInt(task, 10) }

// ytpBucketID is the fake's import bucket, the project HK no
// youtrack_projects row maps.
const ytpBucketID = "0-1"

func (f *ytpFake) add(task int64) {
	id := strconv.FormatInt(task, 10)
	f.issues[ytpIssuePath(task)] = map[string]any{"id": "2-" + id, "idReadable": "HK-" + id, "summary": "#" + id + " imported",
		"project": map[string]any{"id": "0-2", "shortName": "HK"}}
}

// ytpRun runs one command and decodes its report; a non-nil error is the
// command's own (an `error` class exits 1 after printing the report).
func ytpRun(t *testing.T, args ...string) (ytpReport, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, &out, io.Discard)
	var r ytpReport
	if jerr := json.Unmarshal(out.Bytes(), &r); jerr != nil {
		t.Fatalf("%v: output %q is not a report (%v; run err %v)", args, out.String(), jerr, err)
	}
	return r, err
}

// ytpSnapshot is every row the commands could write, so a read-only run is
// checked by comparing two snapshots.
func ytpSnapshot(t *testing.T, conn *pgx.Conn) string {
	t.Helper()
	var s string
	err := conn.QueryRow(t.Context(), `SELECT concat_ws('|',
		(SELECT count(*) FROM yt_task_bindings), (SELECT count(*) FROM yt_comment_canary), (SELECT count(*) FROM yt_entity_map),
		(SELECT count(*) FROM yt_comment_write_links), (SELECT count(*) FROM task_comments), (SELECT count(*) FROM task_origins),
		(SELECT string_agg(id || ':' || state || ':' || attempts, ',' ORDER BY id) FROM yt_write_ops))`).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// T10: comments-preflight classifies the candidates without writing, and
// comments-backfill binds the verified ones, counts a publication conflict
// as ambiguous and goes on, and is idempotent with no YouTrack read for a
// task already bound.
func TestCommentsPreflightAndBackfill(t *testing.T) {
	_, dbURL := testdb.Schema(t, "ytp_cmd")
	ctx := t.Context()
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
	fake := &ytpFake{issues: map[string]map[string]any{}, fail: map[string]int{}, hits: map[string]int{}}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	tenant := u.Host // the tenant the comment worker uses: the host of YOUTRACK_URL

	const project, loose = "ytp", "ytp-loose"
	for _, p := range []string{project, loose} {
		if _, err := st.AddTaskProject(ctx, p, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: project, ProjectID: "0-2", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	newTask := func(p string) int64 {
		t.Helper()
		x, err := st.CreateTask(ctx, store.Task{Lane: "ytp-lane", Title: "preflight", Kind: "implement", CreatedBy: "test", Project: &p})
		if err != nil {
			t.Fatal(err)
		}
		return x.ID
	}
	// Comments written at fixed times: five in one hour two days ago, one
	// in the hour before, one outside the window and one pulled from
	// YouTrack (never copied, so not counted).
	h1 := time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Hour)
	comment := func(task int64, at time.Time, author string) {
		t.Helper()
		if _, err := conn.Exec(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,'seeded',$2,$3)`, task, author, at); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(task int64) {
		t.Helper()
		verified, err := st.YTCanaryCandidate(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		id := strconv.FormatInt(task, 10)
		if _, err := st.AddYTTaskBinding(ctx, store.YTTaskBinding{TaskID: task, Tenant: tenant, IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: "0-2", BoundBy: "test"}, verified); err != nil {
			t.Fatal(err)
		}
	}
	enroll := func(task int64) {
		t.Helper()
		verified, err := st.YTCanaryCandidate(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		id := strconv.FormatInt(task, 10)
		if _, err := st.AddYTCommentCanary(ctx, store.YTCommentCanary{TaskID: task, Tenant: tenant, IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: "0-2", AddedBy: "test"}, verified); err != nil {
			t.Fatal(err)
		}
	}
	mirror := func(task int64, issue string) {
		t.Helper()
		if _, err := st.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{TaskID: task, IssueID: issue, DisplayRef: "OLD-" + issue, Tenant: tenant, Role: "mirror"}); err != nil {
			t.Fatal(err)
		}
	}

	race := newTask(project) // verifies, but its mirror changes during the backfill's GET
	verifies := newTask(project)
	noIssue := newTask(project)
	ambiguous := newTask(project)
	failing := newTask(project)
	unmapped := newTask(loose)
	bound := newTask(project)       // enrolled, bound, with a prepared canary op it covers
	boundAmb := newTask(project)    // bound, then its mirror moved elsewhere
	canaryOp := newTask(project)    // enrolled, a prepared canary op, no imported issue
	canaryQuiet := newTask(project) // enrolled, no recent comment: a candidate through the canary
	idle := newTask(project)        // only an old comment: not a candidate
	for _, task := range []int64{race, verifies, noIssue, ambiguous, failing} {
		comment(task, h1.Add(10*time.Minute), "worker")
	}
	comment(unmapped, h1.Add(-30*time.Minute), "worker")
	comment(boundAmb, h1.Add(20*time.Minute), store.YouTrackOwnedActor)
	comment(idle, time.Now().UTC().Add(-20*24*time.Hour), "worker")
	for _, task := range []int64{race, verifies, ambiguous, failing, bound, boundAmb} {
		fake.add(task)
	}
	fake.fail[ytpIssuePath(failing)] = http.StatusServiceUnavailable
	mirror(ambiguous, "9-"+strconv.FormatInt(ambiguous, 10))
	enroll(bound)
	enroll(canaryOp)
	enroll(canaryQuiet)
	st.SetYTCommentsMode(store.YTCommentsCanary)
	for _, task := range []int64{bound, canaryOp} {
		if _, err := st.CreateTaskComment(ctx, task, "worker", "prepared in canary"); err != nil {
			t.Fatal(err)
		}
	}
	st.SetYTCommentsMode(store.YTCommentsOff)
	var canaryOpID int64
	if err := conn.QueryRow(ctx, `SELECT id FROM yt_write_ops WHERE entity_id=$1`, strconv.FormatInt(canaryOp, 10)).Scan(&canaryOpID); err != nil {
		t.Fatal(err)
	}
	bind(bound)
	bind(boundAmb)
	mirror(boundAmb, "9-"+strconv.FormatInt(boundAmb, 10))

	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "fake-test-key")

	checkClasses := func(name string, r ytpReport, boundIDs, verifiesIDs, ambiguousIDs []int64) {
		t.Helper()
		if r.Tenant != tenant || r.Days != 14 {
			t.Errorf("%s: tenant %q days %d", name, r.Tenant, r.Days)
		}
		p := r.project(t, project)
		ytpIDs(t, name+" bound", p.Bound, boundIDs...)
		ytpIDs(t, name+" bound_ambiguous", p.BoundAmbiguous, boundAmb)
		ytpIDs(t, name+" verifies", p.Verifies, verifiesIDs...)
		ytpIDs(t, name+" no_issue", p.NoIssue, noIssue, canaryOp, canaryQuiet)
		ytpIDs(t, name+" ambiguous", p.Ambiguous, ambiguousIDs...)
		ytpIDs(t, name+" error", p.Error, failing)
		ytpIDs(t, name+" loose no_issue", r.project(t, loose).NoIssue, unmapped)
		for _, other := range r.Projects {
			for _, c := range []ytpClass{other.Bound, other.BoundAmbiguous, other.Verifies, other.NoIssue, other.Ambiguous, other.Error} {
				if slices.Contains(c.TaskIDs, idle) {
					t.Errorf("%s: task %d with only an old comment is a candidate", name, idle)
				}
			}
		}
		if r.Uncovered.Count != 1 || !slices.Equal(r.Uncovered.OpIDs, []int64{canaryOpID}) {
			t.Errorf("%s: unresolved_ops_uncovered = %+v, want op %d", name, r.Uncovered, canaryOpID)
		}
		if r.CommentsTotal != 8 || r.MaxCommentsPerHour != 5 {
			t.Errorf("%s: comments_total %d max_comments_per_hour %d, want 8 and 5", name, r.CommentsTotal, r.MaxCommentsPerHour)
		}
	}
	exitedFailed := func(name string, err error) {
		t.Helper()
		var code exitCodeError
		if !errors.As(err, &code) || code.code != youtrackExitFailed {
			t.Fatalf("%s: err %v, want exit %d for the error class", name, err, youtrackExitFailed)
		}
	}
	noRead := func(name string, tasks ...int64) {
		t.Helper()
		for _, task := range tasks {
			if n := fake.hitsOf(ytpIssuePath(task)); n != 0 {
				t.Errorf("%s: %d YouTrack reads for task %d", name, n, task)
			}
		}
	}
	bindings := func() string {
		t.Helper()
		var s string
		if err := conn.QueryRow(ctx, `SELECT coalesce(string_agg(task_id || '=' || tenant || '/' || issue_id, ',' ORDER BY task_id), '') FROM yt_task_bindings`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	bindingOf := func(task int64) string {
		id := strconv.FormatInt(task, 10)
		return id + "=" + tenant + "/2-" + id
	}

	before := ytpSnapshot(t, conn)
	r, err := ytpRun(t, "yt", "comments-preflight")
	exitedFailed("preflight", err)
	if after := ytpSnapshot(t, conn); after != before {
		t.Fatalf("preflight wrote: before %s, after %s", before, after)
	}
	if r.BoundNow != nil {
		t.Errorf("preflight reports bound_now %d", *r.BoundNow)
	}
	checkClasses("preflight", r, []int64{bound}, []int64{race, verifies}, []int64{ambiguous})
	noRead("preflight", bound, boundAmb, unmapped)
	if r, _ := ytpRun(t, "yt", "comments-preflight", "--project", loose, "--days", "3"); len(r.Projects) != 1 || r.Projects[0].Project != loose || r.Days != 3 || r.CommentsTotal != 1 {
		t.Errorf("--project %s --days 3: %+v", loose, r)
	}

	// A mirror link for the race task appears while the backfill reads its
	// issue. It agrees with the issue, so the classification still says
	// verifies, but the publication sees mappings other than the verified
	// ones and refuses: counted ambiguous, and the loop goes on.
	var armed atomic.Bool
	armed.Store(true)
	fake.mu.Lock()
	fake.onGet = func(path string) {
		if path == ytpIssuePath(race) && armed.CompareAndSwap(true, false) {
			id := strconv.FormatInt(race, 10)
			if _, err := st.UpsertYouTrackMirror(ctx, store.YouTrackMirrorInput{TaskID: race, IssueID: "2-" + id, DisplayRef: "HK-" + id, Tenant: tenant, Role: "mirror"}); err != nil {
				t.Errorf("mirror during the GET: %v", err)
			}
		}
	}
	fake.mu.Unlock()
	fake.resetHits()
	r, err = ytpRun(t, "yt", "comments-backfill")
	exitedFailed("backfill", err)
	if r.BoundNow == nil || *r.BoundNow != 1 {
		t.Fatalf("backfill bound_now = %v, want 1", r.BoundNow)
	}
	checkClasses("backfill", r, []int64{bound}, []int64{verifies}, []int64{race, ambiguous})
	noRead("backfill", bound, boundAmb, unmapped)
	if got, want := bindings(), bindingOf(verifies)+","+bindingOf(bound)+","+bindingOf(boundAmb); got != want {
		t.Fatalf("bindings after the backfill = %s, want %s", got, want)
	}

	// Again, with the mappings stable: the race task binds now.
	fake.resetHits()
	r, err = ytpRun(t, "yt", "comments-backfill")
	exitedFailed("second backfill", err)
	if r.BoundNow == nil || *r.BoundNow != 1 {
		t.Fatalf("second backfill bound_now = %v, want 1", r.BoundNow)
	}
	checkClasses("second backfill", r, []int64{verifies, bound}, []int64{race}, []int64{ambiguous})
	noRead("second backfill", verifies, bound, boundAmb, unmapped)

	// And again: nothing to bind, nothing written, and no YouTrack read for
	// a bound task.
	fake.resetHits()
	snap := ytpSnapshot(t, conn)
	r, err = ytpRun(t, "yt", "comments-backfill")
	exitedFailed("third backfill", err)
	if r.BoundNow == nil || *r.BoundNow != 0 {
		t.Fatalf("third backfill bound_now = %v, want 0", r.BoundNow)
	}
	if after := ytpSnapshot(t, conn); after != snap {
		t.Fatalf("the third backfill wrote: %s -> %s", snap, after)
	}
	checkClasses("third backfill", r, []int64{race, verifies, bound}, nil, []int64{ambiguous})
	noRead("third backfill", race, verifies, bound, boundAmb, unmapped)
	if n := fake.hitsOf(ytpIssuePath(noIssue)); n != 1 {
		t.Errorf("third backfill read the unbound task %d %d times, want 1", noIssue, n)
	}
}

// The preflight never migrates: against a schema from before this release
// (no yt_task_bindings table, no record of its migration) it refuses with
// exit 2 and asks for the deploy, makes no YouTrack read, and leaves the
// schema as it was. With store.Open in its place, the open would create the
// table and record the migration.
func TestCommentsPreflightRefusesSchemaWithoutBindings(t *testing.T) {
	_, dbURL := testdb.Schema(t, "ytp_old")
	ctx := t.Context()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	// The previous release's schema: every other migration is recorded.
	if _, err := conn.Exec(ctx, `DROP TABLE yt_task_bindings`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM schema_migrations WHERE id=$1`, store.YTTaskBindingsMigration); err != nil {
		t.Fatal(err)
	}
	schemaLeft := func() string {
		t.Helper()
		var table, recorded bool
		if err := conn.QueryRow(ctx, `SELECT to_regclass('yt_task_bindings') IS NOT NULL, EXISTS(SELECT 1 FROM schema_migrations WHERE id=$1)`,
			store.YTTaskBindingsMigration).Scan(&table, &recorded); err != nil {
			t.Fatal(err)
		}
		return "table " + strconv.FormatBool(table) + ", record " + strconv.FormatBool(recorded)
	}
	if got := schemaLeft(); got != "table false, record false" {
		t.Fatalf("fixture: %s", got)
	}
	fake := &ytpFake{issues: map[string]map[string]any{}, fail: map[string]int{}, hits: map[string]int{}}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "fake-test-key")

	var out bytes.Buffer
	err = run([]string{"yt", "comments-preflight"}, &out, io.Discard)
	var code exitCodeError
	if !errors.As(err, &code) || code.code != youtrackExitConfig || !strings.Contains(err.Error(), store.YTTaskBindingsMigration) || !strings.Contains(err.Error(), "deploy") {
		t.Fatalf("err = %v, want exit %d naming %s and the deploy", err, youtrackExitConfig, store.YTTaskBindingsMigration)
	}
	if out.Len() != 0 {
		t.Errorf("a refused preflight printed %q", out.String())
	}
	if got := schemaLeft(); got != "table false, record false" {
		t.Fatalf("the refused preflight changed the schema: %s", got)
	}
	fake.mu.Lock()
	hits := len(fake.hits)
	fake.mu.Unlock()
	if hits != 0 {
		t.Errorf("the refused preflight read YouTrack: %v", fake.hits)
	}
}

// The worker started in mode on sets the store's gate to on: a comment on a
// bound task gets its op, and the worker sends it.
func TestYTCommentsWorkerStartsInModeOn(t *testing.T) {
	_, dbURL := testdb.Schema(t, "ytp_on")
	ctx := t.Context()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	project := "ytp-on"
	if _, err := st.AddTaskProject(ctx, project, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: project, ProjectID: "0-2", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	task, err := st.CreateTask(ctx, store.Task{Lane: "ytp-on-lane", Title: "on", Kind: "implement", CreatedBy: "test", Project: &project})
	if err != nil {
		t.Fatal(err)
	}
	const tenant = "on.youtrack.cloud"
	id := strconv.FormatInt(task.ID, 10)
	verified, err := st.YTCanaryCandidate(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddYTTaskBinding(ctx, store.YTTaskBinding{TaskID: task.ID, Tenant: tenant, IssueID: "2-" + id, IssueReadable: "HK-" + id, ProjectID: "0-2", BoundBy: "test"}, verified); err != nil {
		t.Fatal(err)
	}
	// Written after the binding while the mode is still off: no op until
	// the running worker's catch-up (hk 3897).
	missed, err := st.CreateTaskComment(ctx, task.ID, "worker", "written before the worker started")
	if err != nil {
		t.Fatal(err)
	}
	remote := ytwritetest.NewFake()
	// The catch-up is off unless asked for (HANDOFFKEEP_YT_COMMENTS_CATCHUP),
	// except for bindings issue_create ops published (hk 3914).
	if off, err := newYTCommentsWorker(st, remote, tenant, store.YTCommentsOn, false, nil); err != nil {
		t.Fatal(err)
	} else if _, issueOnly := off.catchUp.(ytIssueCommentCatchUp); !issueOnly {
		t.Fatalf("worker without the catch-up: %+v %v", off, err)
	}
	w, err := newYTCommentsWorker(st, remote, tenant, store.YTCommentsOn, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTaskComment(ctx, task.ID, "worker", "sent in mode on"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.protocol.Drive(ctx, ytCommentsPassLimit); err != nil {
		t.Fatal(err)
	}
	ops, err := st.ListYTWriteOps(ctx, store.YTOpFilter{State: "all", TaskID: task.ID})
	if err != nil || len(ops) != 1 || ops[0].State != ytwrite.StateApplied || len(remote.Comments("2-"+id)) != 1 {
		t.Fatalf("mode on: ops %+v %v, remote %+v", ops, err, remote.Comments("2-"+id))
	}
	runCtx, stop := context.WithCancel(ctx)
	w.interval = 10 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(runCtx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(remote.Comments("2-"+id)) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	stop()
	<-done
	ops, err = st.ListYTWriteOps(ctx, store.YTOpFilter{State: "all", TaskID: task.ID})
	if err != nil || len(ops) != 2 || ops[1].Key != store.YTCommentKey(missed.ID) || ops[1].State != ytwrite.StateApplied || len(remote.Comments("2-"+id)) != 2 {
		t.Fatalf("catch-up through the running worker: ops %+v %v, remote %+v", ops, err, remote.Comments("2-"+id))
	}
}

// hk 3897: --all makes every task a candidate, not only those with a recent
// comment, and a task with no hk project verifies against the import's
// bucket HK (HK-<id> in the one project with that short name). The bucket is
// resolved once per run; no HK project, or two, refuses the whole run before
// any task.
func TestCommentsBackfillAllBindsBucketTasks(t *testing.T) {
	_, dbURL := testdb.Schema(t, "ytp_all")
	ctx := t.Context()
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
	fake := &ytpFake{issues: map[string]map[string]any{}, fail: map[string]int{}, hits: map[string]int{}}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	tenant := u.Host
	const project = "ytp-all"
	if _, err := st.AddTaskProject(ctx, project, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: project, ProjectID: "0-2", ShortName: "PW"}); err != nil {
		t.Fatal(err)
	}
	newTask := func(p *string) int64 {
		t.Helper()
		x, err := st.CreateTask(ctx, store.Task{Lane: "ytp-all-lane", Title: "all", Kind: "implement", CreatedBy: "test", Project: p})
		if err != nil {
			t.Fatal(err)
		}
		return x.ID
	}
	issue := func(readable, internal, summary, projectID, short string) {
		fake.issues["/api/issues/"+readable] = map[string]any{"id": internal, "idReadable": readable, "summary": summary,
			"project": map[string]any{"id": projectID, "shortName": short}}
	}
	// CreateTask requires a project; the import's no-project tasks are older
	// rows, so the project is cleared afterwards.
	p := project
	bucket := newTask(&p)   // no project, closed long ago: HK-<id>
	orphan := newTask(&p)   // no project and no issue
	idle := newTask(&p)     // mapped project, no comment
	wrongSum := newTask(&p) // no project; HK-<id> names another task
	for _, task := range []int64{bucket, orphan, wrongSum} {
		if _, err := conn.Exec(ctx, `UPDATE tasks SET project=NULL WHERE id=$1`, task); err != nil {
			t.Fatal(err)
		}
	}
	id := func(task int64) string { return strconv.FormatInt(task, 10) }
	issue("HK-"+id(bucket), "5-"+id(bucket), "#"+id(bucket)+" closed", ytpBucketID, "HK")
	issue("HK-"+id(wrongSum), "5-"+id(wrongSum), "#9999 other", ytpBucketID, "HK")
	issue("PW-"+id(idle), "2-"+id(idle), "#"+id(idle)+" idle", "0-2", "PW")
	var oldComment int64
	if err := conn.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,'old','worker',$2) RETURNING id`,
		bucket, time.Now().UTC().Add(-90*24*time.Hour)).Scan(&oldComment); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "fake-test-key")

	// Without --all none of them is a candidate: nothing has a recent comment.
	r, err := ytpRun(t, "yt", "comments-preflight")
	if err != nil || len(r.Projects) != 0 {
		t.Fatalf("preflight without --all: %+v %v", r.Projects, err)
	}
	before := ytpSnapshot(t, conn)
	r, err = ytpRun(t, "yt", "comments-preflight", "--all")
	if err != nil {
		t.Fatal(err)
	}
	if after := ytpSnapshot(t, conn); after != before {
		t.Fatalf("preflight --all wrote: %s -> %s", before, after)
	}
	noProject := r.project(t, "")
	ytpIDs(t, "no project verifies", noProject.Verifies, bucket)
	ytpIDs(t, "no project no_issue", noProject.NoIssue, orphan)
	ytpIDs(t, "no project ambiguous", noProject.Ambiguous, wrongSum)
	ytpIDs(t, "mapped verifies", r.project(t, project).Verifies, idle)

	for name, projects := range map[string][]map[string]any{
		"no HK project":   {{"id": "0-2", "shortName": "PW"}},
		"two HK projects": {{"id": ytpBucketID, "shortName": "HK"}, {"id": "0-8", "shortName": "HK"}},
	} {
		fake.mu.Lock()
		fake.projects = projects
		fake.mu.Unlock()
		fake.resetHits()
		var out bytes.Buffer
		err := run([]string{"yt", "comments-backfill", "--all"}, &out, io.Discard)
		var code exitCodeError
		if !errors.As(err, &code) || code.code != youtrackExitConfig || !strings.Contains(err.Error(), "HK") {
			t.Fatalf("%s: err %v, want exit %d naming HK", name, err, youtrackExitConfig)
		}
		if out.Len() != 0 || fake.hitsOf("/api/issues/HK-"+id(bucket)) != 0 {
			t.Fatalf("%s: the refused run printed %q or read an issue", name, out.String())
		}
		if n := ytpSnapshot(t, conn); n != before {
			t.Fatalf("%s: the refused run wrote: %s", name, n)
		}
	}
	fake.mu.Lock()
	fake.projects = nil
	fake.mu.Unlock()

	r, err = ytpRun(t, "yt", "comments-backfill", "--all")
	if err != nil || r.BoundNow == nil || *r.BoundNow != 2 {
		t.Fatalf("backfill --all: bound_now %v, err %v", r.BoundNow, err)
	}
	for task, want := range map[int64]store.YTTaskBinding{
		bucket: {TaskID: bucket, Tenant: tenant, IssueID: "5-" + id(bucket), IssueReadable: "HK-" + id(bucket), ProjectID: ytpBucketID, CommentsFrom: oldComment, Origin: store.YTBindingImportVerified},
		idle:   {TaskID: idle, Tenant: tenant, IssueID: "2-" + id(idle), IssueReadable: "PW-" + id(idle), ProjectID: "0-2", Origin: store.YTBindingImportVerified},
	} {
		got, found, err := st.YTTaskBinding(ctx, task)
		got.BoundBy, got.BoundAt = "", time.Time{}
		if err != nil || !found || got != want {
			t.Fatalf("task %d binding %+v %v %v, want %+v", task, got, found, err, want)
		}
	}
	for _, task := range []int64{orphan, wrongSum} {
		if _, found, err := st.YTTaskBinding(ctx, task); err != nil || found {
			t.Fatalf("task %d bound (%v)", task, err)
		}
	}
	// Bound now, the bucket task's comments are copied like any other's.
	st.SetYTCommentsMode(store.YTCommentsOn)
	c, err := st.CreateTaskCommentProjected(ctx, bucket, "worker", "after the bind-all")
	if err != nil || c.YouTrackProjection == nil || c.YouTrackProjection.Status != string(ytwrite.StatusPending) {
		t.Fatalf("comment on the bucket task: %+v %v", c.YouTrackProjection, err)
	}
	r, err = ytpRun(t, "yt", "comments-preflight", "--all")
	if err != nil {
		t.Fatal(err)
	}
	ytpIDs(t, "no project bound", r.project(t, "").Bound, bucket)
}

// The usage names the two new commands.
func TestYTUsageNamesCommentCommands(t *testing.T) {
	err := run([]string{"yt", "nonsense"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "comments-preflight") || !strings.Contains(err.Error(), "comments-backfill") {
		t.Fatalf("yt usage: %v", err)
	}
	for _, args := range [][]string{{"yt", "comments-preflight", "extra"}, {"yt", "comments-backfill", "--days", "0"}, {"yt", "comments-preflight", "--days", "x"}} {
		if err := run(args, io.Discard, io.Discard); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}
