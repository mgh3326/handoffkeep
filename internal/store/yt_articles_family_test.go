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

// ytartQueuedCreate prepares document id's root-capable article_create
// directly, as a release before hk 5761 could queue it beside another open
// create of its family (no family check at preparation).
func ytartQueuedCreate(t *testing.T, s *Store, id int64, key string) ytwrite.Op {
	t.Helper()
	n, err := ytwrite.ArticleOp(ytwrite.ArticleRequest{Principal: YTDocsPrincipal, Actor: YTDocsPrincipal, YTProjectID: ytartShared, YTProjectShort: "SHRDOC",
		Tenant: ytcTenant, DocumentID: id, Summary: YTArticleSummary(key, "note"), Body: "body of " + key, ParentFamily: ytArticleFamily(id, key)})
	if err != nil {
		t.Fatal(err)
	}
	op, created, err := s.YTWriteLedger().Prepare(t.Context(), time.Now().UTC(), n)
	if err != nil || !created {
		t.Fatalf("prepare %d: %v %v", id, created, err)
	}
	return op
}

// Two root-capable creates of one family already queued (a previous release
// prepared both): the worker's admission dispatches only the older one while
// it is unresolved, holds the newer one through remote_unknown, and admits it
// once the family's root is recorded (hk 5761, S1).
func TestYTArticleCreateDispatchFencedByFamily(t *testing.T) {
	s := ytartSetup(t, "ytart_famfence", true)
	ctx := t.Context()
	first := ytartPut(t, s, "famfence/first.md", "first", nil)
	second := ytartPut(t, s, "famfence/second.md", "second", nil)
	plain := ytartPut(t, s, "plain", "no family", nil)
	a := ytartQueuedCreate(t, s, first, "famfence/first.md")
	b := ytartQueuedCreate(t, s, second, "famfence/second.md")
	c := ytartQueuedCreate(t, s, plain, "plain")
	l := s.YTTenantLedger(ytcTenant)
	now := time.Now().UTC()
	due := func() map[int64]bool {
		t.Helper()
		ops, err := l.Due(ctx, now.Add(time.Minute), 100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]bool{}
		for _, op := range ops {
			out[op.ID] = true
		}
		return out
	}
	if d := due(); !d[a.ID] || d[b.ID] || !d[c.ID] {
		t.Fatalf("due %v: want the older family create and the familyless one, not the newer", d)
	}
	claim := func(id int64, want ytwrite.ClaimOutcome) {
		t.Helper()
		if _, outcome, err := l.Claim(ctx, now, id); err != nil || outcome != want {
			t.Fatalf("claim %d = %v, %v; want %v", id, outcome, err, want)
		}
	}
	claim(b.ID, ytwrite.ClaimHeld)
	claim(a.ID, ytwrite.ClaimClaimed)
	claim(b.ID, ytwrite.ClaimHeld)
	if _, err := l.MarkUnknown(ctx, now, a.ID, now.Add(time.Hour), ytwrite.ReasonTransport, "lost"); err != nil {
		t.Fatal(err)
	}
	claim(b.ID, ytwrite.ClaimHeld)
	if err := s.YTWriteLedger().RecordFamilyRoot(ctx, ytcTenant, ytartShared, "famfence", "3-700", a.ID); err != nil {
		t.Fatal(err)
	}
	claim(b.ID, ytwrite.ClaimClaimed)

	// The reverse order: a newer create already in flight (a previous release
	// dispatched it while the older one was deferred) holds the older one.
	older := ytartPut(t, s, "famrev/older.md", "older", nil)
	newer := ytartPut(t, s, "famrev/newer.md", "newer", nil)
	o := ytartQueuedCreate(t, s, older, "famrev/older.md")
	n := ytartQueuedCreate(t, s, newer, "famrev/newer.md")
	now = time.Now().UTC()
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET next_attempt_at = $2 WHERE id = $1`, o.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET next_attempt_at = $2 WHERE id = $1`, n.ID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := l.Claim(ctx, now, o.ID); err != nil || outcome != ytwrite.ClaimNotDue {
		t.Fatalf("claim the deferred older create = %v, %v; want not_due", outcome, err)
	}
	// The newer one cannot be claimed past the prepared older one either: put
	// it in flight as the previous release did.
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET state = 'dispatched', dispatched_at = now() WHERE id = $1`, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE yt_write_ops SET next_attempt_at = $2 WHERE id = $1`, o.ID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	claim(o.ID, ytwrite.ClaimHeld)
	if _, err := l.MarkUnknown(ctx, now, n.ID, now.Add(time.Hour), ytwrite.ReasonTransport, "lost"); err != nil {
		t.Fatal(err)
	}
	if d := due(); d[o.ID] {
		t.Fatalf("due %v lists the older create behind a remote_unknown newer one", d)
	}
	claim(o.ID, ytwrite.ClaimHeld)
	if err := s.YTWriteLedger().RecordFamilyRoot(ctx, ytcTenant, ytartShared, "famrev", "3-701", n.ID); err != nil {
		t.Fatal(err)
	}
	claim(o.ID, ytwrite.ClaimClaimed)
}

