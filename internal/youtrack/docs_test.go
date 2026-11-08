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

func (f *fakeStore) DueYouTrackDocs(_ context.Context, patterns []string, limit int) ([]store.Document, error) {
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
		if !matchesDocPattern(k, patterns) {
			continue
		}
		d := f.docs[k]
		if a, ok := f.docArticles[k]; ok && a.SourceSHA == d.SHA256 {
			continue
		}
		out = append(out, d)
	}
	return out, nil
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

func (f *fakeStore) UpsertYouTrackDocArticle(_ context.Context, x store.YouTrackDocArticle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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
	// One parent per prefix (design, decision) plus one article per document.
	if creates != 4 {
		t.Fatalf("creates = %d, want 4 (2 index + 2 docs)", creates)
	}
	actions := docActions(report)
	if actions["design/alpha"] != "create" || actions["decision/beta"] != "create" {
		t.Fatalf("actions = %v", actions)
	}
	if _, ok := actions["policy/gate"]; ok {
		t.Fatalf("agent-read policy doc was mirrored: %v", actions)
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

func TestDocsOverCapBecomesStub(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	big := "# Big runbook\n\n" + strings.Repeat("x", docArticleMaxBytes)
	fs.putDoc("runbook/big", big)
	svc := newTestService(t, fy, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := docActions(report)["runbook/big"]; got != "stub" {
		t.Fatalf("action = %q, want stub", got)
	}
	row, _ := fs.docArticle("runbook/big")
	_, _, _, bodies, _ := fy.articleSnapshot()
	stub := bodies[row.ArticleID]
	if strings.Contains(stub, "xxxxxxxx") || !strings.Contains(stub, "hk에서 읽어 주세요") {
		t.Fatalf("stub should link to hk, not carry the body: %q", stub[:min(len(stub), 200)])
	}
	if report.DocBytes >= int64(docArticleMaxBytes) {
		t.Fatalf("DocBytes = %d, a stub must not count the whole body", report.DocBytes)
	}
}

func TestDocsDryRunWritesNothing(t *testing.T) {
	fs := newFakeStore()
	fy := newFakeYouTrack()
	fs.putDoc("research/gamma", "# Gamma\n\nbody")
	svc := newTestService(t, fy, fs)

	report, err := svc.Sync(context.Background(), SyncOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := docActions(report)["research/gamma"]; got != "would_create" {
		t.Fatalf("action = %q, want would_create", got)
	}
	if creates, updates, _, _, _ := fy.articleSnapshot(); creates != 0 || updates != 0 {
		t.Fatalf("dry run wrote articles: creates=%d updates=%d", creates, updates)
	}
	if _, ok := fs.docArticle("research/gamma"); ok {
		t.Fatal("dry run wrote the ledger")
	}
	if report.DocBytes != 0 {
		t.Fatalf("dry run counted bytes written: %d", report.DocBytes)
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
