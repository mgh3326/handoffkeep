package ytwrite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The document's article goes under its parent (hk 3995): the amended policy
// article when the payload names one, else its family root, which is created
// first when the project has none.

// docArticleOf returns the article with the document's summary.
func docArticleOf(t *testing.T, e articleEnv) ytwrite.RemoteArticle {
	t.Helper()
	for _, a := range e.yt.Articles(artProject) {
		if a.Summary == artSummary {
			return a
		}
	}
	t.Fatalf("no article with summary %q in %+v", artSummary, e.yt.Articles(artProject))
	return ytwrite.RemoteArticle{}
}

func prepareWith(t *testing.T, e articleEnv, req ytwrite.ArticleRequest) ytwrite.Op {
	t.Helper()
	n, err := ytwrite.ArticleOp(req)
	if err != nil {
		t.Fatal(err)
	}
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestArticleCreateCreatesTheFamilyRootFirst(t *testing.T) {
	e := newArticleEnv(t)
	req := articleReq()
	req.ParentFamily = "design"
	op := prepareWith(t, e, req)
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	arts := e.yt.Articles(artProject)
	if len(arts) != 2 {
		t.Fatalf("articles %+v, want the family root and the document", arts)
	}
	root := arts[0]
	if root.Summary != "design" || root.ParentID != "" || !strings.Contains(root.Content, ytwrite.FamilyRootMarker("design")) {
		t.Fatalf("family root %+v", root)
	}
	if doc := docArticleOf(t, e); doc.ParentID != root.ID {
		t.Fatalf("document under %q, want family root %q", doc.ParentID, root.ID)
	}
}

func TestArticleCreateAdoptsTheExistingFamilyRoot(t *testing.T) {
	e := newArticleEnv(t)
	root := e.yt.InjectArticle(artProject, "design", "design documents\n\n"+ytwrite.FamilyRootMarker("design"))
	req := articleReq()
	req.ParentFamily = "design"
	op := prepareWith(t, e, req)
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	if n := len(e.yt.Articles(artProject)); n != 2 {
		t.Fatalf("%d articles, want the injected root and the document only", n)
	}
	if doc := docArticleOf(t, e); doc.ParentID != root {
		t.Fatalf("document under %q, want the existing root %q", doc.ParentID, root)
	}
}

// Two roots carrying the family's marker are ambiguous: nothing is sent.
func TestArticleCreateTwoFamilyRootsRefused(t *testing.T) {
	e := newArticleEnv(t)
	e.yt.InjectArticle(artProject, "design", ytwrite.FamilyRootMarker("design"))
	e.yt.InjectArticle(artProject, "design", "again\n\n"+ytwrite.FamilyRootMarker("design"))
	req := articleReq()
	req.ParentFamily = "design"
	op := prepareWith(t, e, req)
	e.drive()
	e.want(op.ID, ytwrite.StateRejected, ytwrite.ReasonAmbiguousIssue)
	if e.yt.Calls() != 0 {
		t.Fatalf("%d writes, want none", e.yt.Calls())
	}
}

// A decision's article goes under the policy article it amends; no family
// root is created for it.
func TestArticleCreateUnderTheAmendedPolicyArticle(t *testing.T) {
	e := newArticleEnv(t)
	policy := e.yt.InjectArticle(artProject, "policy/index", "the policy index")
	req := articleReq()
	req.ParentArticleID = policy
	op := prepareWith(t, e, req)
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	if n := len(e.yt.Articles(artProject)); n != 2 {
		t.Fatalf("%d articles, want the policy article and the document only", n)
	}
	if doc := docArticleOf(t, e); doc.ParentID != policy {
		t.Fatalf("document under %q, want policy article %q", doc.ParentID, policy)
	}
}

func TestCheckArticleParents(t *testing.T) {
	ok := articleReq()
	ok.ParentFamily = "design"
	if err := ytwrite.CheckArticle(ok); err != nil {
		t.Fatalf("a family parent was refused: %v", err)
	}
	for name, edit := range map[string]func(*ytwrite.ArticleRequest){
		"family_with_space": func(r *ytwrite.ArticleRequest) { r.ParentFamily = "de sign" },
		"family_marker":     func(r *ytwrite.ArticleRequest) { r.ParentFamily = "a-->b" },
		"both_parents": func(r *ytwrite.ArticleRequest) {
			r.ParentFamily = "design"
			r.ParentArticleID = "7-1"
		},
	} {
		r := articleReq()
		edit(&r)
		if err := ytwrite.CheckArticle(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
