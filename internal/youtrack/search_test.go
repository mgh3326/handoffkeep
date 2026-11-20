package youtrack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
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

func (f *fakeStore) ExactNumericHit(_ context.Context, q, scope, session string) ([]store.SearchResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !store.NumericIDQuery(q) {
		return nil, false, nil
	}
	idStr := strings.TrimPrefix(strings.TrimSpace(q), "#")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, true, nil
	}
	var out []store.SearchResult
	if (scope == "tasks" || scope == "all") && f.tasks != nil {
		if t, ok := f.tasks[id]; ok && (session == "" || t.Lane == session) {
			out = append(out, store.SearchResult{Scope: "tasks", Key: strconv.FormatInt(t.ID, 10), Title: t.Title, Snippet: t.Title, Session: t.Lane, Kind: t.Kind, CreatedAt: t.CreatedAt})
		}
	}
	return out, true, nil
}

func (f *fakeStore) ExactDocHit(_ context.Context, key, session string) ([]store.SearchResult, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.docs[key]; ok && (session == "" || d.Session == session) {
		return []store.SearchResult{{Scope: "docs", Key: key, Title: key, Session: d.Session, Kind: d.Kind, Snippet: d.Body, CreatedAt: d.CreatedAt}}, true, nil
	}
	return nil, false, nil
}

func (f *fakeStore) SearchCtx(_ context.Context, q, session string, limit int) ([]store.SearchResult, error) {
	return append([]store.SearchResult(nil), f.ctxHits...), nil
}

// fakeSearchYT answers /api/issues and /api/articles, honouring $top, and
// records each query it receives.
type fakeSearchYT struct {
	issues   []Issue
	articles []Article
	status   int
	delay    time.Duration
	oversize bool
	// pathDelay overrides delay for one endpoint, e.g. "/api/issues", so one
	// lane can be slow while the other answers at once. pathStatus fails one
	// endpoint with an HTTP status, with the other endpoints unaffected.
	pathDelay  map[string]time.Duration
	pathStatus map[string]int

	mu      sync.Mutex
	queries []string
}

