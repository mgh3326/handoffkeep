package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/testdb"
)

// ytartFake is the YouTrack the article commands read: the project list, the
// article list (paged by $skip/$top, without content) and one article by id
// or readable id; hits counts requests per path.
type ytartFake struct {
	mu       sync.Mutex
	articles []map[string]any
	hits     map[string]int
}

func (f *ytartFake) add(id, readable, projectID, summary, content string) {
	f.articles = append(f.articles, map[string]any{"id": id, "idReadable": readable, "summary": summary, "content": content,
		"project": map[string]any{"id": projectID}})
}

func (f *ytartFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[r.URL.Path]++
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method != http.MethodGet:
		http.Error(w, `{"error":"unexpected"}`, http.StatusMethodNotAllowed)
	case r.URL.Path == "/api/admin/projects":
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "0-1", "shortName": "HK", "name": "Unassigned hk tasks"},
			{"id": "0-3", "shortName": "SHRDOC", "name": "Shared documents"}, {"id": "0-7", "shortName": "RDA", "name": "rda"},
			{"id": "0-8", "shortName": "SMA", "name": "sma"}})
	case r.URL.Path == "/api/articles":
		skip, _ := strconv.Atoi(r.URL.Query().Get("$skip"))
		top, _ := strconv.Atoi(r.URL.Query().Get("$top"))
		page := []map[string]any{}
		for i := skip; i < len(f.articles) && i < skip+top; i++ {
			x := map[string]any{}
			for k, v := range f.articles[i] {
				if k != "content" {
					x[k] = v
				}
			}
			page = append(page, x)
		}
		_ = json.NewEncoder(w).Encode(page)
	case strings.HasPrefix(r.URL.Path, "/api/articles/"):
		want := strings.TrimPrefix(r.URL.Path, "/api/articles/")
		for _, x := range f.articles {
			if x["id"] == want || x["idReadable"] == want {
				_ = json.NewEncoder(w).Encode(x)
				return
			}
		}
		http.Error(w, `{"error":"Entity with id not found"}`, http.StatusNotFound)
	default:
		http.Error(w, `{"error":"unexpected"}`, http.StatusNotFound)
	}
}

func (f *ytartFake) hitsOf(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[path]
}

type ytartClass struct {
	Count       int     `json:"count"`
	DocumentIDs []int64 `json:"document_ids"`
}

type ytartProject struct {
	Project    string     `json:"project"`
	Lookup     string     `json:"lookup"`
	Bound      ytartClass `json:"bound"`
	Matched    ytartClass `json:"matched"`
	SHADiffers ytartClass `json:"sha_differs"`
	Missing    ytartClass `json:"missing"`
	Ambiguous  ytartClass `json:"ambiguous"`
	Excluded   ytartClass `json:"excluded"`
	Error      ytartClass `json:"error"`
}

type ytartReport struct {
	Tenant    string         `json:"tenant"`
	Documents int            `json:"documents"`
	Projects  []ytartProject `json:"projects"`
	BoundNow  *int           `json:"bound_now"`
}

func (r ytartReport) project(t *testing.T, name string) ytartProject {
	t.Helper()
	for _, p := range r.Projects {
		if p.Project == name {
			return p
		}
	}
	t.Fatalf("no project %q in %+v", name, r.Projects)
	return ytartProject{}
}

func ytartIDs(t *testing.T, name string, got ytartClass, want ...int64) {
	t.Helper()
	if want == nil {
		want = []int64{}
	}
	if got.Count != len(want) || !slices.Equal(got.DocumentIDs, want) {
		t.Errorf("%s = %d %v, want %v", name, got.Count, got.DocumentIDs, want)
	}
}

func ytartRun(t *testing.T, args ...string) (ytartReport, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, &out, io.Discard)
	var r ytartReport
	if jerr := json.Unmarshal(out.Bytes(), &r); jerr != nil {
		t.Fatalf("%v: output %q is not a report (%v; run err %v)", args, out.String(), jerr, err)
	}
	return r, err
}

func ytartSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// articles-preflight classifies every document without writing: a project
// whose articles answer to <SHORT>-A-<document id> is read by one GET each
// (lookup readable_id), one whose articles do not is matched by summary from
// the article list (lookup summary); two articles with one summary are
// ambiguous, an article in another project does not count, an excluded
// document is never read. articles-backfill binds matched and sha_differs
// (split documents with needs_update), and a second run binds nothing and
// reads no bound document's article. The audit then counts the unbound
// documents per project.
func TestArticlesPreflightAndBackfill(t *testing.T) {
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
	fake := &ytartFake{hits: map[string]int{}}
	ts := httptest.NewServer(fake)
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	tenant := u.Host

	for _, p := range []store.YouTrackProject{{HKProject: "rda", ProjectID: "0-7", ShortName: "RDA"}, {HKProject: "sma", ProjectID: "0-8", ShortName: "SMA"}, {HKProject: "nomap"}} {
		if _, err := st.AddTaskProject(ctx, p.HKProject, "test"); err != nil {
			t.Fatal(err)
		}
		if p.ShortName != "" {
			if err := st.PutYouTrackProject(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	newTask := func(p string) *int64 {
		t.Helper()
		x, err := st.CreateTask(ctx, store.Task{Lane: "yta-lane", Title: "articles", Kind: "implement", CreatedBy: "test", Project: &p})
		if err != nil {
			t.Fatal(err)
		}
		return &x.ID
	}
	rdaTask, smaTask, nomapTask := newTask("rda"), newTask("sma"), newTask("nomap")
	put := func(key, body string, task *int64) int64 {
		t.Helper()
		d, _, err := st.PutDocument(ctx, store.Document{Key: key, Kind: "note", Body: body, CreatedBy: "test", TaskID: task})
		if err != nil {
			t.Fatal(err)
		}
		return d.ID
	}
	id := func(n int64) string { return strconv.FormatInt(n, 10) }
	split := strings.Repeat("line\n", 50*1024) // 250 KiB: an index article and parts

	r1 := put("note/rda/one", "rda one", rdaTask)
	r2 := put("note/rda/two", "rda two", rdaTask)
	r3 := put("note/rda/gone", "rda gone", rdaTask)
	r4 := put("reports/rda-job/report.md", "a task's report", rdaTask)
	r5 := put("note/rda/large", split, rdaTask)
	s1 := put("note/sma/one", "sma one", smaTask)
	s2 := put("note/sma/twice", "sma twice", smaTask)
	s3 := put("note/sma/edited", "sma edited", smaTask)
	x1 := put("note/shared/elsewhere", "shared", nil)
	n1 := put("note/nomap/doc", "no mapping", nomapTask)
	// Two keys that differ only after the 250-rune cut render one summary;
	// the one article with it cannot be told apart (finding r4545018005).
	long := "note/sma/" + strings.Repeat("k", 260)
	c1 := put(long+"-a", "collides a", smaTask)
	c2 := put(long+"-b", "collides b", smaTask)
	if store.YTArticleSummary(long+"-a", "note") != store.YTArticleSummary(long+"-b", "note") {
		t.Fatal("the two long keys should render one summary")
	}

	sum := func(key string) string { return store.YTArticleSummary(key, "note") }
	fake.add("1-1", "RDA-A-"+id(r1), "0-7", sum("note/rda/one"), "rda one")
	fake.add("1-2", "RDA-A-"+id(r2), "0-7", sum("note/rda/two"), "rda two, as imported")
	fake.add("1-5", "RDA-A-"+id(r5), "0-7", store.YTArticleIndexSummary("note/rda/large", "note"), "# index")
	fake.articles = append(fake.articles, map[string]any{"id": "1-6", "idReadable": "RDA-A-900", "summary": sum("note/rda/large") + " / part 1 of 2",
		"content": "part", "project": map[string]any{"id": "0-7"}, "parentArticle": map[string]any{"id": "1-5"}})
	fake.add("2-1", "SMA-A-1", "0-8", sum("note/sma/one"), "sma one")
	fake.add("2-2", "SMA-A-2", "0-8", sum("note/sma/twice"), "sma twice")
	fake.add("2-3", "SMA-A-3", "0-8", sum("note/sma/twice"), "sma twice")
	fake.add("2-4", "SMA-A-4", "0-8", sum("note/sma/edited"), "sma before the edit")
	fake.add("3-1", "RDA-A-901", "0-7", sum("note/shared/elsewhere"), "shared") // right summary, wrong project
	fake.add("2-5", "SMA-A-5", "0-8", sum(long+"-a"), "collides a")

	t.Setenv("HANDOFFKEEP_DB_URL", dbURL)
	t.Setenv("YOUTRACK_URL", ts.URL)
	t.Setenv("YOUTRACK_BOT_API_KEY", "fake-test-key")
	bindings := func() int {
		t.Helper()
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM yt_article_bindings`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	r, err := ytartRun(t, "yt", "articles-preflight")
	if err != nil {
		t.Fatal(err)
	}
	if r.Tenant != tenant || r.Documents != 12 || r.BoundNow != nil || bindings() != 0 {
		t.Fatalf("preflight report %+v, %d bindings", r, bindings())
	}
	rda := r.project(t, "rda")
	if rda.Lookup != ytArticleLookupReadable {
		t.Errorf("rda lookup = %q, want %q", rda.Lookup, ytArticleLookupReadable)
	}
	ytartIDs(t, "rda matched", rda.Matched, r1)
	ytartIDs(t, "rda sha_differs", rda.SHADiffers, r2, r5)
	ytartIDs(t, "rda missing", rda.Missing, r3)
	ytartIDs(t, "rda excluded", rda.Excluded, r4)
	ytartIDs(t, "rda ambiguous", rda.Ambiguous)
	ytartIDs(t, "rda error", rda.Error)
	sma := r.project(t, "sma")
	if sma.Lookup != ytArticleLookupSummary {
		t.Errorf("sma lookup = %q, want %q", sma.Lookup, ytArticleLookupSummary)
	}
	ytartIDs(t, "sma matched", sma.Matched, s1)
	ytartIDs(t, "sma ambiguous", sma.Ambiguous, s2, c1, c2)
	ytartIDs(t, "sma sha_differs", sma.SHADiffers, s3)
	ytartIDs(t, "shared missing", r.project(t, store.YTArticleBucketShared).Missing, x1)
	ytartIDs(t, "nomap missing", r.project(t, "nomap").Missing, n1)
	if n := fake.hitsOf("/api/articles/RDA-A-" + id(r4)); n != 0 {
		t.Errorf("the excluded document's article was read %d times", n)
	}
	if n := fake.hitsOf("/api/articles/SMA-A-" + id(s2)); n != 0 {
		t.Errorf("a project in summary lookup was read by readable id again (%d)", n)
	}
	// One preflight on project rda only.
	if r, err := ytartRun(t, "yt", "articles-preflight", "--project", "rda"); err != nil || len(r.Projects) != 1 || r.Documents != 5 {
		t.Fatalf("--project rda: %+v, %v", r, err)
	}

	r, err = ytartRun(t, "yt", "articles-backfill", "--by", "test:backfill")
	if err != nil {
		t.Fatal(err)
	}
	if r.BoundNow == nil || *r.BoundNow != 5 || bindings() != 5 {
		t.Fatalf("backfill bound %v, %d rows; want 5", r.BoundNow, bindings())
	}
	want := map[int64]struct {
		article, sha string
		needsUpdate  bool
	}{
		r1: {"1-1", ytartSHA("rda one"), false},
		r2: {"1-2", ytartSHA("rda two, as imported"), true},
		r5: {"1-5", "", true},
		s1: {"2-1", ytartSHA("sma one"), false},
		s3: {"2-4", ytartSHA("sma before the edit"), true},
	}
	for doc, w := range want {
		b, found, err := st.YTArticleBinding(ctx, doc)
		if err != nil || !found {
			t.Fatalf("document %d: found %v, %v", doc, found, err)
		}
		if b.Tenant != tenant || b.ArticleID != w.article || b.ContentSHA != w.sha || b.NeedsUpdate != w.needsUpdate || b.Part != 0 || b.Origin != store.YTBindingImportVerified || b.BoundBy != "test:backfill" {
			t.Errorf("document %d binding = %+v, want %+v", doc, b, w)
		}
	}
	for _, doc := range []int64{r3, r4, s2, x1, n1, c1, c2} {
		if _, found, err := st.YTArticleBinding(ctx, doc); err != nil || found {
			t.Errorf("document %d was bound (%v)", doc, err)
		}
	}

	// A second run binds nothing and reads no bound document's article.
	before := fake.hitsOf("/api/articles/RDA-A-" + id(r1))
	r, err = ytartRun(t, "yt", "articles-backfill")
	if err != nil {
		t.Fatal(err)
	}
	if *r.BoundNow != 0 || bindings() != 5 || fake.hitsOf("/api/articles/RDA-A-"+id(r1)) != before {
		t.Fatalf("second backfill bound %d, %d rows, %d reads of a bound article", *r.BoundNow, bindings(), fake.hitsOf("/api/articles/RDA-A-"+id(r1))-before)
	}
	ytartIDs(t, "rda bound", r.project(t, "rda").Bound, r1, r2, r5)
	ytartIDs(t, "sma bound", r.project(t, "sma").Bound, s1, s3)

	// The audit counts the in-scope documents with no binding.
	a := &ytAuditor{st: st, tenant: tenant, articles: true}
	docs, err := a.documents(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if docs.Documents != 11 || docs.Unbound.Count != 6 || !slices.Equal(docs.Unbound.DocumentIDs, []int64{r3, s2, x1, n1, c1, c2}) {
		t.Fatalf("audit documents = %+v", docs)
	}
	per := map[string][]int64{}
	for _, p := range docs.Projects {
		per[p.Project] = p.Unbound.DocumentIDs
	}
	for p, w := range map[string][]int64{"rda": {r3}, "sma": {s2, c1, c2}, store.YTArticleBucketShared: {x1}, "nomap": {n1}} {
		if !slices.Equal(per[p], w) {
			t.Errorf("audit %s unbound = %v, want %v", p, per[p], w)
		}
	}
	other := &ytAuditor{st: st, tenant: "other.example", articles: true}
	if docs, err := other.documents(ctx, "rda"); err != nil || docs.Unbound.Count != 4 {
		t.Fatalf("another tenant's audit of rda = %+v, %v; want its 4 in-scope documents unbound", docs, err)
	}
}
