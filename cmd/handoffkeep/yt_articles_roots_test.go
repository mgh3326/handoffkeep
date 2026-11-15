package main

import (
	"context"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/youtrack"
	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// fakeArticleList is the YouTrack read the roots report makes (hk 4797).
type fakeArticleList struct {
	all    []youtrack.Article
	byID   map[string]youtrack.Article
	reads  []string
	fails  map[string]error
	listEr error
}

func (f *fakeArticleList) ListArticles(context.Context) ([]youtrack.Article, error) {
	if f.listEr != nil {
		return nil, f.listEr
	}
	return f.all, nil
}

func (f *fakeArticleList) ReadArticle(_ context.Context, id string) (youtrack.Article, error) {
	f.reads = append(f.reads, id)
	if err := f.fails[id]; err != nil {
		return youtrack.Article{}, err
	}
	return f.byID[id], nil
}

// article is one top-level article of the shared documents project.
func article(project, id, summary string, created int64) youtrack.Article {
	return youtrack.Article{ID: id, Summary: summary, Project: &youtrack.ProjectRef{ID: project, ShortName: "SHRDOC"}, Created: created}
}

func childOf(id, parent string) youtrack.Article {
	return youtrack.Article{ID: id, Summary: "[hk] brief [doc]", Project: &youtrack.ProjectRef{ID: "0-30", ShortName: "SHRDOC"},
		ParentArticle: &youtrack.ArticleRef{ID: parent}}
}

// The report names every root of a family, the children each has, and the
// oldest as the one hk uses; a second root in another project is another
// family (hk 4797).
func TestYTArticleRootsReport(t *testing.T) {
	roots := map[string]youtrack.Article{
		"3-1": article("0-30", "3-1", "briefs", 1000),
		"3-2": article("0-30", "3-2", "briefs", 2000),
		"3-3": article("0-31", "3-3", "briefs", 3000),
		"3-4": article("0-30", "3-4", "not a family", 4000),
	}
	for id, root := range roots {
		root.Content = ytwrite.FamilyRootContent(root.Summary)
		roots[id] = root
	}
	// An article whose summary is a family name but carries no marker is not
	// a root.
	other := article("0-30", "3-5", "deploy", 5000)
	other.Content = "a person's deploy notes"
	roots["3-5"] = other
	f := &fakeArticleList{byID: roots, fails: map[string]error{}}
	f.all = []youtrack.Article{roots["3-1"], roots["3-2"], roots["3-3"], roots["3-4"], roots["3-5"], childOf("3-9", "3-1"), childOf("3-10", "3-2")}

	report, err := ytArticleRoots(context.Background(), f, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Duplicates != 1 {
		t.Fatalf("duplicates %d, want 1 (two briefs roots on one project)", report.Duplicates)
	}
	got := map[string]ytArticleRoot{}
	for _, r := range report.Roots {
		got[r.Family+"/"+r.Project+"/"+r.ArticleID] = r
	}
	if len(got) != 3 {
		t.Fatalf("roots %+v, want the three the marker names (the non-family summary is never a root)", report.Roots)
	}
	if r := got["briefs/0-30/3-1"]; !r.Used || r.Children != 1 || r.ProjectShort != "SHRDOC" || r.Created != "1970-01-01T00:00:01Z" {
		t.Fatalf("oldest briefs root %+v, want used with one child", r)
	}
	if r := got["briefs/0-30/3-2"]; r.Used || r.Children != 1 {
		t.Fatalf("second briefs root %+v, want unused with one child", r)
	}
	if r := got["briefs/0-31/3-3"]; !r.Used {
		t.Fatalf("the only root of its project %+v, want used", r)
	}
	if _, ok := got["deploy/0-30/3-5"]; ok {
		t.Fatalf("an article with no marker was reported as a root: %+v", report.Roots)
	}

	// --family and --project narrow the same report.
	only, err := ytArticleRoots(context.Background(), f, "briefs", "0-31")
	if err != nil {
		t.Fatal(err)
	}
	if only.Family != "briefs" || len(only.Roots) != 1 || only.Roots[0].ArticleID != "3-3" || only.Duplicates != 0 {
		t.Fatalf("filtered report %+v", only)
	}
}

// A read failure fails the report rather than reporting a family as rootless.
func TestYTArticleRootsReportReadFailure(t *testing.T) {
	f := &fakeArticleList{byID: map[string]youtrack.Article{}, fails: map[string]error{}}
	root := article("0-30", "3-1", "briefs", 1000)
	f.all = []youtrack.Article{root}
	f.fails["3-1"] = context.DeadlineExceeded
	if _, err := ytArticleRoots(context.Background(), f, "", ""); err == nil {
		t.Fatal("a failed article read reported roots")
	}
}
