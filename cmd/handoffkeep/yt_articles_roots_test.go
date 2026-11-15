package main

import (
	"context"
	"errors"
	"fmt"
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

// fakeRootsYT is a YouTrack of family roots and their children, in memory:
// the roots read, the parent move and the delete the merge makes (hk 4806).
type fakeRootsYT struct {
	order    []string
	articles map[string]*youtrack.Article
	// extraChildren is what ReadArticle reports beside the live children: a
	// child that arrived between the plan and the re-read.
	extraChildren map[string][]youtrack.ArticleRef
	// writes records the moves and deletes in the order they happened.
	writes     []string
	failMove   map[string]error
	failDelete map[string]error
}

func newFakeRootsYT(articles ...youtrack.Article) *fakeRootsYT {
	f := &fakeRootsYT{articles: map[string]*youtrack.Article{}, extraChildren: map[string][]youtrack.ArticleRef{},
		failMove: map[string]error{}, failDelete: map[string]error{}}
	for i := range articles {
		a := articles[i]
		f.order = append(f.order, a.ID)
		f.articles[a.ID] = &a
	}
	return f
}

func (f *fakeRootsYT) ListArticles(context.Context) ([]youtrack.Article, error) {
	var out []youtrack.Article
	for _, id := range f.order {
		a, ok := f.articles[id]
		if !ok {
			continue
		}
		listed := *a
		listed.Content = "" // the list read carries no content
		out = append(out, listed)
	}
	return out, nil
}

func (f *fakeRootsYT) ReadArticle(_ context.Context, id string) (youtrack.Article, error) {
	a, ok := f.articles[id]
	if !ok {
		return youtrack.Article{}, fmt.Errorf("article %s: not found", id)
	}
	out := *a
	out.ChildArticles = append([]youtrack.ArticleRef(nil), f.extraChildren[id]...)
	for _, other := range f.order {
		c, ok := f.articles[other]
		if ok && c.HasParent() && c.ParentArticle.ID == id {
			out.ChildArticles = append(out.ChildArticles, youtrack.ArticleRef{ID: other})
		}
	}
	return out, nil
}

func (f *fakeRootsYT) SetArticleParent(_ context.Context, id, parent string) (youtrack.Article, error) {
	if err := f.failMove[id]; err != nil {
		return youtrack.Article{}, err
	}
	a, ok := f.articles[id]
	if !ok {
		return youtrack.Article{}, fmt.Errorf("article %s: not found", id)
	}
	if _, ok := f.articles[parent]; !ok {
		return youtrack.Article{}, fmt.Errorf("parent %s: not found", parent)
	}
	f.writes = append(f.writes, "move "+id+"->"+parent)
	a.ParentArticle = &youtrack.ArticleRef{ID: parent}
	return *a, nil
}

func (f *fakeRootsYT) DeleteArticle(_ context.Context, id string) error {
	if err := f.failDelete[id]; err != nil {
		return err
	}
	if _, ok := f.articles[id]; !ok {
		return fmt.Errorf("article %s: not found", id)
	}
	f.writes = append(f.writes, "delete "+id)
	delete(f.articles, id)
	return nil
}

// fakeFamilyRoots is the store's recorded root, by project and family.
type fakeFamilyRoots map[string]string

func (f fakeFamilyRoots) FamilyRoot(_ context.Context, _, projectID, family string) (string, bool, error) {
	id, ok := f[projectID+"/"+family]
	return id, ok, nil
}

// rootOf is a top-level article carrying its family's marker.
func rootOf(project, id, family string, created int64) youtrack.Article {
	a := article(project, id, family, created)
	a.Content = ytwrite.FamilyRootContent(family)
	return a
}

// kidOf is one article under a root.
func kidOf(project, id, parent, summary string) youtrack.Article {
	return youtrack.Article{ID: id, Summary: summary, Project: &youtrack.ProjectRef{ID: project, ShortName: "SHRDOC"},
		ParentArticle: &youtrack.ArticleRef{ID: parent}}
}

func mustRootsMergePlan(t *testing.T, f *fakeRootsYT, rec ytFamilyRootSource) ytRootsMergeReport {
	t.Helper()
	report, err := ytRootsMergePlan(context.Background(), f, rec, "yt.example.com", "", "")
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// The plan keeps the recorded root, the oldest when nothing is recorded, and
// refuses a family whose recorded root and oldest disagree or whose recorded
// root is no longer a root. A dry run writes nothing (hk 4806).
func TestYTRootsMergePlanKeptRoot(t *testing.T) {
	roots := []youtrack.Article{rootOf("0-30", "3-1", "briefs", 1000), rootOf("0-30", "3-2", "briefs", 2000)}
	cases := []struct {
		name     string
		recorded fakeFamilyRoots
		kept     string
		source   string
		refused  string
	}{
		{"none recorded", fakeFamilyRoots(nil), "3-1", ytKeptOldest, ""},
		{"recorded is the oldest", fakeFamilyRoots{"0-30/briefs": "3-1"}, "3-1", ytKeptRecorded, ""},
		{"recorded is not the oldest", fakeFamilyRoots{"0-30/briefs": "3-2"}, "", "", ytRefusedRecordedNotOldest},
		{"recorded is gone", fakeFamilyRoots{"0-30/briefs": "3-9"}, "", "", ytRefusedRecordedMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeRootsYT(roots...)
			report := mustRootsMergePlan(t, f, tc.recorded)
			if len(report.Families) != 1 {
				t.Fatalf("families %+v", report.Families)
			}
			fam := report.Families[0]
			if fam.Kept != tc.kept || fam.KeptSource != tc.source || fam.Refused != tc.refused {
				t.Fatalf("family %+v, want kept %q (%s) refused %q", fam, tc.kept, tc.source, tc.refused)
			}
			if tc.refused != "" && (fam.Counts.Refused != 1 || report.Counts.Refused != 1 || len(fam.Duplicates) != 0) {
				t.Fatalf("refused family %+v", fam)
			}
			if len(f.writes) != 0 {
				t.Fatalf("the dry run wrote: %v", f.writes)
			}
		})
	}
}

// The plan names every child of a duplicate, by id and summary, and the
// delete that follows the moves.
func TestYTRootsMergePlanChildren(t *testing.T) {
	f := newFakeRootsYT(
		rootOf("0-30", "3-1", "briefs", 1000),
		rootOf("0-30", "3-2", "briefs", 2000),
		kidOf("0-30", "3-91", "3-2", "[hk] brief [doc]"),
		kidOf("0-30", "3-90", "3-2", "[hk] deploy [doc]"),
	)
	report := mustRootsMergePlan(t, f, fakeFamilyRoots{})
	if len(report.Families) != 1 {
		t.Fatalf("families %+v", report.Families)
	}
	fam := report.Families[0]
	if fam.Kept != "3-1" || fam.KeptSource != ytKeptOldest || len(fam.Duplicates) != 1 {
		t.Fatalf("family %+v", fam)
	}
	dup := fam.Duplicates[0]
	if dup.ArticleID != "3-2" || !dup.Delete || dup.Skip != "" {
		t.Fatalf("duplicate %+v, want 3-2 deleted", dup)
	}
	if len(dup.Children) != 2 || dup.Children[0].ArticleID != "3-90" || dup.Children[1].ArticleID != "3-91" ||
		dup.Children[0].Summary != "[hk] deploy [doc]" || dup.Children[1].Summary != "[hk] brief [doc]" {
		t.Fatalf("children %+v", dup.Children)
	}
	if len(f.writes) != 0 {
		t.Fatalf("the dry run wrote: %v", f.writes)
	}
}

// The apply moves a duplicate's children under the kept root, then deletes
// the emptied duplicate, and never deletes the kept root.
func TestYTRootsMergeApplyMovesThenDeletes(t *testing.T) {
	f := newFakeRootsYT(
		rootOf("0-30", "3-1", "briefs", 1000),
		rootOf("0-30", "3-2", "briefs", 2000),
		kidOf("0-30", "3-90", "3-2", "one"),
		kidOf("0-30", "3-91", "3-2", "two"),
	)
	report := ytRootsMergeApply(context.Background(), f, mustRootsMergePlan(t, f, fakeFamilyRoots{}))
	want := []string{"move 3-90->3-1", "move 3-91->3-1", "delete 3-2"}
	if len(f.writes) != len(want) {
		t.Fatalf("writes %v, want %v", f.writes, want)
	}
	for i := range want {
		if f.writes[i] != want[i] {
			t.Fatalf("writes %v, want %v", f.writes, want)
		}
	}
	if _, ok := f.articles["3-1"]; !ok {
		t.Fatal("the kept root was deleted")
	}
	if got := (ytRootsMergeCounts{Moved: 2, Deleted: 1}); report.Counts != got {
		t.Fatalf("counts %+v, want %+v", report.Counts, got)
	}
	if len(report.Families) != 1 || report.Families[0].Error != "" || report.Families[0].Duplicates[0].Skip != "" {
		t.Fatalf("family %+v", report.Families)
	}
}

// A duplicate that still has a child after its moves is left in place: the
// re-read proves it, not the plan.
func TestYTRootsMergeApplyLeavesADuplicateWithChildren(t *testing.T) {
	f := newFakeRootsYT(
		rootOf("0-30", "3-1", "briefs", 1000),
		rootOf("0-30", "3-2", "briefs", 2000),
		kidOf("0-30", "3-90", "3-2", "one"),
	)
	plan := mustRootsMergePlan(t, f, fakeFamilyRoots{})
	f.extraChildren["3-2"] = []youtrack.ArticleRef{{ID: "3-99"}}
	report := ytRootsMergeApply(context.Background(), f, plan)
	if len(f.writes) != 1 || f.writes[0] != "move 3-90->3-1" {
		t.Fatalf("writes %v, want the move only", f.writes)
	}
	dup := report.Families[0].Duplicates[0]
	if dup.Delete || dup.Skip != ytSkipHasChildren {
		t.Fatalf("duplicate %+v, want has_children", dup)
	}
	if got := (ytRootsMergeCounts{Moved: 1, Skipped: 1}); report.Counts != got {
		t.Fatalf("counts %+v, want %+v", report.Counts, got)
	}
}

// A run that fails partway stops that family and converges on the next run:
// the children already moved are not moved again, and the duplicates left are
// taken then.
func TestYTRootsMergeApplyRerunConverges(t *testing.T) {
	f := newFakeRootsYT(
		rootOf("0-30", "3-1", "briefs", 1000),
		rootOf("0-30", "3-2", "briefs", 2000),
		rootOf("0-30", "3-3", "briefs", 3000),
		kidOf("0-30", "3-90", "3-2", "one"),
		kidOf("0-30", "3-91", "3-2", "two"),
		kidOf("0-30", "3-92", "3-3", "three"),
	)
	f.failMove["3-91"] = errors.New("boom")
	first := ytRootsMergeApply(context.Background(), f, mustRootsMergePlan(t, f, fakeFamilyRoots{}))
	if got := (ytRootsMergeCounts{Moved: 1, Skipped: 2}); first.Counts != got || first.failedCount() != 1 {
		t.Fatalf("first run %+v failed %d, want %+v with one failure", first.Counts, first.failedCount(), got)
	}
	delete(f.failMove, "3-91")
	second := ytRootsMergeApply(context.Background(), f, mustRootsMergePlan(t, f, fakeFamilyRoots{}))
	if got := (ytRootsMergeCounts{Moved: 2, Deleted: 2}); second.Counts != got || second.failedCount() != 0 {
		t.Fatalf("second run %+v, want %+v", second.Counts, got)
	}
	if _, ok := f.articles["3-2"]; ok {
		t.Fatal("a duplicate root survived the second run")
	}
	if _, ok := f.articles["3-3"]; ok {
		t.Fatal("a duplicate root survived the second run")
	}
	third := mustRootsMergePlan(t, f, fakeFamilyRoots{})
	if len(third.Families) != 0 || third.Counts != (ytRootsMergeCounts{}) {
		t.Fatalf("third plan %+v, want no duplicate left", third)
	}
	writes := len(f.writes)
	ytRootsMergeApply(context.Background(), f, third)
	if len(f.writes) != writes {
		t.Fatalf("the converged run wrote: %v", f.writes[writes:])
	}
	for _, w := range f.writes {
		if w == "delete 3-1" {
			t.Fatal("the kept root was deleted")
		}
	}
}

// A delete failure stops the family: the next duplicate is not processed,
// and the failing duplicate is counted as skipped once (hk 4806 review).
func TestYTRootsMergeApplyDeleteFailureStopsTheFamily(t *testing.T) {
	f := newFakeRootsYT(
		rootOf("0-30", "3-1", "briefs", 1000),
		rootOf("0-30", "3-2", "briefs", 2000),
		rootOf("0-30", "3-3", "briefs", 3000),
		kidOf("0-30", "3-90", "3-2", "one"),
		kidOf("0-30", "3-91", "3-3", "two"),
	)
	f.failDelete["3-2"] = errors.New("nope")
	report := ytRootsMergeApply(context.Background(), f, mustRootsMergePlan(t, f, fakeFamilyRoots{}))
	if len(f.writes) != 1 || f.writes[0] != "move 3-90->3-1" {
		t.Fatalf("writes %v, want the first duplicate's move only", f.writes)
	}
	if got := (ytRootsMergeCounts{Moved: 1, Skipped: 2}); report.Counts != got {
		t.Fatalf("counts %+v, want %+v", report.Counts, got)
	}
	if report.failedCount() != 1 {
		t.Fatalf("failures %d, want 1", report.failedCount())
	}
	for _, dup := range report.Families[0].Duplicates {
		if dup.Delete || dup.Skip != ytSkipFamilyStopped {
			t.Fatalf("duplicate %+v, want family_stopped", dup)
		}
	}
	if _, ok := f.articles["3-3"]; !ok {
		t.Fatal("the second duplicate was deleted after the family stopped")
	}
	if got := f.articles["3-91"].ParentArticle.ID; got != "3-3" {
		t.Fatalf("child 3-91 moved to %s after the family stopped", got)
	}
}
