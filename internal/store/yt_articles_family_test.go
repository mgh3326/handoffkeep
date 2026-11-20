package store

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// One root-capable article_create per tenant, project and family until its
// root is recorded (hk 5761, design/2026-11-18/hk5085-yt-split S1, S13): the
// feed and the catch-up apply the backfill's family check and lock
// (ytArticleFamilyGateTx) before they insert a short document's create.

// ytartCaseAllOps is document id's ops of kind in every state, oldest first.
func ytartCaseAllOps(t *testing.T, s *Store, id int64, kind string) []ytwrite.Op {
	t.Helper()
	var out []ytwrite.Op
	for _, state := range []ytwrite.State{ytwrite.StatePrepared, ytwrite.StateDispatched, ytwrite.StateRemoteUnknown, ytwrite.StateApplied, ytwrite.StateRejected} {
		ops, err := s.YTWriteLedger().ListState(t.Context(), state, YTArticleDocPageMax)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range ops {
			if op.Kind == kind && op.EntityKind == ytwrite.ArticleEntity && op.EntityID == strconv.FormatInt(id, 10) {
				out = append(out, op)
			}
		}
	}
	return out
}

// Two short documents of one new family on one project get one root-capable
// create between them; the second is prepared by the catch-up once the
// family's root is recorded. A long document of the family is split at once
// (a split creates no root). A family whose root create is remote_unknown and
// unrecorded holds its next short document with no op. Run on PostgreSQL and
// MySQL.
func ytArticlesFamilyOneRootCase(t *testing.T, s *Store) {
	ctx := t.Context()
	s.SetYTDocsMode(YTDocsOn, ytartCaseTenant, ytartCaseBuckets())
	s.SetYTArticleSplit(true)
	creates := func(id int64) []ytwrite.Op {
		t.Helper()
		return ytartCaseAllOps(t, s, id, ytwrite.KindArticleCreate)
	}
	run := func() {
		t.Helper()
		ytartRescanDue(s)
		if _, err := s.RunYTFeedConsumers(ctx, 100); err != nil {
			t.Fatal(err)
		}
	}

	// The first run's full catch-up decides both short documents.
	first, _ := ytartCaseDoc(t, s, "famcase/first.md", "first")
	second, _ := ytartCaseDoc(t, s, "famcase/second.md", "second")
	long, _ := ytartCaseDoc(t, s, "famcase/long.md", ytsplitBody("a"))
	run()
	ops := creates(first)
	if len(ops) != 1 || ops[0].State != ytwrite.StatePrepared {
		t.Fatalf("first document's creates %+v, want one prepared", ops)
	}
	var p ytwrite.ArticleCreatePayload
	if err := json.Unmarshal(ops[0].Payload, &p); err != nil || p.ParentFamily != "famcase" {
		t.Fatalf("first create's family %q (%v), want famcase", p.ParentFamily, err)
	}
	if got := creates(second); len(got) != 0 {
		t.Fatalf("second document of the family got %+v while the first create's root is not recorded", got)
	}
	if got := ytartCaseAllOps(t, s, long, ytwrite.KindArticleSplit); len(got) != 1 {
		t.Fatalf("long document's splits %+v, want one: a split creates no root and is not held", got)
	}
	if got := creates(long); len(got) != 0 {
		t.Fatalf("long document made a create %+v", got)
	}
	// Still held on the next rescan: the document is waiting, not dropped.
	run()
	if got := creates(second); len(got) != 0 {
		t.Fatalf("second document on the rescan: %+v", got)
	}
	// The root is recorded (the first create's send resolved it): the rescan
	// prepares the second document's create.
	project := ops[0].YTTargetID
	if err := s.YTWriteLedger().RecordFamilyRoot(ctx, ytartCaseTenant, project, "famcase", "3-500", ops[0].ID); err != nil {
		t.Fatal(err)
	}
	run()
	if got := creates(second); len(got) != 1 || got[0].State != ytwrite.StatePrepared || got[0].YTTargetID != project {
		t.Fatalf("second document after the root was recorded: %+v", got)
	}

	// The feed: two new documents of another family in one pass.
	a, _ := ytartCaseDoc(t, s, "famunk/a.md", "a")
	b, _ := ytartCaseDoc(t, s, "famunk/b.md", "b")
	run()
	aops := creates(a)
	if len(aops) != 1 {
		t.Fatalf("a's creates %+v, want one", aops)
	}
	if got := creates(b); len(got) != 0 {
		t.Fatalf("b got %+v in the pass that prepared a's create", got)
	}
	// a's root create ends remote_unknown with no root recorded: b waits.
	l := s.YTTenantLedger(ytartCaseTenant)
	now := time.Now().UTC()
	if _, outcome, err := l.Claim(ctx, now, aops[0].ID); err != nil || outcome != ytwrite.ClaimClaimed {
		t.Fatalf("claim a's create: %v %v", outcome, err)
	}
	if _, err := l.MarkUnknown(ctx, now, aops[0].ID, now.Add(time.Hour), ytwrite.ReasonTransport, "lost"); err != nil {
		t.Fatal(err)
	}
	run()
	if got := creates(b); len(got) != 0 {
		t.Fatalf("b got %+v while a's root create is uncertain", got)
	}
}

