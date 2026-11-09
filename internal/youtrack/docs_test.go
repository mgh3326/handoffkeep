package youtrack

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// These fake-store methods back the docs sync. They follow the SQL in
// internal/store/youtrack_docs.go: LIKE prefix patterns, a document is due when
// it has no article or its sha256 differs, and orphans are ledger rows whose
// document is gone.

// DueYouTrackDocs mirrors the store query: a document is due when it has no
// article, its sha256 changed, or its article is in another project than the
// document's project resolves to (the task's project, else defaultProject). A
// parked or backed-off key is not due, and the rest are ordered by their next
// attempt time and then by key.
func (f *fakeStore) DueYouTrackDocs(_ context.Context, patterns []string, defaultProject string, limit int) ([]store.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	keys := make([]string, 0, len(f.docs))
	for k := range f.docs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ri, rj := f.rankAttempt(store.YouTrackQueueDoc, keys[i]), f.rankAttempt(store.YouTrackQueueDoc, keys[j])
		if !ri.Equal(rj) {
			return ri.Before(rj)
		}
		return keys[i] < keys[j]
	})
	out := []store.Document{}
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		if !matchesDocPattern(k, patterns) {
			continue
		}
		d := f.docs[k]
		a, ok := f.docArticles[k]
		if ok && a.SourceSHA == d.SHA256 && a.HKProject == f.docProject(d, defaultProject) {
			continue
		}
		if !f.dueAttempt(store.YouTrackQueueDoc, k, now) {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeStore) AllYouTrackDocs(_ context.Context, limit int) ([]store.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, 0, len(f.docs))
	for k := range f.docs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []store.Document{}
	for _, k := range keys {
		if len(out) >= limit {
			break
		}
		out = append(out, f.docs[k])
	}
	return out, nil
}

// docProject is the project a document resolves to, as the store query does:
// its task's project when set and not blank, else the default; a name with no
// mapping falls back to "other", and no mapping at all resolves to "".
func (f *fakeStore) docProject(d store.Document, defaultProject string) string {
	name := defaultProject
	if d.TaskID != nil {
		if task, ok := f.tasks[*d.TaskID]; ok && task.Project != nil && strings.TrimSpace(*task.Project) != "" {
			name = strings.TrimSpace(*task.Project)
		}
	}
	if _, ok := f.projects[name]; ok {
		return name
	}
	if _, ok := f.projects["other"]; ok {
		return "other"
	}
	return ""
}

func matchesDocPattern(key string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasPrefix(key, strings.TrimSuffix(p, "%")) {
			return true
		}
	}
	return false
}

