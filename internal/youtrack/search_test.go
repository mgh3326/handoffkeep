package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// The fakeStore answers the search mapping from its in-memory tables. The SQL
// behind store.YouTrackTaskHits and store.YouTrackDocHits is pinned by the
// store's DB-backed tests, not here.

func (f *fakeStore) DocumentKeyExists(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.docs[key]
	return ok, nil
}

func (f *fakeStore) YouTrackTaskHits(_ context.Context, tenant string, issueIDs []string, lane string) (map[string]store.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]store.SearchResult{}
	for _, id := range issueIDs {
		// Like the SQL: only mirrors in this tenant count, and the lower task id
		// wins when two tasks mirror one issue.
		best := int64(-1)
		for taskID, m := range f.mirrors {
			if m.IssueID != id || m.Tenant != tenant {
				continue
			}
			if best == -1 || taskID < best {
				best = taskID
			}
		}
		if best == -1 {
			continue
		}
		t, ok := f.tasks[best]
		if !ok || (lane != "" && t.Lane != lane) {
			continue
		}
		out[id] = store.SearchResult{Scope: "tasks", Key: strconv.FormatInt(best, 10), Session: t.Lane, Kind: t.Kind, CreatedAt: t.CreatedAt}
	}
	return out, nil
}

func (f *fakeStore) YouTrackDocHits(_ context.Context, articleIDs []string, session string) (map[string]store.SearchResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]store.SearchResult{}
	for _, id := range articleIDs {
		for key, a := range f.docArticles {
			if a.ArticleID != id {
				continue
			}
			d, ok := f.docs[key]
			if !ok || (session != "" && d.Session != session) {
				continue
			}
			out[id] = store.SearchResult{Scope: "docs", Key: key, Title: key, Session: d.Session, Kind: d.Kind, CreatedAt: d.CreatedAt}
		}
	}
	return out, nil
}

// fakeSearchYT answers /api/issues and /api/articles, honouring $top, and
// records each query it receives.
type fakeSearchYT struct {
	issues   []Issue
	articles []Article
	status   int
	delay    time.Duration
	oversize bool

	mu      sync.Mutex
	queries []string
}

func (y *fakeSearchYT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	y.mu.Lock()
	y.queries = append(y.queries, r.URL.Path+"?"+r.URL.Query().Get("query"))
	y.mu.Unlock()
	if y.delay > 0 {
		select {
		case <-time.After(y.delay):
		case <-r.Context().Done():
			return
		}
	}
	if y.status != 0 {
		http.Error(w, `{"error":"unavailable"}`, y.status)
		return
	}
	if y.oversize {
		_, _ = w.Write([]byte(strings.Repeat("x", maxBodyBytes+10)))
		return
	}
	top, _ := strconv.Atoi(r.URL.Query().Get("$top"))
	switch r.URL.Path {
	case "/api/issues":
		xs := y.issues
		if top > 0 && len(xs) > top {
			xs = xs[:top]
		}
		_ = json.NewEncoder(w).Encode(xs)
	case "/api/articles":
		xs := y.articles
		if top > 0 && len(xs) > top {
			xs = xs[:top]
		}
		_ = json.NewEncoder(w).Encode(xs)
	default:
		http.NotFound(w, r)
	}
}

func (y *fakeSearchYT) recorded() []string {
	y.mu.Lock()
	defer y.mu.Unlock()
	return append([]string(nil), y.queries...)
}

// fallbackRecorder stands in for PostgreSQL: it answers per scope and records
// every call.
type fallbackRecorder struct {
	rows  map[string][]store.SearchResult
	err   error
	calls []string
}

func (f *fallbackRecorder) search(_ context.Context, _, scope, _ string, _ int) ([]store.SearchResult, error) {
	f.calls = append(f.calls, scope)
	return f.rows[scope], f.err
}

func newSearchTest(t *testing.T, yt *fakeSearchYT, timeout time.Duration) (*fakeStore, *Service) {
	t.Helper()
	srv := httptest.NewServer(yt)
	t.Cleanup(srv.Close)
	client, err := New(Config{BaseURL: srv.URL, Token: "test-token", Sleep: func(context.Context, time.Duration) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	fs := newFakeStore()
	// The fake seeds projects in a map, so its listing order is random. Each
	// test adds only the projects it needs, which keeps the query deterministic.
	fs.projects = map[string]store.YouTrackProject{}
	return fs, NewService(fs, client, Options{Tenant: "tenant", SearchTimeout: timeout})
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func searchKeys(xs []store.SearchResult) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, x.Scope+":"+x.Key)
	}
	return out
}

