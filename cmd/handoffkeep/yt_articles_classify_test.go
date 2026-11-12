package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/store"
	"github.com/mgh3326/handoffkeep/internal/youtrack"
)

// ytartRemote is an in-memory ytArticleRemote: articles by id and readable
// id, and counts of each read.
type ytartRemote struct {
	articles []youtrack.Article
	reads    map[string]int
	lists    int
}

func (f *ytartRemote) ReadArticle(_ context.Context, id string) (youtrack.Article, error) {
	f.reads[id]++
	for _, x := range f.articles {
		if x.ID == id || x.IDReadable == id {
			return x, nil
		}
	}
	return youtrack.Article{}, &youtrack.APIError{Status: 404}
}

func (f *ytartRemote) ListArticles(context.Context) ([]youtrack.Article, error) {
	f.lists++
	out := make([]youtrack.Article, len(f.articles))
	for i, x := range f.articles {
		x.Content = ""
		out[i] = x
	}
	return out, nil
}

func ytartHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func ytartDoc(id int64, key, body, bucket, short, projectID string) store.YTArticleDoc {
	return store.YTArticleDoc{YTArticleCandidate: store.YTArticleCandidate{DocumentID: id, Key: key, Kind: "note", SHA256: ytartHex(body),
		BodyBytes: len(body), TaskID: 1, Bucket: bucket, ShortName: short, YTProjectID: projectID}}
}

// classify finds an article by one GET of <SHORT>-A-<document id> when the
// project's first GET verifies, and otherwise by summary in the article
// list; two articles with the summary are ambiguous, and an article of
// another project or a child article never verifies.
func TestYTArticleClassify(t *testing.T) {
	ctx := t.Context()
	art := func(id, readable, project, key, content string) youtrack.Article {
		return youtrack.Article{ID: id, IDReadable: readable, Summary: store.YTArticleSummary(key, "note"), Content: content, Project: &youtrack.ProjectRef{ID: project}}
	}
	f := &ytartRemote{reads: map[string]int{}, articles: []youtrack.Article{
		art("1-1", "RDA-A-11", "0-7", "note/a", "body a"),
		art("1-2", "RDA-A-12", "0-7", "note/b", "old b"),
		art("2-1", "SMA-A-1", "0-8", "note/c", "body c"),
		art("2-2", "SMA-A-2", "0-8", "note/d", "d"),
		art("2-3", "SMA-A-3", "0-8", "note/d", "d"),
		art("2-4", "SMA-A-4", "0-7", "note/e", "e"), // wrong project
		art("2-5", "SMA-A-5", "0-8", "note/f", "f"),
	}}
	child := art("2-6", "SMA-A-6", "0-8", "note/g", "g")
	child.ParentArticle = &youtrack.ArticleRef{ID: "2-5"}
	f.articles = append(f.articles, child)
	a := &ytArticler{remote: f, tenant: "t", lookup: map[string]string{}, buckets: map[string]ytArticleBucket{
		store.YTArticleBucketShared: {ID: "0-3", ShortName: "SHRDOC2"}, store.YTArticleBucketUnassigned: {ID: "0-1", ShortName: "HK"}}}

	check := func(d store.YTArticleDoc, class, article string) ytArticleResult {
		t.Helper()
		got := a.classify(ctx, d, "by")
		if got.class != class || got.binding.ArticleID != article {
			t.Fatalf("document %d: %s %q (%s), want %s %q", d.DocumentID, got.class, got.binding.ArticleID, got.detail, class, article)
		}
		return got
	}
	// Readable-id lookup.
	got := check(ytartDoc(11, "note/a", "body a", "rda", "RDA", "0-7"), ytArticleMatched, "1-1")
	if got.binding.ContentSHA != ytartHex("body a") || got.binding.NeedsUpdate || got.binding.ProjectID != "0-7" || got.binding.ArticleReadable != "RDA-A-11" {
		t.Fatalf("matched binding %+v", got.binding)
	}
	got = check(ytartDoc(12, "note/b", "new b", "rda", "RDA", "0-7"), ytArticleSHADiffers, "1-2")
	if got.binding.ContentSHA != ytartHex("old b") || !got.binding.NeedsUpdate {
		t.Fatalf("sha_differs binding %+v", got.binding)
	}
	if a.lookup["0-7"] != ytArticleLookupReadable || f.lists != 1 || f.reads["RDA-A-11"] != 1 {
		t.Fatalf("lookup %v, %d lists, reads %v", a.lookup, f.lists, f.reads)
	}
	// Summary lookup: the first GET (SMA-A-31) does not exist.
	check(ytartDoc(31, "note/c", "body c", "sma", "SMA", "0-8"), ytArticleMatched, "2-1")
	if a.lookup["0-8"] != ytArticleLookupSummary || f.lists != 1 || f.reads["SMA-A-31"] != 1 || f.reads["2-1"] != 1 {
		t.Fatalf("lookup %v, %d lists, reads %v", a.lookup, f.lists, f.reads)
	}
	check(ytartDoc(32, "note/d", "d", "sma", "SMA", "0-8"), ytArticleAmbiguous, "")
	check(ytartDoc(33, "note/e", "e", "sma", "SMA", "0-8"), ytArticleMissing, "")
	if f.reads["SMA-A-32"] != 0 || f.lists != 1 {
		t.Fatalf("summary lookup read by readable id or listed again: %v, %d lists", f.reads, f.lists)
	}
	// A split document's index article: bound with needs_update, no sha.
	idx := art("2-7", "SMA-A-7", "0-8", "note/big", "# index")
	idx.Summary = store.YTArticleIndexSummary("note/big", "note")
	f.articles = append(f.articles, idx)
	a.index = nil
	got = check(ytartDoc(34, "note/big", "large", "sma", "SMA", "0-8"), ytArticleSHADiffers, "2-7")
	if got.binding.ContentSHA != "" || !got.binding.NeedsUpdate {
		t.Fatalf("split binding %+v", got.binding)
	}
	// A child article never verifies; neither does an unmapped project or
	// one whose youtrack_projects row still names the HK bucket.
	a.index = nil
	check(ytartDoc(39, "note/g", "g", "sma", "SMA", "0-8"), ytArticleMissing, "")
	check(ytartDoc(35, "note/nomap", "x", "nomap", "", ""), ytArticleMissing, "")
	check(ytartDoc(36, "note/hk-row", "x", "handoffkeep", "HK", "0-1"), ytArticleMissing, "")
	// The readable id verifies, but a second top-level article of the
	// project has the summary: ambiguous, not bound (finding r4544954720).
	f.articles = append(f.articles, art("1-3", "RDA-A-40", "0-7", "note/dup", "dup"), art("1-4", "RDA-A-99", "0-7", "note/dup", "dup"))
	a.index = nil
	if got := check(ytartDoc(40, "note/dup", "dup", "rda", "RDA", "0-7"), ytArticleAmbiguous, ""); f.reads["RDA-A-40"] != 1 || !strings.Contains(got.detail, "2 articles") {
		t.Fatalf("readable duplicate: %+v, reads %v", got, f.reads)
	}
	// A shared document is looked up under the bucket's own short name,
	// tail included (finding r4544954807).
	f.articles = append(f.articles, art("3-1", "SHRDOC2-A-41", "0-3", "note/shared", "s"))
	a.index = nil
	check(store.YTArticleDoc{YTArticleCandidate: store.YTArticleCandidate{DocumentID: 41, Key: "note/shared", Kind: "note", SHA256: ytartHex("s"), Bucket: store.YTArticleBucketShared}}, ytArticleMatched, "3-1")
	if f.reads["SHRDOC2-A-41"] != 1 || a.lookup["0-3"] != ytArticleLookupReadable {
		t.Fatalf("shared lookup: reads %v, lookup %v", f.reads, a.lookup)
	}
	// Excluded and bound documents are classified without a read.
	before := len(f.reads)
	rep := ytartDoc(37, "reports/job/report.md", "r", "sma", "SMA", "0-8")
	check(rep, ytArticleExcluded, "")
	bound := ytartDoc(38, "note/c", "body c", "sma", "SMA", "0-8")
	bound.BoundTenant = "t"
	check(bound, ytArticleBound, "")
	bound.BoundTenant = "other"
	check(bound, ytArticleAmbiguous, "")
	if len(f.reads) != before {
		t.Fatalf("reads for excluded or bound documents: %v", f.reads)
	}
}