func (f *fakeStore) OrphanYouTrackDocArticles(_ context.Context, limit int) ([]store.YouTrackDocArticle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.YouTrackDocArticle{}
	for k, a := range f.docArticles {
		if strings.HasPrefix(k, "index:") {
			continue
		}
		if _, ok := f.docs[k]; !ok {
			out = append(out, a)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) GetYouTrackDocArticle(_ context.Context, docKey string) (store.YouTrackDocArticle, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.docArticles[docKey]
	return a, ok, nil
}

func (f *fakeStore) YouTrackDocArticles(_ context.Context, key string) ([]store.YouTrackDocArticle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []store.YouTrackDocArticle{}
	for _, a := range f.docArticles {
		if a.DocumentKey == key || (a.DocumentKey == "" && a.DocKey == key) {
			if a.PartIndex == 0 {
				a.PartIndex = 1
			}
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PartIndex < out[j].PartIndex })
	return out, nil
}

func (f *fakeStore) DeleteYouTrackDocArticleParts(_ context.Context, key string, from int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, a := range f.docArticles {
		if a.DocumentKey == key && a.PartIndex >= from {
			delete(f.docArticles, k)
		}
	}
	return nil
}

func (f *fakeStore) UpsertYouTrackDocArticle(_ context.Context, x store.YouTrackDocArticle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if x.DocumentKey == "" {
		x.DocumentKey = x.DocKey
	}
	if x.PartIndex == 0 {
		x.PartIndex = 1
	}
	if x.PartCount == 0 {
		x.PartCount = 1
	}
	f.docArticles[x.DocKey] = x
	return nil
}

var docTestTime = time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)

func docSHA(body string) string {
	sum := sha256.Sum256([]byte(body))
	return fmt.Sprintf("%x", sum)
}

func (f *fakeStore) putDoc(key, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[key] = store.Document{Key: key, Kind: "other", Body: body, SHA256: docSHA(body), CreatedBy: "test", CreatedAt: docTestTime, UpdatedAt: docTestTime}
}

func (f *fakeStore) docArticle(key string) (store.YouTrackDocArticle, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.docArticles[key]
	return a, ok
}

func (f *fakeYouTrack) articleSnapshot() (creates, updates int, byID map[string]Article, bodies map[string]string, parents map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	byID = map[string]Article{}
	bodies = map[string]string{}
	parents = map[string]string{}
	for k, v := range f.articles {
		byID[k] = v
	}
	for k, v := range f.articleBodies {
		bodies[k] = v
	}
	for k, v := range f.articleParents {
		parents[k] = v
	}
	return f.articleCreates, f.articleUpdates, byID, bodies, parents
}

func docActions(report SyncReport) map[string]string {
	out := map[string]string{}
	for _, a := range report.Docs {
		out[a.Key] = a.Action
	}
	return out
}

func TestDocsCreateUnderPrefixIndexAndSkipUnchanged(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	fs.putDoc("design/alpha", "# Alpha design\n\nbody A")
	fs.putDoc("decision/beta", "# Beta decision\n\nbody B")
	fs.putDoc("policy/gate", "# Gate policy\n\nagent-only")
	svc := newTestService(t, fy, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Errors) != 0 {
		t.Fatalf("errors: %v", report.Errors)
	}
	creates, _, byID, bodies, parents := fy.articleSnapshot()
	// All document prefixes are included: design, decision and policy.
	if creates != 6 {
		t.Fatalf("creates = %d, want 6 (3 index + 3 docs)", creates)
	}
	actions := docActions(report)
	if actions["design/alpha"] != "create" || actions["decision/beta"] != "create" {
		t.Fatalf("actions = %v", actions)
	}
	if actions["policy/gate"] != "create" {
		t.Fatalf("policy prefix was not mirrored: %v", actions)
	}
	alpha, ok := fs.docArticle("design/alpha")
	if !ok {
		t.Fatal("no ledger row for design/alpha")
	}
	article := byID[alpha.ArticleID]
	if article.Summary != "Alpha design" {
		t.Fatalf("title = %q, want the first heading", article.Summary)
	}
	body := bodies[alpha.ArticleID]
	if !strings.Contains(body, "`design/alpha`") || !strings.Contains(body, "https://console.robinco.dev/ui/doc/design/alpha") {
		t.Fatalf("header lacks key or console link:\n%s", body)
	}
	if !strings.Contains(body, "# Alpha design\n\nbody A") {
		t.Fatalf("body is not kept as is:\n%s", body)
	}
	index, ok := fs.docArticle("index:design:other")
	if !ok {
		t.Fatal("no index row for design under the other project")
	}
	if parents[alpha.ArticleID] != index.ArticleID {
		t.Fatalf("parent = %q, want index %q", parents[alpha.ArticleID], index.ArticleID)
	}
	if report.DocBytes == 0 {
		t.Fatal("DocBytes not reported")
	}

	// Nothing changed: a second run writes no article.
	again, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Docs) != 0 || again.DocBytes != 0 {
		t.Fatalf("unchanged docs were written again: %+v", again.Docs)
	}
	if c, u, _, _, _ := fy.articleSnapshot(); c != creates || u != 0 {
		t.Fatalf("second run calls: creates=%d updates=%d", c, u)
	}
}