func TestSearchTasksMapsIssuesDropsUnmappedAndFiltersSession(t *testing.T) {
	yt := &fakeSearchYT{issues: []Issue{
		{ID: "2-404", IDReadable: "HK-404", Summary: "orphan"},
		{ID: "2-7", IDReadable: "HK-7", Summary: "summary seven", Description: "alpha   beta\n gamma"},
		{ID: "2-9", IDReadable: "HK-9", Summary: "summary nine"},
	}}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	if err := fs.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	t7 := fs.addTask("handoffkeep", "operator-desk", "seven", "open", 1)
	t9 := fs.addTask("handoffkeep", "other-lane", "nine", "open", 1)
	fs.mirror(t7.ID, "2-7", "HK-7")
	fs.mirror(t9.ID, "2-9", "HK-9")

	fb := &fallbackRecorder{}
	got, err := svc.Search(ctx, fb.search, "alpha", "tasks", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tasks:" + strconv.FormatInt(t7.ID, 10), "tasks:" + strconv.FormatInt(t9.ID, 10)}
	if keys := searchKeys(got); strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v (unmapped 2-404 dropped, YouTrack order kept)", keys, want)
	}
	if got[0].Title != "summary seven" || got[0].Snippet != "alpha beta gamma" {
		t.Fatalf("first hit title/snippet = %q / %q", got[0].Title, got[0].Snippet)
	}
	if len(fb.calls) != 0 {
		t.Fatalf("fallback called %v on a healthy YouTrack answer", fb.calls)
	}

	got, err = svc.Search(ctx, fb.search, "alpha", "tasks", "operator-desk", 20)
	if err != nil {
		t.Fatal(err)
	}
	if keys := searchKeys(got); len(keys) != 1 || keys[0] != "tasks:"+strconv.FormatInt(t7.ID, 10) {
		t.Fatalf("session filter keys = %v, want only task %d", keys, t7.ID)
	}
}