func init() {
	registerYTAreaCase("yt_articles_family_one_root", ytArticlesFamilyOneRootCase)
}

// Two transactions on one family's lock (PostgreSQL): the catch-up of the
// second document waits for the transaction that holds the first document's
// root-capable create, then reads it and prepares nothing
// (family_root_pending). Once the root is recorded it prepares its create.
func TestYTArticleFamilyLockSerialisesCreates(t *testing.T) {
	s := ytartSetup(t, "ytart_famlock", true)
	ctx := t.Context()
	first := ytartPut(t, s, "famlock/first.md", "first", nil)
	second := ytartPut(t, s, "famlock/second.md", "second", nil)
	g, _ := s.ytDocsNow()
	now := time.Now().UTC()

	tx, release, err := s.begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer tx.Rollback(ctx)
	if err := lockYTEntity(ctx, tx, ytwrite.ArticleEntity, strconv.FormatInt(first, 10)); err != nil {
		t.Fatal(err)
	}
	r, err := readYTArticleRow(ctx, tx, first)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := prepareYTArticleTx(ctx, tx, now, g, first, r); err != nil || out != ytArticleCreatePrepared {
		t.Fatalf("first document = %q, %v; want create_prepared", out, err)
	}

	type result struct {
		out ytArticleOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := s.catchUpYTArticle(ctx, now, g, second)
		done <- result{out, err}
	}()
	// The second document's transaction waits on the family lock the first
	// one holds (an advisory lock not granted), and does not finish before it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case res := <-done:
			t.Fatalf("the second document's catch-up finished (%q, %v) while the first create was uncommitted", res.out, res.err)
		default:
		}
		var waiting bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the second document's catch-up never waited on the family lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	res := <-done
	if res.err != nil || res.out != ytArticleFamilyPending {
		t.Fatalf("second document = %q, %v; want family_root_pending", res.out, res.err)
	}
	if got := ytartOps(t, s, second); len(got) != 0 {
		t.Fatalf("second document prepared %+v", got)
	}
	ops := ytartOps(t, s, first)
	if len(ops) != 1 || ops[0].Kind != ytwrite.KindArticleCreate {
		t.Fatalf("first document's ops %+v", ops)
	}

	if err := s.YTWriteLedger().RecordFamilyRoot(ctx, ytcTenant, ops[0].YTTargetID, "famlock", "3-600", ops[0].ID); err != nil {
		t.Fatal(err)
	}
	if out, err := s.catchUpYTArticle(ctx, now, g, second); err != nil || out != ytArticleCreatePrepared {
		t.Fatalf("second document after the root was recorded = %q, %v; want create_prepared", out, err)
	}
}

// The lock and the check are per family: a document of another family on the
// same project is prepared beside an unresolved root-capable create.
func TestYTArticleFamilyGateIsPerFamily(t *testing.T) {
	s := ytartSetup(t, "ytart_famskip", true)
	ctx := t.Context()
	first := ytartPut(t, s, "famskip/first.md", "first", nil)
	other := ytartPut(t, s, "famother/doc.md", "other", nil)
	g, _ := s.ytDocsNow()
	now := time.Now().UTC()
	for _, id := range []int64{first, other} {
		if out, err := s.catchUpYTArticle(ctx, now, g, id); err != nil || out != ytArticleCreatePrepared {
			t.Fatalf("document %d = %q, %v; want create_prepared", id, out, err)
		}
	}
}
