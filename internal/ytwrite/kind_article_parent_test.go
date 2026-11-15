package ytwrite_test

import (
	"context"
	"testing"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
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
	// The root is looked up by its own sentence and by its marker, never by the
	// family word every document of the family carries (hk 4784).
	searches := e.yt.Searches()
	if len(searches) != 3 || searches[0].Query != ytwrite.FamilyRootPhrase("design") || searches[1].Query != ytwrite.FamilyRootMarker("design") ||
		searches[0].Summary != "design" || searches[0].ParentID != "" || searches[2].Query != artSummary {
		t.Fatalf("searches %+v, want the family root by its sentence and marker, then the document's summary", searches)
	}
	arts := e.yt.Articles(artProject)
	if len(arts) != 2 {
		t.Fatalf("articles %+v, want the family root and the document", arts)
	}
	root := arts[0]
	if root.Summary != "design" || root.ParentID != "" || root.Content != ytwrite.FamilyRootContent("design") {
		t.Fatalf("family root %+v", root)
	}
	if doc := docArticleOf(t, e); doc.ParentID != root.ID {
		t.Fatalf("document under %q, want family root %q", doc.ParentID, root.ID)
	}
}

func TestArticleCreateAdoptsTheExistingFamilyRoot(t *testing.T) {
	e := newArticleEnv(t)
	root := e.yt.InjectArticle(artProject, "design", ytwrite.FamilyRootContent("design"))
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

// A root an operator edited — its sentence gone, the marker kept — is still the
// family's root: the lookup asks for the marker too, so the create adopts it
// instead of making a second one beside it (review of PR #441).
func TestArticleCreateAdoptsAnEditedFamilyRoot(t *testing.T) {
	e := newArticleEnv(t)
	root := e.yt.InjectArticle(artProject, "design", "design documents, rewritten by hand\n\n"+ytwrite.FamilyRootMarker("design"))
	req := articleReq()
	req.ParentFamily = "design"
	op := prepareWith(t, e, req)
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	if n := len(e.yt.Articles(artProject)); n != 2 {
		t.Fatalf("%d articles, want the edited root and the document only", n)
	}
	if doc := docArticleOf(t, e); doc.ParentID != root {
		t.Fatalf("document under %q, want the edited root %q", doc.ParentID, root)
	}
}

// Two roots carrying the family's marker — a retry an earlier release made, or
// a copy a person made — are not ambiguous: the OLDEST is the family's root,
// every document goes under it, and nothing is created (hk 4797). The marker
// search alone could see only one of them, the sentence search the other.
func TestArticleCreateUsesTheOldestFamilyRoot(t *testing.T) {
	e := newArticleEnv(t)
	oldest := e.yt.InjectArticle(artProject, "design", "again\n\n"+ytwrite.FamilyRootMarker("design"))
	e.yt.InjectArticle(artProject, "design", ytwrite.FamilyRootContent("design"))
	req := articleReq()
	req.ParentFamily = "design"
	op := prepareWith(t, e, req)
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	if e.yt.Calls() != 1 {
		t.Fatalf("%d writes, want the document's one create only", e.yt.Calls())
	}
	if doc := docArticleOf(t, e); doc.ParentID != oldest {
		t.Fatalf("document under %q, want the oldest root %q", doc.ParentID, oldest)
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

// A family-root create that commits but returns no id is reconciled by the
// marker search, never repeated: the root that POST made is the family's, the
// document goes under it, and the op applies in the same dispatch (hk 4797).
func TestArticleCreateRootWithoutIDIsReconciled(t *testing.T) {
	e := newArticleEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.NoID})
	req := articleReq()
	req.ParentFamily = "design"
	op := prepareWith(t, e, req)
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	var roots []ytwrite.RemoteArticle
	for _, a := range e.yt.Articles(artProject) {
		if a.Summary == "design" {
			roots = append(roots, a)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("%d family roots, want exactly 1: %+v", len(roots), roots)
	}
	if doc := docArticleOf(t, e); doc.ParentID != roots[0].ID {
		t.Fatalf("document under %q, want family root %q", doc.ParentID, roots[0].ID)
	}
}