func TestSearchIgnoresMappingsFromAnotherTenant(t *testing.T) {
	yt := &fakeSearchYT{issues: []Issue{{ID: "2-7", Summary: "seven"}}}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	if err := fs.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	// The older task holds a mapping from a previous YouTrack tenant. It has the
	// lower id, so only the tenant filter keeps it from winning.
	old := fs.addTask("handoffkeep", "operator-desk", "old tenant", "open", 1)
	fs.mu.Lock()
	fs.mirrors[old.ID] = store.YouTrackMirror{TaskID: old.ID, IssueID: "2-7", Tenant: "old-tenant"}
	fs.mu.Unlock()
	cur := fs.addTask("handoffkeep", "operator-desk", "current tenant", "open", 1)
	fs.mirror(cur.ID, "2-7", "HK-7")

	got, err := svc.Search(ctx, (&fallbackRecorder{}).search, "seven", "tasks", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if keys := searchKeys(got); len(keys) != 1 || keys[0] != "tasks:"+strconv.FormatInt(cur.ID, 10) {
		t.Fatalf("keys = %v, want only the current-tenant task %d", keys, cur.ID)
	}
}

func TestSearchDocsMapsArticlesAndDropsIndexArticles(t *testing.T) {
	yt := &fakeSearchYT{articles: []Article{
		{ID: "101-2", IDReadable: "KB-2", Summary: "index page", Content: "heading index"},
		{ID: "101-1", IDReadable: "KB-1", Summary: "Notes", Content: "# Heading\n\nThe body text"},
	}}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	fs.mu.Lock()
	fs.docs["design/alpha-notes"] = store.Document{Key: "design/alpha-notes", Kind: "design", Session: "sess-a"}
	fs.mu.Unlock()
	if err := fs.UpsertYouTrackDocArticle(ctx, store.YouTrackDocArticle{DocKey: "design/alpha-notes", HKProject: "handoffkeep", ArticleID: "101-1"}); err != nil {
		t.Fatal(err)
	}
	if err := fs.UpsertYouTrackDocArticle(ctx, store.YouTrackDocArticle{DocKey: "index:hk:handoffkeep", HKProject: "handoffkeep", ArticleID: "101-2"}); err != nil {
		t.Fatal(err)
	}

	fb := &fallbackRecorder{}
	got, err := svc.Search(ctx, fb.search, "heading", "docs", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if keys := searchKeys(got); len(keys) != 1 || keys[0] != "docs:design/alpha-notes" {
		t.Fatalf("keys = %v, want only design/alpha-notes (index article has no document)", keys)
	}
	if got[0].Title != "design/alpha-notes" || got[0].Snippet != "# Heading The body text" || got[0].Session != "sess-a" {
		t.Fatalf("doc hit = %+v", got[0])
	}
	if got, err = svc.Search(ctx, fb.search, "heading", "docs", "other-session", 20); err != nil || len(got) != 0 {
		t.Fatalf("session filter = %+v, %v; want no hits", got, err)
	}
	if len(fb.calls) != 0 {
		t.Fatalf("fallback called %v on a healthy YouTrack answer", fb.calls)
	}
}

func TestSearchExactLookupsStayOnPostgres(t *testing.T) {
	yt := &fakeSearchYT{}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	fs.mu.Lock()
	fs.docs["design/exact-key"] = store.Document{Key: "design/exact-key", Kind: "design"}
	fs.mu.Unlock()

	cases := []struct{ q, scope string }{
		{"design/exact-key", "all"},
		{"design/exact-key", "docs"},
		{"#42", "all"},
		{"42", "tasks"},
		{"hello", "ctx"},
		{"hello", "memory"},
	}
	for _, c := range cases {
		fb := &fallbackRecorder{rows: map[string][]store.SearchResult{c.scope: {{Scope: c.scope, Key: "pg"}}}}
		got, err := svc.Search(ctx, fb.search, c.q, c.scope, "", 20)
		if err != nil {
			t.Fatalf("%q %s: %v", c.q, c.scope, err)
		}
		if len(fb.calls) == 0 || len(got) != 1 || got[0].Key != "pg" {
			t.Fatalf("%q %s: fallback calls %v, got %+v; want PostgreSQL to answer", c.q, c.scope, fb.calls, got)
		}
	}
	if q := yt.recorded(); len(q) != 0 {
		t.Fatalf("YouTrack was asked %v for exact or PostgreSQL-only lookups", q)
	}
}

func TestSearchFallsBackWithReasonOnYouTrackFailure(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		delay    time.Duration
		oversize bool
		timeout  time.Duration
		reason   string
	}{
		{"server error", http.StatusInternalServerError, 0, false, time.Second, "http_500"},
		{"unavailable", http.StatusServiceUnavailable, 0, false, time.Second, "http_503"},
		{"timeout", 0, 2 * time.Second, false, 50 * time.Millisecond, "timeout"},
		{"oversize answer", 0, 0, true, time.Second, "oversize"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			yt := &fakeSearchYT{status: c.status, delay: c.delay, oversize: c.oversize}
			fs, svc := newSearchTest(t, yt, c.timeout)
			if err := fs.PutYouTrackProject(context.Background(), store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
				t.Fatal(err)
			}
			logs := captureLog(t)
			fb := &fallbackRecorder{rows: map[string][]store.SearchResult{"tasks": {{Scope: "tasks", Key: "pg"}}}}
			got, err := svc.Search(context.Background(), fb.search, "anything", "tasks", "", 20)
			if err != nil {
				t.Fatalf("search failed instead of falling back: %v", err)
			}
			if len(got) != 1 || got[0].Key != "pg" {
				t.Fatalf("got %+v, want the PostgreSQL answer", got)
			}
			if want := "search.youtrack_fallback reason=" + c.reason + " scope=tasks"; !strings.Contains(logs.String(), want) {
				t.Fatalf("log = %q, want %q", logs.String(), want)
			}
		})
	}
}

func TestSearchEscapesUserTextAsOneQuotedPhrase(t *testing.T) {
	yt := &fakeSearchYT{}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	if err := fs.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	fb := &fallbackRecorder{}
	input := `project: OTHER "x" \ for: me`
	if _, err := svc.Search(ctx, fb.search, input, "tasks", "", 20); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Search(ctx, fb.search, input, "docs", "", 20); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`/api/issues?(project: {HK}) "project: OTHER \"x\" \\ for: me"`,
		`/api/articles?"project: OTHER \"x\" \\ for: me"`,
	}
	got := yt.recorded()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("queries =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestSearchLimitAndTruncatedMarkEveryRow(t *testing.T) {
	yt := &fakeSearchYT{issues: []Issue{
		{ID: "2-1", Summary: "one"}, {ID: "2-2", Summary: "two"}, {ID: "2-3", Summary: "three"},
	}}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	if err := fs.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	for i, issue := range yt.issues {
		task := fs.addTask("handoffkeep", "operator-desk", issue.Summary, "open", 1)
		fs.mirror(task.ID, issue.ID, "HK-"+strconv.Itoa(i+1))
	}
	fb := &fallbackRecorder{}

	got, err := svc.Search(ctx, fb.search, "x", "tasks", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("limit 2 returned %d rows", len(got))
	}
	for _, x := range got {
		if !x.Truncated {
			t.Fatalf("row %+v of a truncated page lacks Truncated", x)
		}
	}

	got, err = svc.Search(ctx, fb.search, "x", "tasks", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("limit 5 returned %d rows, want 3", len(got))
	}
	for _, x := range got {
		if x.Truncated {
			t.Fatalf("complete page marked truncated: %+v", x)
		}
	}
}