func (y *fakeSearchYT) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	y.mu.Lock()
	y.queries = append(y.queries, r.URL.Path+"?"+r.URL.Query().Get("query"))
	y.mu.Unlock()
	delay := y.delay
	if d, ok := y.pathDelay[r.URL.Path]; ok {
		delay = d
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	if status := y.pathStatus[r.URL.Path]; status != 0 {
		http.Error(w, `{"error":"unavailable"}`, status)
		return
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
	switch {
	case r.URL.Path == "/api/issues":
		xs := y.issues
		if top > 0 && len(xs) > top {
			xs = xs[:top]
		}
		_ = json.NewEncoder(w).Encode(xs)
	case r.URL.Path == "/api/articles":
		// Like YouTrack, content is returned only when the request asks for it.
		xs := y.articles
		if top > 0 && len(xs) > top {
			xs = xs[:top]
		}
		if !strings.Contains(r.URL.Query().Get("fields"), "content") {
			xs = withoutContent(xs)
		}
		_ = json.NewEncoder(w).Encode(xs)
	case strings.HasPrefix(r.URL.Path, "/api/articles/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/articles/")
		for _, a := range y.articles {
			if a.ID == id {
				_ = json.NewEncoder(w).Encode(a)
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func withoutContent(xs []Article) []Article {
	out := make([]Article, len(xs))
	for i, a := range xs {
		a.Content = ""
		out[i] = a
	}
	return out
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

	got, err := svc.Search(ctx, "alpha", "tasks", "", 20)
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

	got, err = svc.Search(ctx, "alpha", "tasks", "operator-desk", 20)
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

	got, err := svc.Search(ctx, "seven", "tasks", "", 20)
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

	got, err := svc.Search(ctx, "heading", "docs", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if keys := searchKeys(got); len(keys) != 1 || keys[0] != "docs:design/alpha-notes" {
		t.Fatalf("keys = %v, want only design/alpha-notes (index article has no document)", keys)
	}
	if got[0].Title != "design/alpha-notes" || got[0].Snippet != "# Heading The body text" || got[0].Session != "sess-a" {
		t.Fatalf("doc hit = %+v", got[0])
	}
	if got, err = svc.Search(ctx, "heading", "docs", "other-session", 20); err != nil || len(got) != 0 {
		t.Fatalf("session filter = %+v, %v; want no hits", got, err)
	}
}

func TestSearchExactLookupsUseExactStore(t *testing.T) {
	yt := &fakeSearchYT{}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	fs.mu.Lock()
	fs.docs["design/exact-key"] = store.Document{Key: "design/exact-key", Kind: "design", Body: "design body"}
	fs.mu.Unlock()
	task := fs.addTask("handoffkeep", "lane-a", "task 42", "open", 1)

	// Exact doc key in docs and all
	got, err := svc.Search(ctx, "design/exact-key", "docs", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "design/exact-key" {
		t.Fatalf("exact doc hit: %+v", got)
	}
	got, err = svc.Search(ctx, "design/exact-key", "all", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "design/exact-key" {
		t.Fatalf("exact doc hit in all: %+v", got)
	}

	// Exact numeric id in tasks and all
	got, err = svc.Search(ctx, "#"+strconv.FormatInt(task.ID, 10), "tasks", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != strconv.FormatInt(task.ID, 10) {
		t.Fatalf("exact numeric hit: %+v", got)
	}
	got, err = svc.Search(ctx, strconv.FormatInt(task.ID, 10), "all", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != strconv.FormatInt(task.ID, 10) {
		t.Fatalf("exact numeric hit in all: %+v", got)
	}

	if q := yt.recorded(); len(q) != 0 {
		t.Fatalf("YouTrack was asked %v for exact lookups", q)
	}
}

func TestSearchErrorsImmediatelyOnYouTrackFailure(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		delay    time.Duration
		oversize bool
		timeout  time.Duration
		wantErr  error
	}{
		{"server error", http.StatusInternalServerError, 0, false, time.Second, store.ErrSearchUnavailable},
		{"unavailable", http.StatusServiceUnavailable, 0, false, time.Second, store.ErrSearchUnavailable},
		{"timeout", 0, 2 * time.Second, false, 50 * time.Millisecond, store.ErrSearchTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			yt := &fakeSearchYT{status: c.status, delay: c.delay, oversize: c.oversize}
			fs, svc := newSearchTest(t, yt, c.timeout)
			if err := fs.PutYouTrackProject(context.Background(), store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
				t.Fatal(err)
			}
			_, err := svc.Search(context.Background(), "anything", "tasks", "", 20)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("got err %v, want %v", err, c.wantErr)
			}
		})
	}
}

func TestSearchAllTimeoutReturnsAvailableContextAsPartial(t *testing.T) {
	yt := &fakeSearchYT{delay: 2 * time.Second}
	fs, svc := newSearchTest(t, yt, 20*time.Millisecond)
	fs.ctxHits = []store.SearchResult{{Scope: "ctx", Key: "memory/available", Title: "available context"}}
	if err := fs.PutYouTrackProject(context.Background(), store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Search(context.Background(), "timeout probe", "all", "", 20)
	if err != nil {
		t.Fatalf("scope all timed out: %v", err)
	}
	if len(got) != 1 || got[0].Key != "memory/available" || !got[0].Partial || !got[0].TimedOut {
		t.Fatalf("partial timeout results = %+v, want available context marked partial and timed_out", got)
	}
}

func TestSearchRawTextQueries(t *testing.T) {
	yt := &fakeSearchYT{}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	if err := fs.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	input := `project: OTHER "x" \ for: me`
	if _, err := svc.Search(ctx, input, "tasks", "", 20); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Search(ctx, input, "docs", "", 20); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`/api/issues?(project: {HK}) and (project\: OTHER \"x\" \\ for\: me)`,
		`/api/articles?project\: OTHER \"x\" \\ for\: me`,
	}
	// The lanes are requested concurrently, so arrival order is not fixed.
	got := yt.recorded()
	sort.Strings(got)
	sort.Strings(want)
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

	got, err := svc.Search(ctx, "x", "tasks", "", 2)
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

	got, err = svc.Search(ctx, "x", "tasks", "", 5)
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

	got, err := svc.Search(ctx, "one", "all", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tasks:" + strconv.FormatInt(t1.ID, 10),
		"docs:design/one",
		"tasks:" + strconv.FormatInt(t2.ID, 10),
	}
	if keys := searchKeys(got); strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want round-robin %v", keys, want)
	}
}

func TestSearchTasksWithoutMirroredProjectsAsksNothing(t *testing.T) {
	yt := &fakeSearchYT{issues: []Issue{{ID: "2-1", Summary: "task one"}}}
	fs, svc := newSearchTest(t, yt, time.Second)
	fs.addTask("handoffkeep", "operator-desk", "task one", "open", 1)
	got, err := svc.Search(context.Background(), "one", "tasks", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || len(yt.recorded()) != 0 {
		t.Fatalf("got %+v, YouTrack queries %v; want no YouTrack call and no hits", got, yt.recorded())
	}
}

// seedDocArticles mirrors each key as a document and links it to its article
// id, so the fake YouTrack's article hits map back to hk documents.
func seedDocArticles(t *testing.T, fs *fakeStore, articleByKey map[string]string) {
	t.Helper()
	ctx := context.Background()
	for key, articleID := range articleByKey {
		fs.mu.Lock()
		fs.docs[key] = store.Document{Key: key, Kind: strings.SplitN(key, "/", 2)[0], Session: "sess-rank"}
		fs.mu.Unlock()
		if err := fs.UpsertYouTrackDocArticle(ctx, store.YouTrackDocArticle{DocKey: key, HKProject: "handoffkeep", ArticleID: articleID}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSearchAllSlowIssueLaneKeepsDocHitsAndMarksPage(t *testing.T) {
	yt := &fakeSearchYT{
		issues:    []Issue{{ID: "2-1", Summary: "task one"}},
		articles:  []Article{{ID: "101-1", Summary: "doc one", Content: "body"}},
		pathDelay: map[string]time.Duration{"/api/issues": 2 * time.Second},
	}
	fs, svc := newSearchTest(t, yt, 100*time.Millisecond)
	ctx := context.Background()
	if err := fs.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	seedDocArticles(t, fs, map[string]string{"design/one": "101-1"})
	logs := captureLog(t)

	page, err := svc.SearchPage(ctx, "one", "all", "", 20)
	if err != nil {
		t.Fatalf("scope all with a slow issue lane: %v", err)
	}
	if keys := searchKeys(page.Results); strings.Join(keys, ",") != "docs:design/one" {
		t.Fatalf("keys = %v, want the article hit kept while issues timed out", keys)
	}
	if !page.Partial || !page.TimedOut {
		t.Fatalf("page = partial %v timed_out %v, want both set", page.Partial, page.TimedOut)
	}
	if !page.Results[0].Partial || !page.Results[0].TimedOut {
		t.Fatalf("row %+v lacks the partial/timed_out marker", page.Results[0])
	}
	if !strings.Contains(logs.String(), "search.youtrack_error lane=issues reason=timeout") {
		t.Fatalf("log = %q, want the timed-out lane named", logs.String())
	}
}

func TestSearchAllEmptyTimedOutPageIsMarked(t *testing.T) {
	yt := &fakeSearchYT{delay: 2 * time.Second}
	fs, svc := newSearchTest(t, yt, 20*time.Millisecond)
	if err := fs.PutYouTrackProject(context.Background(), store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}

	logs := captureLog(t)
	page, err := svc.SearchPage(context.Background(), "nothing matches", "all", "", 20)
	if err != nil {
		t.Fatalf("scope all with no lane answering: %v", err)
	}
	if len(page.Results) != 0 || !page.Partial || !page.TimedOut {
		t.Fatalf("page = %+v, want an empty page marked partial and timed_out", page)
	}
	// Both lanes missed the deadline; each one is logged, not just the first.
	if out := logs.String(); !strings.Contains(out, "lane=issues reason=timeout") || !strings.Contains(out, "lane=articles reason=timeout") {
		t.Fatalf("log = %q, want both timed-out lanes logged", out)
	}
}

func TestSearchDocsRanksDecisionAheadOfJobCopies(t *testing.T) {
	yt := &fakeSearchYT{articles: []Article{
		{ID: "101-1", Summary: "r1"},
		{ID: "101-2", Summary: "d-b"},
		{ID: "101-3", Summary: "d-a"},
		{ID: "101-4", Summary: "other z"},
		{ID: "101-5", Summary: "brief x"},
		{ID: "101-6", Summary: "d-c"},
		{ID: "101-7", Summary: "design q"},
	}}
	fs, svc := newSearchTest(t, yt, time.Second)
	ctx := context.Background()
	seedDocArticles(t, fs, map[string]string{
		"reports/job-1/report": "101-1",
		"decision/b":           "101-2",
		"decision/a":           "101-3",
		"zeta/other-z":         "101-4",
		"brief/2026-11-20/x":   "101-5",
		"decision/c":           "101-6",
		"design/q":             "101-7",
	})

	got, err := svc.Search(ctx, "x", "docs", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	// Rank order: decision, then design, then other, then job copies last. Each
	// rank keeps YouTrack's order (decision/b before decision/a before decision/c).
	want := []string{
		"docs:decision/b", "docs:decision/a", "docs:decision/c",
		"docs:design/q", "docs:zeta/other-z",
		"docs:reports/job-1/report", "docs:brief/2026-11-20/x",
	}
	if keys := searchKeys(got); strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v\nwant %v", keys, want)
	}
}

func TestSearchDocsFetchesRoomForRerank(t *testing.T) {
	// Three job copies come first in YouTrack's order. Asking for limit+1 hits
	// would return only those three and never reach the decision doc behind them.
	yt := &fakeSearchYT{articles: []Article{
		{ID: "101-1", Summary: "r1"},
		{ID: "101-2", Summary: "r2"},
		{ID: "101-3", Summary: "r3"},
		{ID: "101-4", Summary: "d1"},
	}}
	fs, svc := newSearchTest(t, yt, time.Second)
	seedDocArticles(t, fs, map[string]string{
		"reports/job-1/r": "101-1",
		"reports/job-2/r": "101-2",
		"reports/job-3/r": "101-3",
		"decision/d1":     "101-4",
	})

	got, err := svc.Search(context.Background(), "x", "docs", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"docs:decision/d1", "docs:reports/job-1/r"}
	if keys := searchKeys(got); strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for _, x := range got {
		if !x.Truncated {
			t.Fatalf("row %+v of a truncated page lacks Truncated", x)
		}
	}
}

// The docs candidates are read without content, so the wider 3× candidate set
// stays under the client's 4 MiB response cap however large the articles are.
// A candidate request with content (61 × 100 KiB ≈ 6 MiB) would be refused as
// oversize and fail the whole search; this test fails if that comes back.
func TestSearchDocsCandidatesStayUnderResponseCap(t *testing.T) {
	big := strings.Repeat("a", 100<<10)
	articles := make([]Article, 0, 70)
	mapped := map[string]string{}
	for i := 1; i <= 70; i++ {
		id := "101-" + strconv.Itoa(i)
		articles = append(articles, Article{ID: id, Summary: "doc " + id, Content: big})
		if i <= 25 {
			mapped["design/bulk-"+strconv.Itoa(i)] = id
		}
	}
	yt := &fakeSearchYT{articles: articles}
	fs, svc := newSearchTest(t, yt, time.Second)
	seedDocArticles(t, fs, mapped)

	got, err := svc.Search(context.Background(), "bulk", "docs", "", 20)
	if err != nil {
		t.Fatalf("docs search with large articles: %v", err)
	}
	if len(got) != 20 {
		t.Fatalf("rows = %d, want the limit of 20", len(got))
	}
	if len(got[0].Snippet) == 0 || !strings.HasPrefix(got[0].Snippet, "aaaa") {
		t.Fatalf("snippet = %q, want the article's content", got[0].Snippet)
	}
}

// Each failed lane is classified when it returns and logged. An HTTP error that
// came back before the deadline is not relabelled a timeout by the other lane's
// deadline, so the page fails as unavailable instead of partial.
func TestSearchAllHTTPErrorIsNotLabelledTimeout(t *testing.T) {
	yt := &fakeSearchYT{
		issues:     []Issue{{ID: "2-1", Summary: "one"}},
		articles:   []Article{{ID: "101-1", Summary: "doc one", Content: "body"}},
		pathStatus: map[string]int{"/api/issues": http.StatusUnauthorized},
		pathDelay:  map[string]time.Duration{"/api/articles": 2 * time.Second},
	}
	fs, svc := newSearchTest(t, yt, 100*time.Millisecond)
	ctx := context.Background()
	if err := fs.PutYouTrackProject(ctx, store.YouTrackProject{HKProject: "handoffkeep", ShortName: "HK"}); err != nil {
		t.Fatal(err)
	}
	logs := captureLog(t)

	_, err := svc.SearchPage(ctx, "one", "all", "", 20)
	if !errors.Is(err, store.ErrSearchUnavailable) {
		t.Fatalf("err = %v, want search_unavailable (the 401 is an upstream failure)", err)
	}
	out := logs.String()
	if !strings.Contains(out, "lane=issues reason=http_401") || !strings.Contains(out, "lane=articles reason=timeout") {
		t.Fatalf("log = %q, want both lanes logged with their own reasons", out)
	}
}

// A docs hit whose content read misses the deadline keeps its document, with
// its summary as the excerpt, and the page is marked partial.
func TestSearchAllContentTimeoutKeepsDocWithSummary(t *testing.T) {
	yt := &fakeSearchYT{
		articles:  []Article{{ID: "101-1", Summary: "doc one", Content: "body text"}},
		pathDelay: map[string]time.Duration{"/api/articles/101-1": 2 * time.Second},
	}
	fs, svc := newSearchTest(t, yt, 150*time.Millisecond)
	seedDocArticles(t, fs, map[string]string{"design/one": "101-1"})

	page, err := svc.SearchPage(context.Background(), "one", "all", "", 20)
	if err != nil {
		t.Fatalf("scope all with a slow content read: %v", err)
	}
	if keys := searchKeys(page.Results); strings.Join(keys, ",") != "docs:design/one" {
		t.Fatalf("keys = %v, want the doc kept", keys)
	}
	if got := page.Results[0].Snippet; got != "doc one" {
		t.Fatalf("snippet = %q, want the summary fallback", got)
	}
	if !page.Partial || !page.TimedOut {
		t.Fatalf("page = partial %v timed_out %v, want both set", page.Partial, page.TimedOut)
	}
}

func TestSearchHelpers(t *testing.T) {
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
	if q := issueSearchQuery([]string{"HK", "BIG"}, "x"); q != "(project: {HK} or project: {BIG}) and (x)" {
		t.Fatalf("issueSearchQuery = %s", q)
	}
	// Korean partial words: matches individually, not wrapped in one phrase
	for _, kw := range []string{"좌회", "택배", "배달 루프", "그랜저"} {
		if q := issueSearchQuery([]string{"HK"}, kw); q != "(project: {HK}) and ("+kw+")" {
			t.Fatalf("issueSearchQuery Korean %q = %s", kw, q)
		}
	}
	// Each special character escaped literally
	escapeCases := []struct {
		in   string
		want string
	}{
		{"project: HK", `project\: HK`},
		{"for: me", `for\: me`},
		{"#123", `\#123`},
		{"#tag", `\#tag`},
		{"unclosed { brace", `unclosed \{ brace`},
		{"closed { brace }", `closed \{ brace \}`},
		{"back\\slash", `back\\slash`},
		{`"quoted text"`, `\"quoted text\"`},
		{"parens (test)", `parens \(test\)`},
		{"project: HK #bug {test} \"phrase\" \\path 좌회", `project\: HK \#bug \{test\} \"phrase\" \\path 좌회`},
		{"or project: X", `project\: X`},
	}
	for _, c := range escapeCases {
		if got := escapeYouTrackQuery(c.in); got != c.want {
			t.Errorf("escapeYouTrackQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if q := issueSearchQuery([]string{"HK"}, "for: me"); q != `(project: {HK}) and (for\: me)` {
		t.Fatalf("issueSearchQuery for: me = %s", q)
	}
	if q := issueSearchQuery([]string{"HK"}, "unclosed { brace"); q != `(project: {HK}) and (unclosed \{ brace)` {
		t.Fatalf("issueSearchQuery brace = %s", q)
	}
	if q := issueSearchQuery([]string{"HK"}, "or project: X"); q != `(project: {HK}) and (project\: X)` {
		t.Fatalf("issueSearchQuery leading conjunction = %s", q)
	}
}
