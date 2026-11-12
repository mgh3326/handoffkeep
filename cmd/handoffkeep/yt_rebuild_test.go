package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// These tests run the restore drill (hk 4121) against a fake YouTrack over
// HTTP, through the real client, and an in-memory PG side (ytrbSource): no
// database. The store reads and the seeded sample are tested in
// internal/store (TestYTRebuildReads).

// ytrbSource is the PG side: bound tasks and each task's comments.
type ytrbSource struct {
	tasks     []store.YTRebuildTask
	top       map[int64]bool
	comments  map[int64]store.YTRebuildComments
	docs      []store.YTRebuildDocument
	decisions []store.YTRebuildDecision
	// noArticles and noDecisions leave yt_article_bindings and
	// yt_decision_bindings out of the schema.
	noArticles, noDecisions bool
}

func (s *ytrbSource) YTRebuildDecisions(_ context.Context, _ string, afterID int64, limit int) ([]store.YTRebuildDecision, error) {
	out := []store.YTRebuildDecision{}
	for _, d := range s.decisions {
		if d.DecisionID > afterID && len(out) < limit {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *ytrbSource) YTRebuildDocuments(_ context.Context, _, _ string, afterID int64, limit int) ([]store.YTRebuildDocument, error) {
	out := []store.YTRebuildDocument{}
	for _, d := range s.docs {
		if d.DocumentID > afterID && len(out) < limit {
			out = append(out, d)
		}
	}
	return out, nil
}

func (s *ytrbSource) YTRebuildCount(_ context.Context, what, _ string) (int, error) {
	if what == "documents" {
		return len(s.docs), nil
	}
	return len(s.decisions), nil
}

func (s *ytrbSource) YTRebuildTasks(_ context.Context, tenant, project string, afterID int64, limit int) ([]store.YTRebuildTask, error) {
	out := []store.YTRebuildTask{}
	for _, t := range s.tasks {
		if t.TaskID > afterID && (project == "" || t.Project == project) && len(out) < limit {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *ytrbSource) YTRebuildSampleTasks(_ context.Context, _, project string, _ int64, _, _ int) ([]store.YTRebuildSample, error) {
	out := []store.YTRebuildSample{}
	for _, t := range s.tasks {
		if _, ok := s.comments[t.TaskID]; ok && (project == "" || t.Project == project) {
			out = append(out, store.YTRebuildSample{YTRebuildTask: t, Top: s.top[t.TaskID]})
		}
	}
	return out, nil
}

func (s *ytrbSource) YTRebuildTaskComments(_ context.Context, taskID int64, _ string) (store.YTRebuildComments, error) {
	c := s.comments[taskID]
	if c.PulledYTIDs == nil {
		c.PulledYTIDs = map[string]bool{}
	}
	return c, nil
}

// ytrbComment is a listed comment as YouTrack answers it.
type ytrbComment struct {
	ID      string `json:"id"`
	Text    string `json:"text"`
	Created int64  `json:"created"`
	Deleted bool   `json:"deleted"`
	Author  struct {
		ID    string `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	} `json:"author"`
}

// ytrbFake is the YouTrack the drill reads: issues by internal id, their
// comments paged by $skip/$top, and per path a queue of 429 answers (the
// Retry-After seconds, "" for none) served before the real one. Anything but a
// GET is refused and counted.
type ytrbFake struct {
	mu       sync.Mutex
	issues   map[string]map[string]any
	articles map[string]string
	comments map[string][]ytrbComment
	limited  map[string][]string
	gets     map[string]int
	refused  int
}

func newYTRBFake() *ytrbFake {
	return &ytrbFake{issues: map[string]map[string]any{}, articles: map[string]string{}, comments: map[string][]ytrbComment{}, limited: map[string][]string{}, gets: map[string]int{}}
}

func (f *ytrbFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet {
		f.refused++
		http.Error(w, `{"error":"unexpected"}`, http.StatusMethodNotAllowed)
		return
	}
	f.gets[r.URL.Path]++
	if q := f.limited[r.URL.Path]; len(q) > 0 {
		f.limited[r.URL.Path] = q[1:]
		if q[0] != "" {
			w.Header().Set("Retry-After", q[0])
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"too many requests"}`))
		return
	}
	if article, ok := strings.CutPrefix(r.URL.Path, "/api/articles/"); ok {
		content, found := f.articles[article]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Entity with id not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": article, "content": content, "$type": "Article"})
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
		page := []ytrbComment{}
		if skip < len(all) {
			page = all[skip:min(len(all), skip+top)]
		}
		_ = json.NewEncoder(w).Encode(page)
		return
	}
	issue, ok := f.issues[rest]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"Entity with id not found"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(issue)
}

// issue adds an issue with an hk ID and an hk 프로젝트 field (omitted when
// project is "-").
func (f *ytrbFake) issue(id string, hkID int64, project, description string) {
	fields := []map[string]any{}
	if hkID > 0 {
		fields = append(fields, map[string]any{"name": ytwrite.FieldHKID, "$type": "SimpleIssueCustomField", "value": hkID})
	}
	if project != "-" {
		fields = append(fields, map[string]any{"name": ytwrite.FieldProject, "$type": "SimpleIssueCustomField", "value": project})
	}
	f.issues[id] = map[string]any{"id": id, "idReadable": "HK-" + id, "summary": "#", "description": description, "customFields": fields, "$type": "Issue"}
}

func (f *ytrbFake) comment(issue, id, text string, created time.Time, login, name string) {
	c := ytrbComment{ID: id, Text: text, Created: created.UnixMilli()}
	c.Author.ID, c.Author.Login, c.Author.Name = "1-"+login, login, name
	f.comments[issue] = append(f.comments[issue], c)
}

// ytrbRun runs the drill against fake and src, recording the waits.
func ytrbRun(t *testing.T, fake *ytrbFake, src *ytrbSource, project string) (ytRebuildReport, []time.Duration) {
	t.Helper()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	client, err := youtrack.New(youtrack.Config{BaseURL: srv.URL, Token: "test", UserAgent: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var slept []time.Duration
	b := &ytRebuilder{src: src, remote: youtrack.NewWriteRemote(client), tenant: "yt.test", project: project, taskPage: 2, articles: !src.noArticles, haveDecisions: !src.noDecisions,
		reader: &ytAuditor{sleep: func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }}}
	r, err := b.run(t.Context(), 1, 200)
	if err != nil {
		t.Fatal(err)
	}
	if fake.refused != 0 {
		t.Fatalf("the drill sent %d requests the fake refuses (a write, or an unknown path)", fake.refused)
	}
	return r, slept
}

func ytrbIDs(c ytRebuildClass) []string {
	out := []string{}
	for _, it := range c.Items {
		out = append(out, it.ID)
	}
	return out
}

// hk 4121: every bound task is rebuilt from its issue, in each of the three
// description forms; one that differs is listed with its reason.
func TestYouTrackRebuildTasks(t *testing.T) {
	at := time.Date(2026, 11, 12, 1, 2, 3, 0, time.UTC)
	fake := newYTRBFake()
	// 1: the import's description; the project only in the field.
	fake.issue("2-1", 1, "alpha", "Original hk title: Fix  the  thing\nPR: https://x/1")
	// 2: issue_create's RenderIssue, before any fields snapshot (no fields).
	_, desc := ytwrite.RenderIssue(ytwrite.IssueText{Actor: "worker", At: at, OpID: 9, TaskID: 2, Project: "beta", Title: "Make it"})
	fake.issue("2-2", 0, "-", desc)
	// 3: issue_fields' RenderFields: whitespace collapsed; no project is
	// Unassigned.
	fake.issue("2-3", 3, ytwrite.UnassignedProject, ytwrite.RenderFields(ytwrite.FieldsText{TaskID: 3, Project: ytwrite.UnassignedProject, Title: "Tidy\tup  now"}))
	// 4: the title changed in hk after the issue was written.
	fake.issue("2-4", 4, "alpha", ytwrite.RenderFields(ytwrite.FieldsText{TaskID: 4, Project: "alpha", Title: "Old title"}))
	// 5: the issue is gone. 6: another task's hk ID. 7: moved in hk only.
	fake.issue("2-6", 60, "alpha", "Original hk title: Six")
	fake.issue("2-7", 7, "alpha", "Original hk title: Seven")
	src := &ytrbSource{tasks: []store.YTRebuildTask{
		{TaskID: 1, Title: "Fix  the  thing", Project: "alpha", IssueID: "2-1"},
		{TaskID: 2, Title: "Make it", Project: "beta", IssueID: "2-2"},
		{TaskID: 3, Title: "Tidy\tup  now", Project: "", IssueID: "2-3"},
		{TaskID: 4, Title: "New title", Project: "alpha", IssueID: "2-4"},
		{TaskID: 5, Title: "Five", Project: "alpha", IssueID: "2-5"},
		{TaskID: 6, Title: "Six", Project: "alpha", IssueID: "2-6"},
		{TaskID: 7, Title: "Seven", Project: "gamma", IssueID: "2-7"},
	}}
	r, _ := ytrbRun(t, fake, src, "")
	got := r.Tasks
	if got.Checked != 7 || got.Equal != 3 || got.Differ.Count != 4 || got.Errors.Count != 0 {
		t.Fatalf("tasks = %+v", got)
	}
	reasons := map[string]string{}
	for _, it := range got.Differ.Items {
		reasons[it.ID] = it.Reason
	}
	want := map[string]string{"4": `title "Old title"`, "5": "issue 2-5 not found", "6": "hk ID 60", "7": `project "alpha", hk "gamma"`}
	for id, w := range want {
		if reasons[id] != w {
			t.Errorf("task %s reason %q, want %q", id, reasons[id], w)
		}
	}
	if len(got.NotRebuildable) == 0 || r.differs() != 4 {
		t.Fatalf("not_rebuildable %v, differs %d", got.NotRebuildable, r.differs())
	}

	// --project keeps one project's tasks.
	r, _ = ytrbRun(t, fake, src, "beta")
	if r.Tasks.Checked != 1 || r.Tasks.Equal != 1 || r.Project != "beta" {
		t.Fatalf("--project beta: %+v", r.Tasks)
	}
}

// hk 4121: a task's comments are rebuilt from its issue: an hk-written one's
// author and time from its provenance line, an imported one's from
// YouTrack's author and created time; a legacy mirror copy is an exception;
// the pull's comments and other hk writes are left out; every difference is
// listed.
func TestYouTrackRebuildComments(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 30, 15, 123456000, time.UTC)
	fake := newYTRBFake()
	op := func(actor string, at time.Time, opID int64, body string) string {
		return ytwrite.ProvenanceLine(actor, at, opID) + "\n\n" + body + "\n\n" + ytwrite.OpMarker(opID)
	}
	// Task 1, issue 2-1: every comment rebuilds.
	fake.comment("2-1", "4-1", "imported body", t0, "worker:a", "worker:a")                                                   // hk 11: imported, login is the actor
	fake.comment("2-1", "4-2", op("worker:b", t0.Add(time.Hour), 501, "op body"), t0.Add(2*time.Hour), "hk-bot", "hk bot")    // hk 12
	fake.comment("2-1", "4-3", "**운영자** · 2026-11-01 09:40\n\nlegacy body\n\n<!-- hk-comment:13 -->", t0, "hk-bot", "hk bot") // hk 13
	fake.comment("2-1", "4-4", "from a person in YouTrack", t0, "robin", "Robin")                                             // pulled into hk as 14
	fake.comment("2-1", "4-5", op("hk:doc", t0, 900, "# reports/x/report.md\n\nreport"), t0, "hk-bot", "hk bot")              // a document copy
	fake.comment("2-1", "4-6", "second imported", t0.Add(time.Minute), "x", "operator:robin")                                 // hk 15: the name is the actor
	report := "# reports/j1/report.md\n\nthe report"
	fake.comment("2-1", "4-8", report, t0, "worker:a", "worker:a")                    // the import's copy of a task report
	fake.comment("2-1", "4-9", report, t0.Add(2*time.Minute), "worker:a", "worker:a") // hk 16: a task comment with the same text
	fake.comment("2-1", "4-10", report, t0, "worker:a", "worker:a")                   // a second copy of the report: one too many
	del := ytrbComment{ID: "4-7", Text: "removed", Deleted: true}
	fake.comments["2-1"] = append(fake.comments["2-1"], del)
	// Task 2, issue 2-2: one difference of each kind.
	fake.comment("2-2", "4-21", op("worker:c", t0, 601, "edited in YouTrack"), t0, "hk-bot", "hk bot")                              // hk 21: body differs
	fake.comment("2-2", "4-22", op("worker:other", t0, 602, "who wrote it"), t0, "hk-bot", "hk bot")                                // hk 22: provenance actor differs
	fake.comment("2-2", "4-23", "imported late", t0.Add(time.Second), "worker:d", "worker:d")                                       // hk 23: the time differs
	fake.comment("2-2", "4-24", "written in YouTrack only", t0, "robin", "Robin")                                                   // not pulled: yt only
	fake.comment("2-2", "4-25", op("worker:e", t0, 605, "twice"), t0, "hk-bot", "hk bot")                                           // hk 25: two copies
	fake.comment("2-2", "4-26", "twice", t0, "worker:e", "worker:e")                                                                // (imported copy of 25)
	fake.comment("2-2", "4-29", "Article: [brief] brief/2026-11-12/j2 [note] (hk document key: brief/2026-11-12/j2)", t0, "x", "x") // a brief copy, stale
	fake.comment("2-2", "4-27", "**에이전트 메모 (w)** · 2026-11-01 09:40\n\ncut sho\n\n<!-- hk-comment:27 -->", t0, "hk-bot", "hk bot")  // hk 27: legacy, body differs
	src := &ytrbSource{
		tasks: []store.YTRebuildTask{{TaskID: 1, IssueID: "2-1"}, {TaskID: 2, IssueID: "2-2"}},
		top:   map[int64]bool{2: true},
		comments: map[int64]store.YTRebuildComments{
			1: {Comments: []store.YTRebuildComment{
				{ID: 11, Author: "worker:a", Body: "imported body", CreatedAt: t0},
				// The op's time is the comment's; the provenance line keeps
				// the second.
				{ID: 12, Author: "worker:b", Body: "op body", CreatedAt: t0.Add(time.Hour), OpID: 501},
				{ID: 13, Author: "operator:robin", Body: "legacy body", CreatedAt: t0, LegacyYTID: "4-3"},
				{ID: 15, Author: "operator:robin", Body: "second imported", CreatedAt: t0.Add(time.Minute)},
				{ID: 16, Author: "worker:a", Body: report, CreatedAt: t0.Add(2 * time.Minute)},
			}, PulledYTIDs: map[string]bool{"4-4": true}, IssueOps: map[int64]bool{501: true, 900: true},
				Docs: []store.YTRebuildTaskDoc{{Key: "reports/j1/report.md", Kind: "report", Body: "the report", CreatedBy: "worker:edited-later", CreatedAt: t0}, {Key: "notes/n", Kind: "note", Body: "not copied"}}},
			2: {Comments: []store.YTRebuildComment{
				{ID: 21, Author: "worker:c", Body: "the original", CreatedAt: t0, OpID: 601},
				{ID: 22, Author: "worker:f", Body: "who wrote it", CreatedAt: t0, OpID: 602},
				{ID: 23, Author: "worker:d", Body: "imported late", CreatedAt: t0},
				{ID: 24, Author: "worker:d", Body: "never copied", CreatedAt: t0},
				{ID: 25, Author: "worker:e", Body: "twice", CreatedAt: t0, OpID: 605},
				{ID: 27, Author: "w", Body: "cut short by the mirror", CreatedAt: t0},
			}, IssueOps: map[int64]bool{601: true, 602: true, 605: true},
				Docs: []store.YTRebuildTaskDoc{{Key: "brief/2026-11-12/j2", Kind: "brief", Body: "b", CreatedBy: "x", CreatedAt: t0}}},
		},
	}
	r, _ := ytrbRun(t, fake, src, "")
	got := r.Comments
	if got.Tasks != 2 || got.TopTasks != 1 || got.PulledLeftOut != 1 || got.OtherHKWrites != 1 || got.DocumentCopies != 1 || got.Errors.Count != 0 {
		t.Fatalf("comments = %+v", got)
	}
	if got.Equal != 4 || got.Exceptions.Count != 1 || ytrbIDs(got.Exceptions)[0] != "comment 13" || !strings.HasPrefix(got.Exceptions.Items[0].Reason, "legacy_mirror: ") {
		t.Fatalf("equal %d exceptions %+v", got.Equal, got.Exceptions)
	}
	wantDiffer := []string{"yt 4-8", "yt 4-24", "yt 4-29", "yt 4-23", "comment 21", "comment 22", "comment 23", "comment 24", "comment 25", "comment 27"}
	if gotIDs := ytrbIDs(got.Differ); !slices.Equal(gotIDs, wantDiffer) || got.Differ.Count != len(wantDiffer) {
		t.Fatalf("differ %v, want %v (%+v)", gotIDs, wantDiffer, got.Differ.Items)
	}
	reason := map[string]string{}
	for _, it := range got.Differ.Items {
		reason[it.ID] = it.Reason
	}
	for id, w := range map[string]string{
		"comment 21": "4-21 body differs",
		"comment 22": `4-22 provenance "worker:other"`,
		"comment 24": "not in YouTrack",
		"comment 25": "2 copies 4-25,4-26",
		"comment 27": "legacy mirror",
		"yt 4-24":    "matches no hk comment",
		"yt 4-29":    "names document brief/2026-11-12/j2 but is no expected copy",
		"yt 4-8":     "names document reports/j1/report.md but is no expected copy",
	} {
		if !strings.Contains(reason[id], w) {
			t.Errorf("%s reason %q lacks %q", id, reason[id], w)
		}
	}
	if got.Checked != got.Equal+got.Differ.Count+got.Exceptions.Count {
		t.Fatalf("checked %d is not the sum", got.Checked)
	}
}

// hk 4121: a hk-written comment's author and time are read from its
// provenance line, at the second it was written: a comment whose hk time has
// a fraction still rebuilds, one a second off does not.
func TestYouTrackRebuildCommentProvenance(t *testing.T) {
	at := time.Date(2026, 11, 2, 3, 4, 5, 999999000, time.UTC)
	text := ytwrite.ProvenanceLine("operator:robin", at, 77) + "\n\nline one\n\nline two\n\n" + ytwrite.OpMarker(77)
	listed := []ytwrite.RemoteComment{{ID: "4-1", Text: text, AuthorLogin: "hk-bot", CreatedAt: at.Add(time.Hour)}}
	for _, tc := range []struct {
		hkAt  time.Time
		equal bool
	}{{at, true}, {at.Truncate(time.Second), true}, {at.Add(time.Second), false}} {
		var out ytRebuildComments
		hk := store.YTRebuildComments{Comments: []store.YTRebuildComment{{ID: 1, Author: "operator:robin", Body: "line one\n\nline two", CreatedAt: tc.hkAt, OpID: 77}}, PulledYTIDs: map[string]bool{}}
		diffComments(9, hk, listed, &out)
		if (out.Equal == 1) != tc.equal || out.Checked != 1 {
			t.Fatalf("hk time %v: %+v, want equal %v", tc.hkAt, out, tc.equal)
		}
	}
	// A catch-up op names its own later time: the comment's time is not in
	// YouTrack, an exception, not a pass and not a difference.
	var out ytRebuildComments
	early := at.Add(-time.Hour)
	diffComments(9, store.YTRebuildComments{Comments: []store.YTRebuildComment{{ID: 1, Author: "operator:robin", Body: "line one\n\nline two", CreatedAt: early, OpID: 77, OpCreatedAt: at}}}, listed, &out)
	if out.Exceptions.Count != 1 || !strings.HasPrefix(out.Exceptions.Items[0].Reason, "op_time: ") || out.Equal != 0 || out.Differ.Count != 0 {
		t.Fatalf("catch-up op: %+v", out)
	}
	// A provenance line naming another hk op of the issue is another hk
	// write; one hk's ledger does not have for the issue differs.
	for _, tc := range []struct {
		issueOps map[int64]bool
		other    int
		differ   []string
	}{{map[int64]bool{77: true}, 1, []string{"comment 1"}}, {nil, 0, []string{"yt 4-1", "comment 1"}}} {
		out = ytRebuildComments{}
		diffComments(9, store.YTRebuildComments{Comments: []store.YTRebuildComment{{ID: 1, Author: "operator:robin", Body: "line one\n\nline two", CreatedAt: at, OpID: 78}}, IssueOps: tc.issueOps}, listed, &out)
		if out.OtherHKWrites != tc.other || !slices.Equal(ytrbIDs(out.Differ), tc.differ) {
			t.Fatalf("op 77 in the issue's ledger %v: %+v", tc.issueOps, out)
		}
	}
}

// hk 4248: YouTrack trims trailing whitespace from comment bodies; the drill's
// comment compare trims trailing whitespace so a comment ending in \n or spaces
// rebuilds as equal.
func TestYouTrackRebuildCommentTrailingWhitespace(t *testing.T) {
	at := time.Date(2026, 11, 12, 8, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		hkBody string
		ytText string
		equal  bool
		differ string
	}{
		{"trailing newline equals trimmed (hk 4248 comment 1099 case)", "ends with newline\n", "ends with newline", true, ""},
		{"trailing spaces equal trimmed", "ends with spaces   ", "ends with spaces", true, ""},
		{"negative: internal whitespace difference differs", "two  spaces", "two spaces", false, "matches no hk comment"},
		{"negative: leading whitespace difference differs", " leading", "leading", false, "matches no hk comment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out ytRebuildComments
			hk := store.YTRebuildComments{
				Comments:    []store.YTRebuildComment{{ID: 1099, Author: "mac-personal", Body: tc.hkBody, CreatedAt: at}},
				PulledYTIDs: map[string]bool{},
			}
			listed := []ytwrite.RemoteComment{
				{ID: "7-9521", Text: tc.ytText, AuthorLogin: "mac-personal", CreatedAt: at},
			}
			diffComments(866, hk, listed, &out)
			if tc.equal {
				if out.Equal != 1 || out.Differ.Count != 0 || out.Checked != 1 {
					t.Fatalf("want equal=1 differ=0, got %+v", out)
				}
			} else {
				if out.Equal != 0 || out.Differ.Count == 0 {
					t.Fatalf("want differ, got equal=%d differ=%d", out.Equal, out.Differ.Count)
				}
				if tc.differ != "" && len(out.Differ.Items) > 0 && !strings.Contains(out.Differ.Items[0].Reason, tc.differ) {
					t.Fatalf("differ reason %q lacks %q", out.Differ.Items[0].Reason, tc.differ)
				}
			}
		})
	}
}

// hk 4121: a 429 is waited out (its Retry-After, else the protocol's wait)
// and the read repeated; a wait longer than the drill takes stops the run,
// and the stop is reported, not hidden.
func TestYouTrackRebuildRateLimit(t *testing.T) {
	fake := newYTRBFake()
	fake.issue("2-1", 1, "alpha", "Original hk title: One")
	fake.comment("2-1", "4-1", "hello", time.Unix(100, 0), "w", "w")
	fake.limited["/api/issues/2-1"] = []string{"7", ""}
	fake.limited["/api/issues/2-1/comments"] = []string{"3"}
	src := &ytrbSource{
		tasks:    []store.YTRebuildTask{{TaskID: 1, Title: "One", Project: "alpha", IssueID: "2-1"}},
		comments: map[int64]store.YTRebuildComments{1: {Comments: []store.YTRebuildComment{{ID: 1, Author: "w", Body: "hello", CreatedAt: time.Unix(100, 0).UTC()}}}},
	}
	r, slept := ytrbRun(t, fake, src, "")
	if !slices.Equal(slept, []time.Duration{7 * time.Second, ytAuditRateLimitWait, 3 * time.Second}) {
		t.Fatalf("slept %v", slept)
	}
	if r.Tasks.Equal != 1 || r.Comments.Equal != 1 || r.Stopped != "" || r.errors() != 0 || fake.gets["/api/issues/2-1"] != 3 {
		t.Fatalf("tasks %+v comments %+v stopped %q gets %v", r.Tasks, r.Comments, r.Stopped, fake.gets)
	}

	// An hour's wait stops the run after one read; the report says so.
	fake.limited["/api/issues/2-1"] = []string{"3600"}
	r, slept = ytrbRun(t, fake, src, "")
	if r.Stopped == "" || !strings.Contains(r.Stopped, "rate_limited") || len(slept) != 0 || r.Tasks.Errors.Count != 1 || r.Comments.Tasks != 0 {
		t.Fatalf("stopped %q slept %v tasks %+v comments %+v", r.Stopped, slept, r.Tasks, r.Comments)
	}
	var buf bytes.Buffer
	if err := printRebuildText(&buf, r); err != nil || !strings.Contains(buf.String(), "stopped: rate_limited") {
		t.Fatalf("text %q %v", buf.String(), err)
	}
}

// hk 4121: the command refuses flags of the comment audit and checks its own
// before it reads anything.
func TestYouTrackRebuildUsage(t *testing.T) {
	for _, args := range [][]string{
		{"--rebuild", "--sample-comments=-1"},
		{"--rebuild", "--enqueue"},
		{"--rebuild", "--task", "3"},
		{"--rebuild", "extra"},
	} {
		err := youtrackAuditCmd(args, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "usage: youtrack audit --rebuild") {
			t.Errorf("%v: %v", args, err)
		}
		var ec exitCodeError
		if errors.As(err, &ec) {
			t.Errorf("%v: exit %d, want a usage error", args, ec.code)
		}
	}
}

func ytrbSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// hk 4121: a bound document's article content hashes to documents.sha256;
// one whose binding names the import's split index is exception_not_updated,
// not a failure, and its article is not read; a changed or missing article
// differs, and so does a document that grew past 240 KiB after a single-part
// import (its size alone is no exception). Without yt_article_bindings the
// target is skipped with a count.
func TestYouTrackRebuildDocuments(t *testing.T) {
	fake := newYTRBFake()
	fake.articles["3-1"] = "# same\n\nbody"
	fake.articles["3-2"] = "old text"
	fake.articles["3-5"] = "small when imported"
	big := strings.Repeat("x", 240<<10+1)
	src := &ytrbSource{docs: []store.YTRebuildDocument{
		{DocumentID: 1, Key: "notes/a", SHA256: ytrbSHA("# same\n\nbody"), BodyBytes: 13, ArticleID: "3-1"},
		{DocumentID: 2, Key: "notes/b", SHA256: ytrbSHA("new text"), BodyBytes: 8, ArticleID: "3-2"},
		{DocumentID: 3, Key: "notes/c", SHA256: ytrbSHA("gone"), BodyBytes: 4, ArticleID: "3-3"},
		{DocumentID: 4, Key: "notes/big", SHA256: ytrbSHA(big), BodyBytes: len(big), ArticleID: "3-4", SplitIndex: true},
		{DocumentID: 5, Key: "notes/grew", SHA256: ytrbSHA(big), BodyBytes: len(big), ArticleID: "3-5"},
	}}
	r, _ := ytrbRun(t, fake, src, "")
	got := r.Documents
	if got.Checked != 5 || got.Equal != 1 || got.Differ.Count != 3 || got.Exceptions.Count != 1 || got.Skipped != "" {
		t.Fatalf("documents = %+v", got)
	}
	if !slices.Equal(ytrbIDs(got.Differ), []string{"2", "3", "5"}) || !strings.Contains(got.Differ.Items[0].Reason, "content sha256") || got.Differ.Items[1].Reason != "article 3-3 not found" {
		t.Fatalf("differ %+v", got.Differ.Items)
	}
	if it := got.Exceptions.Items[0]; it.ID != "4" || !strings.HasPrefix(it.Reason, "exception_not_updated: notes/big") {
		t.Fatalf("exception %+v", it)
	}
	if fake.gets["/api/articles/3-4"] != 0 || fake.gets["/api/articles/3-5"] != 1 {
		t.Fatalf("article reads %v", fake.gets)
	}

	src.noArticles = true
	r, _ = ytrbRun(t, fake, src, "")
	if r.Documents.Skipped == "" || r.Documents.SkippedCount != 5 || r.Documents.Checked != 0 || r.differs() != 0 {
		t.Fatalf("no bindings table: %+v", r.Documents)
	}
}

// hk 4121: a bound decision's question, options and answer are rebuilt from
// its DEC issue's Y6 block, as decision_fields writes it or between
// decision_create's provenance line and marker; each field that differs is
// listed, and so is an issue still in the import's form. Without
// yt_decision_bindings the target is skipped with a count.
func TestYouTrackRebuildDecisions(t *testing.T) {
	answered := time.Date(2026, 11, 12, 3, 0, 0, 0, time.UTC)
	text := func(id int64) ytwrite.DecisionText {
		return ytwrite.DecisionText{DecisionID: id, TaskID: 9, Question: "Ship it?\n\nTwo lines.", Recommended: "yes",
			Options: []ytwrite.DecisionOption{{Key: "yes", Label: "Ship"}, {Key: "no", Label: "Hold"}},
			Answer:  "yes", AnsweredBy: "operator:robin", AnsweredAt: &answered}
	}
	fake := newYTRBFake()
	// D-1: decision_fields' block. D-2: decision_create's description.
	fake.issue("5-1", 1, "-", ytwrite.RenderDecision(text(1)))
	fake.issue("5-2", 2, "-", ytwrite.RenderDecisionCreate("hk:decisions", answered, 77, text(2)))
	// D-3..D-5: one field each differs from hk.
	for id, edit := range map[int64]func(*ytwrite.DecisionText){
		3: func(x *ytwrite.DecisionText) { x.Question = "Ship it now?" },
		4: func(x *ytwrite.DecisionText) { x.Options = x.Options[:1] },
		5: func(x *ytwrite.DecisionText) { x.Answer = "no" },
	} {
		x := text(id)
		edit(&x)
		fake.issue("5-"+strconv.FormatInt(id, 10), id, "-", ytwrite.RenderDecision(x))
	}
	// D-6: still the import's description.
	fake.issue("5-6", 6, "-", "답변: yes\n권장: yes")
	src := &ytrbSource{}
	for id := int64(1); id <= 7; id++ {
		src.decisions = append(src.decisions, store.YTRebuildDecision{DecisionID: id, IssueID: "5-" + strconv.FormatInt(id, 10), Text: text(id)})
	}
	// D-8: bound, but gone from hk.
	src.decisions = append(src.decisions, store.YTRebuildDecision{DecisionID: 8, IssueID: "5-8", Deleted: true})
	r, _ := ytrbRun(t, fake, src, "")
	got := r.Decisions
	if got.Checked != 8 || got.Equal != 2 || got.Differ.Count != 6 || got.Skipped != "" || got.Errors.Count != 0 {
		t.Fatalf("decisions = %+v", got)
	}
	reason := map[string]string{}
	for _, it := range got.Differ.Items {
		reason[it.ID] = it.Reason
	}
	for id, w := range map[string]string{
		"D-3": "question", "D-4": "options", "D-5": "answer", "D-6": "not in the Y6 decision form",
		"D-7": "issue 5-7 not found", "D-8": "deleted in hk",
	} {
		if !strings.Contains(reason[id], w) {
			t.Errorf("%s reason %q lacks %q", id, reason[id], w)
		}
	}

	src.noDecisions = true
	r, _ = ytrbRun(t, fake, src, "")
	if r.Decisions.Skipped == "" || r.Decisions.SkippedCount != 8 || r.Decisions.Checked != 0 {
		t.Fatalf("no bindings table: %+v", r.Decisions)
	}
}

// hk 4121 (r4545895459): a YouTrack comment that fits both an expected
// document copy (body, time) and an hk comment's tuple is assigned to
// neither and differs as ambiguous, whatever the listing order: the drill
// never reports such an overlap as equal.
func TestYouTrackRebuildCommentDocOverlap(t *testing.T) {
	at := time.Date(2026, 11, 3, 4, 5, 6, 789000000, time.UTC)
	body := "# reports/j9/report.md\n\nsame"
	hk := store.YTRebuildComments{
		// The document was created by A at the same millisecond as B's
		// task comment with its exact text.
		Comments: []store.YTRebuildComment{{ID: 1, Author: "worker:b", Body: body, CreatedAt: at}},
		Docs:     []store.YTRebuildTaskDoc{{Key: "reports/j9/report.md", Kind: "report", Body: "same", CreatedBy: "worker:a", CreatedAt: at}},
	}
	copyOf := func(id, author string) ytwrite.RemoteComment {
		return ytwrite.RemoteComment{ID: id, Text: body, CreatedAt: at, AuthorLogin: author}
	}
	for _, tc := range []struct {
		name   string
		listed []ytwrite.RemoteComment
		differ []string
		docs   int
	}{
		// A's document copy is lost and B's comment is there twice: both
		// copies are ambiguous, the hk comment is not matched, nothing is
		// equal.
		{"lost copy, duplicate comment", []ytwrite.RemoteComment{copyOf("4-1", "worker:b"), copyOf("4-2", "worker:b")}, []string{"yt 4-1", "yt 4-2", "comment 1"}, 0},
		// Both correct copies exist, B's id first: A's copy (author A fits
		// no hk comment) is the document's; B's copy is ambiguous.
		{"both copies, B first", []ytwrite.RemoteComment{copyOf("4-1", "worker:b"), copyOf("4-2", "worker:a")}, []string{"yt 4-1", "comment 1"}, 1},
		{"both copies, A first", []ytwrite.RemoteComment{copyOf("4-1", "worker:a"), copyOf("4-2", "worker:b")}, []string{"yt 4-2", "comment 1"}, 1},
	} {
		var out ytRebuildComments
		diffComments(9, hk, tc.listed, &out)
		if out.Equal != 0 || out.DocumentCopies != tc.docs || !slices.Equal(ytrbIDs(out.Differ), tc.differ) {
			t.Fatalf("%s: equal %d copies %d differ %+v", tc.name, out.Equal, out.DocumentCopies, out.Differ.Items)
		}
		for _, it := range out.Differ.Items {
			if strings.HasPrefix(it.ID, "yt ") && !strings.Contains(it.Reason, "ambiguous: document copy or task comment") {
				t.Fatalf("%s: %s reason %q", tc.name, it.ID, it.Reason)
			}
		}
	}
}