func TestSearchAllInterleavesLanesAndKeepsPostgresHits(t *testing.T) {
	yt := &fakeSearchYT{
		issues:   []Issue{{ID: "2-1", Summary: "task one"}, {ID: "2-2", Summary: "task two"}},
		articles: []Article{{ID: "101-1", Summary: "doc one", Content: "body"}},
	}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	if err := fs.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	t1 := fs.addTask("handoffkeep", "operator-desk", "task one", "open", 1)
	t2 := fs.addTask("handoffkeep", "operator-desk", "task two", "open", 1)
	fs.mirror(t1.ID, "2-1", "HK-1")
	fs.mirror(t2.ID, "2-2", "HK-2")
	fs.mu.Lock()
	fs.docs["design/one"] = store.Document{Key: "design/one", Kind: "design"}
	fs.mu.Unlock()
	if err := fs.UpsertYouTrackDocArticle(ctx, store.YouTrackDocArticle{DocKey: "design/one", HKProject: "handoffkeep", ArticleID: "101-1"}); err != nil {
		t.Fatal(err)
	}

	fb := &fallbackRecorder{rows: map[string][]store.SearchResult{
		"ctx":    {{Scope: "ctx", Key: "checkpoint-1"}},
		"memory": {{Scope: "memory", Key: "memory-1"}},
	}}
	got, err := svc.Search(ctx, fb.search, "one", "all", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tasks:" + strconv.FormatInt(t1.ID, 10),
		"docs:design/one",
		"ctx:checkpoint-1",
		"tasks:" + strconv.FormatInt(t2.ID, 10),
		"memory:memory-1",
	}
	if keys := searchKeys(got); strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want round-robin %v", keys, want)
	}
	if strings.Join(fb.calls, ",") != "ctx,memory" {
		t.Fatalf("PostgreSQL asked for %v, want ctx and memory only", fb.calls)
	}
}

func TestSearchTasksWithoutMirroredProjectsAsksNothing(t *testing.T) {
	yt := &fakeSearchYT{issues: []Issue{{ID: "2-1", Summary: "task one"}}}
	fs, svc := newSearchTest(t, yt, time.Second)
	fs.addTask("handoffkeep", "operator-desk", "task one", "open", 1)
	fb := &fallbackRecorder{}
	got, err := svc.Search(context.Background(), fb.search, "one", "tasks", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || len(yt.recorded()) != 0 {
		t.Fatalf("got %+v, YouTrack queries %v; want no YouTrack call and no hits", got, yt.recorded())
	}
}

func TestSearchHelpers(t *testing.T) {
	if got := quoteSearchText(`a"b\c`); got != `"a\"b\\c"` {
		t.Fatalf("quoteSearchText = %s", got)
	}
	if got := excerpt("  alpha\n\n beta  ", "fallback"); got != "alpha beta" {
		t.Fatalf("excerpt = %q", got)
	}
	if got := excerpt("", "  the   summary "); got != "the summary" {
		t.Fatalf("excerpt fallback = %q", got)
	}
	long := strings.Repeat("가", searchExcerptRunes+5)
	if got := []rune(excerpt(long, "")); len(got) != searchExcerptRunes {
		t.Fatalf("excerpt kept %d runes, want %d", len(got), searchExcerptRunes)
	}
	cases := map[string]int{"": 20, "0": 20, "7": 7, "500": 100}
	for in, want := range cases {
		n, _ := strconv.Atoi(in)
		if got := searchLimit("docs", n); got != want {
			t.Fatalf("searchLimit(docs, %s) = %d, want %d", in, got, want)
		}
	}
	if got := searchLimit("tasks", 80); got != 50 {
		t.Fatalf("searchLimit(tasks, 80) = %d, want 50", got)
	}
	if q := issueSearchQuery([]string{"HK", "BIG"}, "x"); q != `(project: {HK} or project: {BIG}) "x"` {
		t.Fatalf("issueSearchQuery = %s", q)
	}
}
