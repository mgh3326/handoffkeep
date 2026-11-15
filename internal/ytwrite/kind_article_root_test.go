package ytwrite_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// Family roots (hk 4797). A root create is not an op, so a root POST whose
// reply was lost once was retried as not_sent and made a second root beside the
// one the first POST had created (30 "briefs" roots, 28 "brief" in production).
// The root is now found before it is created, the oldest root is the family's
// when several exist, the root's id is recorded, and a POST that may have
// reached YouTrack is never retried as "not sent".

// familyRootsOf returns the project's articles that carry the family's marker,
// oldest first (the order the fake lists them in, which is creation order).
func familyRootsOf(t *testing.T, e articleEnv, family string) []ytwrite.RemoteArticle {
	t.Helper()
	var out []ytwrite.RemoteArticle
	for _, a := range e.yt.Articles(artProject) {
		if a.Summary == family && strings.Contains(a.Content, ytwrite.FamilyRootMarker(family)) {
			out = append(out, a)
		}
	}
	return out
}

// docArticleWithSummary returns the document's article by summary.
func docArticleWithSummary(t *testing.T, e articleEnv, summary string) ytwrite.RemoteArticle {
	t.Helper()
	for _, a := range e.yt.Articles(artProject) {
		if a.Summary == summary {
			return a
		}
	}
	t.Fatalf("no article with summary %q in %+v", summary, e.yt.Articles(artProject))
	return ytwrite.RemoteArticle{}
}

// articleReqFor is articleReq for another document of the same project.
func articleReqFor(document int64, summary string) ytwrite.ArticleRequest {
	r := articleReq()
	r.DocumentID = document
	r.Summary = summary
	return r
}

// familyRootSearches counts the searches for a family's root (its sentence and
// its marker), which the document's own summary search is not one of.
func familyRootSearches(e articleEnv, family string) int {
	n := 0
	for _, s := range e.yt.Searches() {
		if s.Summary == family {
			n++
		}
	}
	return n
}

// A root POST that created its root and lost the reply must not be retried:
// the search finds the root it made, the document goes under it, and the op
// applies without a second root.
func TestArticleCreateRootLostReplyLeavesOneRoot(t *testing.T) {
	e := newArticleEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.CommitDropReply}) // the root's POST
	req := articleReq()
	req.ParentFamily = "design"
	prepared := prepareWith(t, e, req)
	e.drive()
	applied := e.want(prepared.ID, ytwrite.StateApplied, "")
	if applied.Attempts != 1 {
		t.Fatalf("op sent %d times, want the one dispatch", applied.Attempts)
	}
	roots := familyRootsOf(t, e, "design")
	if len(roots) != 1 {
		t.Fatalf("%d family roots, want the one the failed POST made: %+v", len(roots), roots)
	}
	if doc := docArticleWithSummary(t, e, artSummary); doc.ParentID != roots[0].ID {
		t.Fatalf("document under %q, want family root %q", doc.ParentID, roots[0].ID)
	}
	if writes := e.yt.ArticleWrites(); len(writes) != 2 {
		t.Fatalf("%d article writes, want the root's and the document's only: %+v", len(writes), writes)
	}
	if got, ok, err := e.ledger.FamilyRoot(context.Background(), decTenant, artProject, "design"); err != nil || !ok || got != roots[0].ID {
		t.Fatalf("recorded family root %q %v %v, want %q", got, ok, err, roots[0].ID)
	}
}

// A root POST that provably never left (a dial failure) is not an uncertain
// write: the op goes back to prepared with the reconcile backoff, and the retry
// creates the family's one root.
func TestArticleCreateRootDialFailureIsNotSent(t *testing.T) {
	e := newArticleEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.DialFail})
	req := articleReq()
	req.ParentFamily = "design"
	op := prepareWith(t, e, req)
	e.drive()
	got := e.want(op.ID, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
	if delay := got.NextAttemptAt.Sub(e.clock.Now()); delay != 10*time.Second {
		t.Fatalf("op due in %s, want the reconcile backoff of 10s", delay)
	}
	if arts := e.yt.Articles(artProject); len(arts) != 0 {
		t.Fatalf("a dial failure created %+v", arts)
	}
	e.clock.Advance(got.NextAttemptAt.Sub(e.clock.Now()))
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	if roots := familyRootsOf(t, e, "design"); len(roots) != 1 {
		t.Fatalf("%d family roots after the retry, want 1: %+v", len(roots), roots)
	}
}