func TestDocsUpdateRewritesSameArticle(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	fs.putDoc("design/alpha", "# Alpha\n\nfirst")
	svc := newTestService(t, fy, fs)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	first, _ := fs.docArticle("design/alpha")

	fs.putDoc("design/alpha", "# Alpha\n\nsecond")
	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := docActions(report)["design/alpha"]; got != "update" {
		t.Fatalf("action = %q, want update", got)
	}
	creates, updates, _, bodies, _ := fy.articleSnapshot()
	if updates != 1 || creates != 2 {
		t.Fatalf("creates=%d updates=%d, want 2 and 1 (index + doc, then update)", creates, updates)
	}
	second, _ := fs.docArticle("design/alpha")
	if second.ArticleID != first.ArticleID {
		t.Fatalf("article id changed: %s -> %s", first.ArticleID, second.ArticleID)
	}
	if !strings.Contains(bodies[second.ArticleID], "second") {
		t.Fatalf("article not rewritten:\n%s", bodies[second.ArticleID])
	}
}

func TestDocsOverCapIsSplitIntoParts(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	big := "# Big runbook\n\n" + strings.Repeat("x", docArticleMaxBytes)
	fs.putDoc("runbook/big", big)
	svc := newTestService(t, fy, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := docActions(report)["runbook/big"]; got != "create" {
		t.Fatalf("action = %q, want create", got)
	}
	row, _ := fs.docArticle("runbook/big")
	_, _, _, bodies, _ := fy.articleSnapshot()
	if !strings.Contains(bodies[row.ArticleID], "xxxxxxxx") || report.Docs[0].Parts != 2 {
		t.Fatalf("large content not stored in parts: parts=%d", report.Docs[0].Parts)
	}
	if report.DocBytes < int64(docArticleMaxBytes) {
		t.Fatalf("DocBytes = %d, want full content bytes", report.DocBytes)
	}
}

func TestDocsDryRunWritesNothing(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	fs.putDoc("research/gamma", "# Gamma\n\nbody")
	svc := newTestService(t, fy, fs)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	fs.putDoc("research/new", "# New\n\nplanned")
	createsBefore, updatesBefore, _, _, _ := fy.articleSnapshot()

	report, err := svc.Sync(context.Background(), SyncOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	actions := docActions(report)
	if actions["research/gamma"] != "unchanged" || actions["research/new"] != "would_create" {
		t.Fatalf("actions = %v, want unchanged and would_create", actions)
	}
	if creates, updates, _, _, _ := fy.articleSnapshot(); creates != createsBefore || updates != updatesBefore {
		t.Fatalf("dry run wrote articles: creates=%d updates=%d", creates, updates)
	}
	if _, ok := fs.docArticle("research/new"); ok {
		t.Fatal("dry run wrote the ledger")
	}
	if report.DocBytes == 0 || report.DocPrefixes["research"].Unchanged != 1 || report.DocPrefixes["research"].Create != 1 {
		t.Fatal("dry run omitted planned bytes")
	}
}

func TestDocsMultipartRoundTripShrinkAndAdopt(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	fs.putDoc("reference/bootstrap", "# Bootstrap")
	svc := newTestService(t, fy, fs)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	body := "# 대형 문서\n\n" + strings.Repeat("가나다 abc\n\n", 45_000)
	fs.putDoc("reference/large", body)
	fy.mu.Lock()
	fy.loseArticleResponses = 1
	fy.mu.Unlock()
	first, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Errors) == 0 {
		t.Fatal("expected lost create response to be recorded as an error")
	}
	fs.mu.Lock()
	delete(fs.attempts, attemptKey(store.YouTrackQueueDoc, "reference/large"))
	fs.mu.Unlock()
	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := docActions(report)["reference/large"]; got != "adopt" {
		t.Fatalf("lost create action=%q", got)
	}
	rows, err := fs.YouTrackDocArticles(context.Background(), "reference/large")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d, want 3 parts", len(rows))
	}
	creates, _, _, bodies, _ := fy.articleSnapshot()
	if creates != 5 {
		t.Fatalf("creates=%d, want index + bootstrap + 3 parts", creates)
	}
	for _, r := range rows {
		if len(bodies[r.ArticleID]) > docArticleMaxBytes {
			t.Fatalf("part %d is %d bytes", r.PartIndex, len(bodies[r.ArticleID]))
		}
	}
	got, err := svc.ReadDocumentArticle(context.Background(), "reference/large")
	if err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Fatalf("readback differs: got %d bytes, want %d", len(got), len(body))
	}

	fs.putDoc("reference/large", "# 짧은 문서\n\n"+strings.Repeat("한글", 17_000))
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	rows, err = fs.YouTrackDocArticles(context.Background(), "reference/large")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("after shrink rows=%d, want 1", len(rows))
	}
	_, _, articles, _, _ := fy.articleSnapshot()
	if len(articles) != 3 {
		t.Fatalf("after shrink articles=%d, want index + bootstrap + primary", len(articles))
	}
	if _, err := svc.ReadDocumentArticle(context.Background(), "reference/large"); err != nil {
		t.Fatal(err)
	}
}