type ytartProjects []youtrack.AdminProject

func (p ytartProjects) ListAdminProjects(context.Context) ([]youtrack.AdminProject, error) {
	return p, nil
}

// The synthetic buckets are resolved by the names the import gives them,
// not by their base short names: an hk project that took SHRDOC leaves the
// shared documents in SHRDOC2 (client.js getProjects; finding r4544954807).
func TestYTArticleBucketsByImportName(t *testing.T) {
	got, err := ytArticleBuckets(t.Context(), ytartProjects{
		{ID: "0-5", ShortName: "SHRDOC", Name: "shrdoc"},
		{ID: "0-3", ShortName: "SHRDOC2", Name: "Shared documents"},
		{ID: "0-1", ShortName: "HK", Name: "Unassigned hk tasks"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[store.YTArticleBucketShared] != (ytArticleBucket{ID: "0-3", ShortName: "SHRDOC2"}) || got[store.YTArticleBucketUnassigned] != (ytArticleBucket{ID: "0-1", ShortName: "HK"}) {
		t.Fatalf("buckets = %+v", got)
	}
	for _, ps := range []ytartProjects{
		{{ID: "0-1", ShortName: "HK", Name: "Unassigned hk tasks"}},
		{{ID: "0-3", ShortName: "SHRDOC", Name: "Shared documents"}, {ID: "0-4", ShortName: "SHRDOC2", Name: "Shared documents"}, {ID: "0-1", ShortName: "HK", Name: "Unassigned hk tasks"}},
	} {
		if _, err := ytArticleBuckets(t.Context(), ps); !errors.Is(err, youtrack.ErrBucketProject) {
			t.Errorf("%+v: err = %v, want ErrBucketProject", ps, err)
		}
	}
}