// The recorded root is used by the family's later documents without a search:
// one record, one root, and every document under it.
func TestArticleCreateUsesTheRecordedRoot(t *testing.T) {
	e := newArticleEnv(t)
	second := "[design] design/yt/other.md [note]"
	first := articleReq()
	first.ParentFamily = "design"
	op := prepareWith(t, e, first)
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	if n := familyRootSearches(e, "design"); n != 2 {
		t.Fatalf("%d family root searches for the first document, want the sentence and the marker", n)
	}
	secondReq := articleReqFor(4797, second)
	secondReq.ParentFamily = "design"
	op2 := prepareWith(t, e, secondReq)
	e.drive()
	e.want(op2.ID, ytwrite.StateApplied, "")
	if n := familyRootSearches(e, "design"); n != 2 {
		t.Fatalf("%d family root searches in total, want none for the recorded root", n)
	}
	roots := familyRootsOf(t, e, "design")
	if len(roots) != 1 {
		t.Fatalf("%d family roots, want 1: %+v", len(roots), roots)
	}
	if doc := docArticleWithSummary(t, e, second); doc.ParentID != roots[0].ID {
		t.Fatalf("second document under %q, want the recorded root %q", doc.ParentID, roots[0].ID)
	}
}

// Two concurrent documents of a new family make exactly one root: the store's
// family lane lets one create it while the other waits for a later pass, and
// that pass then uses the root the first recorded. The first root POST is held
// open, so the second document runs while no root exists and the create is in
// flight — the window that made the production duplicates.
func TestArticleCreateConcurrentFamilyMakesOneRoot(t *testing.T) {
	e := newArticleEnv(t)
	second := "[design] design/yt/concurrent.md [note]"
	first := articleReq()
	first.ParentFamily = "design"
	op1 := prepareWith(t, e, first)
	secondReq := articleReqFor(4798, second)
	secondReq.ParentFamily = "design"
	op2 := prepareWith(t, e, secondReq)

	entered := make(chan struct{})
	release := make(chan struct{})
	var held atomic.Bool
	e.yt.BeforeArticleCreate = func(_ context.Context, in ytwrite.ArticleCreate) {
		if in.Summary != "design" || in.ParentID != "" || !held.CompareAndSwap(false, true) {
			return
		}
		close(entered)
		<-release
	}
	other, err := ytwrite.New(ytwrite.Config{Ledger: e.ledger, Remote: e.yt, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	drive := func(p *ytwrite.Protocol) error {
		_, err := p.Drive(context.Background(), 10)
		return err
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- drive(e.p) }()
	<-entered // the first document holds the lane inside its root create
	if err := drive(other); err != nil {
		t.Fatal(err)
	}
	// The second document could not take the lane, so it sent nothing and is
	// due after the reconcile backoff.
	deferred := e.want(op2.ID, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	e.want(op1.ID, ytwrite.StateApplied, "")
	if roots := familyRootsOf(t, e, "design"); len(roots) != 1 {
		t.Fatalf("%d family roots after two concurrent documents, want 1: %+v", len(roots), roots)
	}
	e.clock.Advance(deferred.NextAttemptAt.Sub(e.clock.Now()))
	if err := drive(other); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{op1.ID, op2.ID} {
		e.want(id, ytwrite.StateApplied, "")
	}
	roots := familyRootsOf(t, e, "design")
	if len(roots) != 1 {
		t.Fatalf("%d family roots after the second pass, want 1: %+v", len(roots), roots)
	}
	for _, summary := range []string{artSummary, second} {
		if doc := docArticleWithSummary(t, e, summary); doc.ParentID != roots[0].ID {
			t.Fatalf("%q under %q, want the one family root %q", summary, doc.ParentID, roots[0].ID)
		}
	}
	if writes := e.yt.ArticleWrites(); len(writes) != 3 {
		t.Fatalf("%d article writes, want one root and two documents: %+v", len(writes), writes)
	}
}

// rootTestBudget is the dispatch budget the late-reply test sets, small enough
// that its slow root POST outlasts it without a slow test.
const rootTestBudget = 20 * time.Millisecond

// slowRootRemote answers the root POST only after the caller's deadline: it
// creates the root, then reports the deadline, as a server that committed but
// answered too late does.
type slowRootRemote struct {
	*ytwritetest.FakeYouTrack
	after time.Duration
}

func (r slowRootRemote) CreateArticle(ctx context.Context, in ytwrite.ArticleCreate) (string, error) {
	id, err := r.FakeYouTrack.CreateArticle(ctx, in)
	if err != nil || in.ParentID != "" || in.Summary != "design" {
		return id, err
	}
	time.Sleep(r.after)
	return "", context.DeadlineExceeded
}

// A root POST that used the whole dispatch budget and lost its reply leaves the
// root behind: the reconcile runs on its own bounded context and finds it, the
// document's search fails on the spent budget (not_sent, no duplicate root), and
// the next pass creates the document under that one root (hk 4797).
func TestArticleCreateRootTimeoutAfterBudget(t *testing.T) {
	e := newArticleEnv(t)
	remote := slowRootRemote{FakeYouTrack: e.yt, after: 4 * rootTestBudget}
	p, err := ytwrite.New(ytwrite.Config{Ledger: e.ledger, Remote: remote, Now: e.clock.Now,
		DispatchTimeout: rootTestBudget, StaleAfter: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	req := articleReq()
	req.ParentFamily = "design"
	op := prepareWith(t, e, req)
	if _, err := p.Drive(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	got := e.want(op.ID, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
	if roots := familyRootsOf(t, e, "design"); len(roots) != 1 {
		t.Fatalf("%d family roots after the late POST, want the one it made: %+v", len(roots), roots)
	}
	if _, ok, err := e.ledger.FamilyRoot(context.Background(), decTenant, artProject, "design"); err != nil || !ok {
		t.Fatalf("the root the late POST made was not recorded: ok %v err %v", ok, err)
	}
	e.clock.Advance(got.NextAttemptAt.Sub(e.clock.Now()))
	if _, err := p.Drive(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	e.want(op.ID, ytwrite.StateApplied, "")
	roots := familyRootsOf(t, e, "design")
	if len(roots) != 1 {
		t.Fatalf("%d family roots after the retry, want 1: %+v", len(roots), roots)
	}
	if doc := docArticleWithSummary(t, e, artSummary); doc.ParentID != roots[0].ID {
		t.Fatalf("document under %q, want the one family root %q", doc.ParentID, roots[0].ID)
	}
}

func TestArticleCreateLostReplyIsReconciledWithoutDuplicate(t *testing.T) {
	e := newArticleEnv(t)
	e.yt.Queue(ytwritetest.Fault{Kind: ytwritetest.DelayCommit}) // the document's POST
	op := prepareWith(t, e, articleReq())
	e.drive()
	got := e.want(op.ID, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
	if n := len(e.yt.Articles(artProject)); n != 0 {
		t.Fatalf("article visible before its commit was released: %d", n)
	}
	e.yt.ReleaseDelayed()
	e.clock.Advance(got.NextAttemptAt.Sub(e.clock.Now()))
	e.drive()
	e.want(op.ID, ytwrite.StateApplied, "")
	if arts := e.yt.Articles(artProject); len(arts) != 1 {
		t.Fatalf("%d articles, want exactly the one POST's: %+v", len(arts), arts)
	}
	if n := e.yt.Calls(); n != 1 {
		t.Fatalf("%d create calls, want the one POST: %+v", n, e.yt.ArticleWrites())
	}
}
