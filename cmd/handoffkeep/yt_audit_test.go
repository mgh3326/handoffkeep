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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

const ytaBot = "1-1"

// ytaComment is one comment the fake YouTrack lists.
type ytaComment struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Author struct {
		ID string `json:"id"`
	} `json:"author"`
}

// ytaFake is the YouTrack the audit reads: the bot's user, issues by
// readable id, and each internal issue id's comments, paged by $skip/$top.
// Anything else, a POST included, is refused and counted.
type ytaFake struct {
	mu       sync.Mutex
	issues   map[string]map[string]any
	comments map[string][]ytaComment
	refused  int
}

func (f *ytaFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		f.refused++
		http.Error(w, `{"error":"unexpected"}`, http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/api/users/me" {
		_ = json.NewEncoder(w).Encode(map[string]string{"id": ytaBot, "login": "hk-bot", "$type": "User"})
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/api/issues/")
	if !ok {
		f.refused++
		http.Error(w, `{"error":"unexpected"}`, http.StatusNotFound)
		return
	}
	if issue, ok := strings.CutSuffix(rest, "/comments"); ok {
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		top, _ := strconv.Atoi(r.URL.Query().Get("$top"))
		all := f.comments[issue]
		page := []ytaComment{}
		if skip < len(all) {
			page = all[skip:min(len(all), skip+top)]
		}
		_ = json.NewEncoder(w).Encode(page)
		return
	}
	issue, ok := f.issues[rest]
	if !ok {
		http.Error(w, `{"error":"Entity with id not found"}`, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(issue)
}

func (f *ytaFake) add(issue, id, text, author string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := ytaComment{ID: id, Text: text}
	c.Author.ID = author
	f.comments[issue] = append(f.comments[issue], c)
}

func ytaRun(t *testing.T, args ...string) (ytAuditReport, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(append([]string{"youtrack", "audit"}, args...), &out, io.Discard)
	var r ytAuditReport
	if jerr := json.Unmarshal(out.Bytes(), &r); jerr != nil {
		t.Fatalf("%v: output %q is not a report (%v; run err %v)", args, out.String(), jerr, err)
	}
	return r, err
}

func ytaComments(c ytAuditClass) []int64 {
	out := []int64{}
	for _, it := range c.Items {
		out = append(out, it.CommentID)
	}
	return out
}

func ytaTasks(c ytAuditClass) []int64 {
	out := []int64{}
	for _, it := range c.Items {
		out = append(out, it.TaskID)
	}
	return out
}

// hk 3899: the audit classifies a bound task's comments and the unbound
// tasks without writing; --enqueue prepares exactly one op per missing
// comment, and a second run prepares none.
func TestYouTrackAudit(t *testing.T) {
	_, dbURL := testdb.Schema(t, "yta_cmd")
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
	fake := &ytaFake{issues: map[string]map[string]any{}, comments: map[string][]ytaComment{}}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	tenant := u.Host

	const project = "yta"
	if _, err := st.AddTaskProject(ctx, project, "test"); err != nil {
		t.Fatal(err)
	}
	if err := st.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: project, ProjectID: "0-2", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	newTask := func() int64 {
		t.Helper()
		p := project
		x, err := st.CreateTask(ctx, store.Task{Lane: "yta-lane", Title: "audit", Kind: "implement", CreatedBy: "test", Project: &p})
		if err != nil {
			t.Fatal(err)
		}
		return x.ID
	}
	issueOf := func(task int64) string { return "2-" + strconv.FormatInt(task, 10) }
	imported := func(task int64) {
		id := strconv.FormatInt(task, 10)
		fake.mu.Lock()
		fake.issues["HK-"+id] = map[string]any{"id": issueOf(task), "idReadable": "HK-" + id, "summary": "#" + id + " imported",
			"project": map[string]any{"id": "0-2", "shortName": "HK"}}
		fake.mu.Unlock()
	}
	bind := func(task int64, on string) {
		t.Helper()
		verified, err := st.YTCanaryCandidate(ctx, task)
		if err != nil {
			t.Fatal(err)
		}
		id := strconv.FormatInt(task, 10)
		if _, err := st.AddYTTaskBinding(ctx, store.YTTaskBinding{TaskID: task, Tenant: on, IssueID: issueOf(task), IssueReadable: "HK-" + id, ProjectID: "0-2", BoundBy: "test"}, verified); err != nil {
			t.Fatal(err)
		}
	}
	comment := func(task int64, body string) int64 {
		t.Helper()
		c, err := st.CreateTaskComment(ctx, task, "worker", body)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}

	bound := newTask()
	imported(bound)
	bind(bound, tenant)
	issue := issueOf(bound)
	cImported := comment(bound, "imported body\nsecond line")
	fake.add(issue, "4-1", "imported body\nsecond line", "1-7")
	st.SetYTCommentsMode(store.YTCommentsOn)
	cOp := comment(bound, "copied by its op")
	st.SetYTCommentsMode(store.YTCommentsOff)
	var opID int64
	if err := conn.QueryRow(ctx, `SELECT op_id FROM yt_comment_write_links WHERE comment_id=$1`, cOp).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	op, err := st.YTWriteLedger().ByID(ctx, opID)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := ytwrite.RenderComment(op)
	if err != nil {
		t.Fatal(err)
	}
	fake.add(issue, "4-2", rendered, ytaBot)
	cMissing := comment(bound, "never copied")
	cSpaced := comment(bound, "spaced  body")
	fake.add(issue, "4-3", "spaced body\n", "1-7")
	cTrailing := comment(bound, "trimmed trailing ")
	fake.add(issue, "4-13", "trimmed trailing", "1-7")
	cNewline := comment(bound, "trimmed newline\n")
	fake.add(issue, "4-14", "trimmed newline", "1-7")
	cLeading := comment(bound, "\ntrimmed leading")
	fake.add(issue, "4-15", "trimmed leading", "1-7")
	var cEmpty int64
	if err := conn.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,$2,'worker',now()) RETURNING id`, bound, " \n ").Scan(&cEmpty); err != nil {
		t.Fatal(err)
	}
	cDup := comment(bound, "twice")
	fake.add(issue, "4-4", "twice", ytaBot)
	fake.add(issue, "4-5", "twice", ytaBot)
	fake.add(issue, "4-6", "a note only the bot wrote", ytaBot)
	// A comment the activity pull copied from YouTrack: neither side counts.
	var pulled int64
	if err := conn.QueryRow(ctx, `INSERT INTO task_comments(task_id,body,author,created_at) VALUES($1,'said in YouTrack',$2,now()) RETURNING id`,
		bound, store.YouTrackOwnedActor).Scan(&pulled); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO youtrack_comment_links(hk_comment_id,task_id,youtrack_comment_id,created_at) VALUES($1,$2,'4-7',now())`, pulled, bound); err != nil {
		t.Fatal(err)
	}
	fake.add(issue, "4-7", "said in YouTrack", "1-7")
	// Someone wrote the same words again; the pull has no link for it, so it
	// is YouTrack's alone, not hidden by the equal text.
	fake.add(issue, "4-9", "said in YouTrack", "1-8")
	// A comment the legacy mirror push posted and linked: the link is an
	// outbound copy, matched by its id although the push wrapped the body.
	cMirror := comment(bound, "pushed by the legacy mirror")
	if err := st.LinkYouTrackComment(ctx, bound, cMirror, "4-8", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	fake.add(issue, "4-8", "작성: worker\n\npushed by the legacy mirror\n\n<!-- hk-comment:"+strconv.FormatInt(cMirror, 10)+" -->", ytaBot)
	// A legacy copy whose link was lost: its marker names the comment.
	cMarked := comment(bound, "pushed, link lost")
	fake.add(issue, "4-10", "**운영자** · now\n\npushed, link lost\n\n<!-- hk-comment:"+strconv.FormatInt(cMarked, 10)+" -->", ytaBot)
	// A machinery note the push chose not to mirror: its noise link is no id,
	// and its imported copy matches by body.
	cNoise := comment(bound, "machinery note")
	if err := st.LinkYouTrackComment(ctx, bound, cNoise, "noise:"+strconv.FormatInt(cNoise, 10), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	fake.add(issue, "4-11", "machinery note", "1-7")
	// A comment whose body ends in another comment's legacy trailer, and the
	// comment it names, with no other copy: the one YouTrack copy is
	// ambiguous_copy, assigned to neither, and neither is enqueued.
	cNamed := comment(bound, "named by a quoted trailer")
	quoted := "quoted example\n\n<!-- hk-comment:" + strconv.FormatInt(cNamed, 10) + " -->"
	cQuoting := comment(bound, quoted)
	fake.add(issue, "4-12", quoted, "1-7")
	// A comment the adoption linked, whose Python-mirror copy hk cannot
	// identify: adopted_unmatched, never enqueued.
	cAdopted := comment(bound, "posted by the Python mirror")
	if err := st.LinkYouTrackComment(ctx, bound, cAdopted, "adopted:"+strconv.FormatInt(cAdopted, 10), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	noProject := newTask()
	if _, err := conn.Exec(ctx, `UPDATE tasks SET project=NULL WHERE id=$1`, noProject); err != nil {
		t.Fatal(err)
	}
	noIssue := newTask()
	verifies := newTask()
	imported(verifies)
	elsewhere := newTask()
	imported(elsewhere)
	bind(elsewhere, "other.example")

	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "fake-test-key")
	// Two tasks a page, so a default run pages through all five.
	page := ytAuditTaskPage
	ytAuditTaskPage = 2
	t.Cleanup(func() { ytAuditTaskPage = page })

	gap := func(name string, err error) {
		t.Helper()
		var code exitCodeError
		if !errors.As(err, &code) || code.code != youtrackExitAuditGap {
			t.Fatalf("%s: err %v, want exit %d", name, err, youtrackExitAuditGap)
		}
	}
	check := func(name string, r ytAuditReport) {
		t.Helper()
		if r.Tenant != tenant || r.Tasks != 5 || r.Stopped != "" {
			t.Errorf("%s: tenant %q tasks %d stopped %q", name, r.Tenant, r.Tasks, r.Stopped)
		}
		c := r.Total
		if c.TasksAudited != 1 {
			t.Errorf("%s: tasks_audited %d, want 1", name, c.TasksAudited)
		}
		for _, x := range []struct {
			class string
			got   []int64
			want  []int64
		}{
			{"present", ytaComments(c.Present), []int64{cImported, cOp, cMirror, cMarked, cNoise, cTrailing, cNewline, cLeading}},
			{"adopted_unmatched", ytaComments(c.AdoptedUnmatched), []int64{cAdopted}},
			{"missing_in_yt", ytaComments(c.MissingInYT), []int64{cMissing, cSpaced, cEmpty}},
			{"duplicate", ytaComments(c.Duplicate), []int64{cDup}},
			{"no_project", ytaTasks(c.Unbound.NoProject), []int64{noProject}},
			{"no_issue", ytaTasks(c.Unbound.NoIssue), []int64{noIssue}},
			{"verifies", ytaTasks(c.Unbound.Verifies), []int64{verifies}},
			{"other_tenant", ytaTasks(c.Unbound.OtherTenant), []int64{elsewhere}},
			{"ambiguous", ytaTasks(c.Unbound.Ambiguous), []int64{}},
			{"error", ytaTasks(c.Error), []int64{}},
		} {
			if !slices.Equal(x.got, x.want) {
				t.Errorf("%s: %s = %v, want %v", name, x.class, x.got, x.want)
			}
		}
		if c.AmbiguousCopy.Count != 1 || !slices.Equal(c.AmbiguousCopy.Items[0].CommentIDs, []int64{cNamed, cQuoting}) || c.AmbiguousCopy.Items[0].YTIDs[0] != "4-12" {
			t.Errorf("%s: ambiguous_copy %+v, want 4-12 naming %d and %d", name, c.AmbiguousCopy, cNamed, cQuoting)
		}
		if c.Unbound.Count != 4 {
			t.Errorf("%s: unbound %d, want 4", name, c.Unbound.Count)
		}
		if c.Present.Items[1].OpID != opID {
			t.Errorf("%s: the op copy is matched as %+v, want op %d", name, c.Present.Items[1], opID)
		}
		human := []string{}
		for _, it := range c.YTOnlyHuman.Items {
			human = append(human, it.YTIDs...)
		}
		if c.YTOnlyBot.Count != 1 || c.YTOnlyBot.Items[0].YTIDs[0] != "4-6" || !slices.Equal(human, []string{"4-3", "4-9"}) {
			t.Errorf("%s: yt_only bot %+v human %v, want 4-6 and 4-3 4-9", name, c.YTOnlyBot, human)
		}
		if len(r.Projects) != 2 || r.Projects[0].Project != "" || r.Projects[1].Project != project || r.Projects[1].MissingInYT.Count != 3 {
			t.Errorf("%s: projects %+v", name, r.Projects)
		}
	}

	// Read-only: the same report, and nothing written to hk or YouTrack.
	before := ytpSnapshot(t, conn)
	r, err := ytaRun(t)
	gap("read-only", err)
	check("read-only", r)
	if r.Enqueue != nil {
		t.Errorf("read-only run reports an enqueue: %+v", r.Enqueue)
	}
	if after := ytpSnapshot(t, conn); after != before {
		t.Fatalf("read-only audit wrote: %s -> %s", before, after)
	}

	// --enqueue: one op per missing comment, with its key and link.
	r, err = ytaRun(t, "--enqueue")
	gap("enqueue", err)
	check("enqueue", r)
	if e := r.Enqueue; e == nil || e.Prepared != 2 || e.Linked != 0 || e.Skipped != 0 || e.Failed != 0 || len(e.OpIDs) != 2 {
		t.Fatalf("enqueue = %+v, want 2 prepared", r.Enqueue)
	}
	for _, c := range []int64{cMissing, cSpaced} {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM yt_write_ops o JOIN yt_comment_write_links w ON w.op_id = o.id
			WHERE w.comment_id=$1 AND o.idem_key=$2 AND o.principal=$3 AND o.state='prepared' AND o.tenant=$4 AND o.yt_target_id=$5`,
			c, store.YTCommentKey(c), store.YTCommentPrincipal, tenant, issue).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("comment %d has %d prepared ops with its key and link, want 1", c, n)
		}
	}
	var emptyOps int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM yt_comment_write_links WHERE comment_id=$1`, cEmpty).Scan(&emptyOps); err != nil {
		t.Fatal(err)
	}
	if emptyOps != 0 {
		t.Errorf("empty trimmed comment %d was enqueued", cEmpty)
	}
	var ops int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM yt_write_ops`).Scan(&ops); err != nil {
		t.Fatal(err)
	}
	if ops != 3 {
		t.Errorf("%d ops after --enqueue, want 3 (the comment's own and two missing)", ops)
	}

	// A second --enqueue prepares nothing.
	once := ytpSnapshot(t, conn)
	r, err = ytaRun(t, "--enqueue")
	gap("enqueue again", err)
	if e := r.Enqueue; e == nil || e.Prepared != 0 || e.Linked != 2 {
		t.Errorf("second enqueue = %+v, want 2 linked and none prepared", r.Enqueue)
	}
	if again := ytpSnapshot(t, conn); again != once {
		t.Errorf("second --enqueue wrote: %s -> %s", once, again)
	}

	// --task and --limit narrow the run; a clean task exits 0.
	clean := newTask()
	imported(clean)
	bind(clean, tenant)
	cClean := comment(clean, "one copy")
	fake.add(issueOf(clean), "5-1", "one copy", "1-7")
	r, err = ytaRun(t, "--task", strconv.FormatInt(clean, 10))
	if err != nil || r.Tasks != 1 || r.Total.Present.Count != 1 || r.Total.Present.Items[0].CommentID != cClean {
		t.Errorf("--task %d: err %v report %+v", clean, err, r.Total)
	}
	r, _ = ytaRun(t, "--limit", "3")
	if r.Tasks != 3 || r.Limit != 3 {
		t.Errorf("--limit 3 read %d tasks", r.Tasks)
	}
	if fake.refused != 0 {
		t.Errorf("the audit made %d requests the fake refuses (a write?)", fake.refused)
	}
}

// A 429 is waited out as the server asked (5 s without a Retry-After) and the
// read repeated; a wait longer than the audit takes stops the run without
// another request.
func TestYouTrackAuditWaitsOutRateLimits(t *testing.T) {
	var slept []time.Duration
	a := &ytAuditor{sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}
	answers := []error{
		&ytwrite.RemoteError{Status: http.StatusTooManyRequests, RetryAfter: 7 * time.Second},
		&ytwrite.RemoteError{Status: http.StatusTooManyRequests},
		nil,
	}
	reads := 0
	err := a.call(t.Context(), func(context.Context) error { reads++; return answers[reads-1] })
	if err != nil || reads != 3 || !slices.Equal(slept, []time.Duration{7 * time.Second, ytAuditRateLimitWait}) {
		t.Fatalf("err %v reads %d slept %v", err, reads, slept)
	}

	slept, reads = nil, 0
	err = a.call(t.Context(), func(context.Context) error {
		reads++
		return &ytwrite.RemoteError{Status: http.StatusTooManyRequests, RetryAfter: time.Hour}
	})
	var stop errYTAuditStop
	if !errors.As(err, &stop) || reads != 1 || slept != nil {
		t.Fatalf("err %v reads %d slept %v, want a stop after one read", err, reads, slept)
	}

	// Any other answer is returned as it is, never repeated.
	reads = 0
	err = a.call(t.Context(), func(context.Context) error {
		reads++
		return &ytwrite.RemoteError{Status: http.StatusServiceUnavailable}
	})
	var re *ytwrite.RemoteError
	if !errors.As(err, &re) || re.Status != http.StatusServiceUnavailable || reads != 1 {
		t.Fatalf("err %v reads %d", err, reads)
	}
}