// The read-only requeue (hk 5506) does not revive a rejected root-capable
// create beside another document's open create of the family, nor behind an
// uncertain one, in a dry run or in apply; once the root is recorded it
// requeues it (hk 5761).
func TestYTRequeueReadOnlyHoldsFamilyCreate(t *testing.T) {
	s := ytartSetup(t, "ytart_famrequeue", true)
	ctx := t.Context()
	first := ytartPut(t, s, "famrq/first.md", "first", nil)
	second := ytartPut(t, s, "famrq/second.md", "second", nil)
	g, _ := s.ytDocsNow()
	now := time.Now().UTC()
	if out, err := s.catchUpYTArticle(ctx, now, g, first); err != nil || out != ytArticleCreatePrepared {
		t.Fatalf("first = %q, %v", out, err)
	}
	a := ytartOps(t, s, first)[0]
	const readOnly = "youtrack answered 418: youtrack POST /api/articles: 418 invalid_state: The application is currently in read-only mode. Please try again later."
	if _, err := s.YTWriteLedger().Reject(ctx, now, a.ID, "http_418", readOnly); err != nil {
		t.Fatal(err)
	}
	// a is rejected, so second's create is the family's one open create.
	if out, err := s.catchUpYTArticle(ctx, now, g, second); err != nil || out != ytArticleCreatePrepared {
		t.Fatalf("second = %q, %v", out, err)
	}
	b := ytartOps(t, s, second)[0]
	requeue := func(apply bool, wantOutcome, wantReason string) {
		t.Helper()
		rep, err := s.RequeueYTReadOnlyOps(ctx, now, YTRequeueOptions{Tenant: ytcTenant, OpIDs: []int64{a.ID}, By: "operator", Apply: apply})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Ops) != 1 || rep.Ops[0].Outcome != wantOutcome || rep.Ops[0].Reason != wantReason {
			t.Fatalf("requeue apply=%v: %+v, want %s %q", apply, rep.Ops, wantOutcome, wantReason)
		}
	}
	requeue(false, YTRequeueNotEligible, string(ytArticleFamilyPending))
	requeue(true, YTRequeueNotEligible, string(ytArticleFamilyPending))
	if got, _ := s.YTWriteLedger().ByID(ctx, a.ID); got.State != ytwrite.StateRejected {
		t.Fatalf("a was revived beside b: %+v", got)
	}
	l := s.YTTenantLedger(ytcTenant)
	if _, outcome, err := l.Claim(ctx, now, b.ID); err != nil || outcome != ytwrite.ClaimClaimed {
		t.Fatalf("claim b: %v %v", outcome, err)
	}
	if _, err := l.MarkUnknown(ctx, now, b.ID, now.Add(time.Hour), ytwrite.ReasonTransport, "lost"); err != nil {
		t.Fatal(err)
	}
	requeue(true, YTRequeueNotEligible, string(ytArticleFamilyUncertain))
	if err := s.YTWriteLedger().RecordFamilyRoot(ctx, ytcTenant, b.YTTargetID, "famrq", "3-710", b.ID); err != nil {
		t.Fatal(err)
	}
	requeue(true, YTRequeueRequeued, "")
}