func TestDocsPartialPartFailureKeepsDocumentDueUntilComplete(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	key := "reference/retry"
	fs.putDoc(key, "# Retry\n\n"+strings.Repeat("x", 300<<10))
	svc := newTestService(t, fy, fs)
	fy.mu.Lock()
	fy.failSummary = key + " (2/2)"
	fy.mu.Unlock()
	first, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Errors) == 0 {
		t.Fatal("expected second-part create failure")
	}
	if rows, _ := fs.YouTrackDocArticles(context.Background(), key); len(rows) != 0 {
		t.Fatalf("partial document has completion ledger: %+v", rows)
	}
	fy.mu.Lock()
	fy.failSummary = ""
	fy.mu.Unlock()
	fs.mu.Lock()
	delete(fs.attempts, attemptKey(store.YouTrackQueueDoc, key))
	fs.mu.Unlock()
	second, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Errors) != 0 {
		t.Fatalf("retry errors: %v", second.Errors)
	}
	rows, err := fs.YouTrackDocArticles(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].SourceSHA != fs.docs[key].SHA256 || rows[0].PartCount != 2 {
		t.Fatalf("completion ledger: %+v", rows)
	}
	if _, err := svc.ReadDocumentArticle(context.Background(), key); err != nil {
		t.Fatalf("completed read-back: %v", err)
	}
}

func TestDocsFailedSurplusDeleteStaysDueAndReadbackRejectsMixedParts(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	key := "research/shrink-retry"
	fs.putDoc(key, "# Large\n\n"+strings.Repeat("x", 300<<10))
	svc := newTestService(t, fy, fs)
	if _, err := svc.Sync(context.Background(), SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	fs.putDoc(key, "# Small\n\n"+strings.Repeat("한글", 17_000))
	fy.mu.Lock()
	fy.failArticleDelete = true
	fy.mu.Unlock()
	first, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Errors) == 0 {
		t.Fatal("expected surplus delete failure")
	}
	if _, err := svc.ReadDocumentArticle(context.Background(), key); err == nil {
		t.Fatal("mixed ledger returned a successful read-back")
	}
	fy.mu.Lock()
	fy.failArticleDelete = false
	fy.mu.Unlock()
	fs.mu.Lock()
	delete(fs.attempts, attemptKey(store.YouTrackQueueDoc, key))
	fs.mu.Unlock()
	second, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Errors) != 0 {
		t.Fatalf("retry errors: %v", second.Errors)
	}
	rows, err := fs.YouTrackDocArticles(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].PartCount != 1 {
		t.Fatalf("shrunk ledger: %+v", rows)
	}
	got, err := svc.ReadDocumentArticle(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	want := "# Small\n\n" + strings.Repeat("한글", 17_000)
	if got != want {
		t.Fatalf("readback after retry differs: %d bytes", len(got))
	}
}

func TestDocsDeletedDocumentIsLoggedNotDeleted(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	fs.mu.Lock()
	fs.docArticles["review/gone"] = store.YouTrackDocArticle{DocKey: "review/gone", HKProject: "other", ArticleID: "A-9", SourceSHA: "old", SyncedAt: docTestTime}
	fs.mu.Unlock()
	svc := newTestService(t, fy, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range report.Warnings {
		if strings.Contains(w, "review/gone") && strings.Contains(w, "A-9") {
			found = true
		}
	}
	if !found {
		t.Fatalf("deleted document not logged: %v", report.Warnings)
	}
	if creates, updates, _, _, _ := fy.articleSnapshot(); creates != 0 || updates != 0 {
		t.Fatalf("delete was propagated: creates=%d updates=%d", creates, updates)
	}
}
