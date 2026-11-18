package ytwritetest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// NewSplitLedger returns an empty ledger and the id of a document the ledger
// can bind (the store's apply hook reads the documents row).
type NewSplitLedger func(t *testing.T) (ytwrite.Ledger, int64)

// RunSplitProtocol is the article_split contract (hk 5085,
// design/2026-11-18/hk5085-yt-split S1-S9) over a ledger: the protocol,
// that ledger and the fake YouTrack. Each subtest names the invariant it
// shows and, for a regression of PR #477, the review finding it answers.
func RunSplitProtocol(t *testing.T, nl NewSplitLedger) {
	// The happy path: one index, n parts under it, each exact, applied with
	// the index id and the document mapped to it.
	t.Run("creates_index_and_parts_then_applies", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		op := e.prepare()
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
		if w := len(e.yt.ArticleWrites()); w != 4 {
			t.Fatalf("%d creates, want the index and 3 parts", w)
		}
		m, ok, err := e.ledger.MapGet(context.Background(), ytwrite.ArticleEntity, op.EntityID)
		if err != nil || !ok || m.YTID != got.YTResultID {
			t.Fatalf("map row %+v %v %v, want the index %s", m, ok, err, got.YTResultID)
		}
	})

	// S1, S2, S5: every failure after the index POST ends with exactly one
	// index, across any number of sends and recovery passes.
	for _, tc := range []struct {
		name   string
		faults []Fault
		reads  []error
		finds  []error
		// first is the state the first send leaves.
		state  ytwrite.State
		reason string
	}{
		// 4603394669, 4603444899: a part's create fails after the index;
		// 4603875797, 4603875859: a 5xx or timeout on a part is
		// remote_unknown, never not_sent. (A 5xx whose part never lands is
		// held: ambiguous_part_absent_is_held_not_created_again.)
		{name: "5xx_committed_on_part", faults: []Fault{{}, {}, {Kind: ServerError, Commit: true}}, state: ytwrite.StateRemoteUnknown, reason: ytwrite.ReasonServerError},
		// 4603906451: the part committed and its reply was lost: remote_unknown, never prepared.
		{name: "lost_reply_on_part", faults: []Fault{{}, {}, {Kind: CommitDropReply}}, state: ytwrite.StateRemoteUnknown, reason: ytwrite.ReasonTransport},
		{name: "timeout_on_index", faults: []Fault{{Kind: CommitDropReply}}, state: ytwrite.StateRemoteUnknown, reason: ytwrite.ReasonTransport},
		// 4603763489: an empty-id index is remote_unknown no_id, with no
		// lookup that could turn it into not_sent.
		{name: "empty_id_index", faults: []Fault{{Kind: NoID}}, state: ytwrite.StateRemoteUnknown, reason: ytwrite.ReasonNoID},
		{name: "empty_id_index_then_failing_lookup", faults: []Fault{{Kind: NoID}}, finds: []error{errBadGateway, errBadGateway}, state: ytwrite.StateRemoteUnknown, reason: ytwrite.ReasonNoID},
		// 4603794033: err == nil with an empty part id is never done.
		{name: "empty_id_part", faults: []Fault{{}, {Kind: NoID}}, state: ytwrite.StateRemoteUnknown, reason: ytwrite.ReasonNoID},
		// 4604032030: a dial error after the index POST is not "not sent".
		{name: "dial_error_on_part", faults: []Fault{{}, {}, {Kind: DialFail}}, state: ytwrite.StateRemoteUnknown, reason: ytwrite.ReasonTransport},
		// 4603945233: the read after the creates fails.
		{name: "final_read_fails", reads: []error{errBadGateway}, state: ytwrite.StateRemoteUnknown, reason: ytwrite.ReasonUnverified},
	} {
		t.Run("one_index_after_"+tc.name, func(t *testing.T) {
			e := newSplitEnv(t, nl, 3)
			e.yt.Queue(tc.faults...)
			e.yt.QueueFind(tc.finds...)
			e.queueReads(tc.reads...)
			op := e.prepare()
			e.drive()
			e.wantState(op, tc.state, tc.reason)
			for i := 0; i < 12; i++ {
				e.clock.Advance(time.Hour)
				e.drive()
				if n := e.indexes(op); n > 1 {
					t.Fatalf("pass %d: %d indexes", i, n)
				}
			}
			got := e.op(op)
			if e.indexes(op) != 1 {
				t.Fatalf("%d indexes, want 1 (op %s/%s)", e.indexes(op), got.State, got.Reason)
			}
			for i := 1; i <= e.parts; i++ {
				if n := e.part(op, i); n > 1 {
					t.Fatalf("part %d made %d times", i, n)
				}
			}
			if got.State != ytwrite.StateApplied {
				t.Fatalf("op %s/%q %q, want applied once the faults pass", got.State, got.Reason, got.LastError)
			}
			e.wantOneFamily(op, got.YTResultID)
		})
	}

	// S1, S6: an index create that may have landed and that no read finds is
	// never made a second time: the op stays remote_unknown index_not_found
	// and makes no request.
	t.Run("uncertain_index_not_found_is_never_created_again", func(t *testing.T) {
		e := newSplitEnv(t, nl, 2)
		e.yt.Queue(Fault{Kind: ServerError})
		op := e.prepare()
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
		for i := 0; i < 4; i++ {
			e.clock.Advance(time.Hour)
			e.drive()
		}
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonIndexNotFound)
		if w := len(e.yt.ArticleWrites()); w != 1 || e.indexes(op) != 0 {
			t.Fatalf("%d creates, %d indexes: want the one uncertain index POST", w, e.indexes(op))
		}
	})

	// S7 for an ambiguous outcome (review of PR #511, r4615478669,
	// r4615478680): a part create answered 5xx may still land, so a read
	// that does not show it is no proof it failed. Recovery creates nothing
	// beside it; when it lands, the next pass finds it and applies.
	t.Run("ambiguous_part_absent_is_held_not_created_again", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: DelayCommit})
		op := e.prepare()
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		writes := len(e.yt.ArticleWrites())
		for i := 0; i < 3; i++ {
			e.clock.Advance(time.Hour)
			e.drive()
		}
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonOutcomeAmbiguous)
		if w := len(e.yt.ArticleWrites()); w != writes {
			t.Fatalf("%d creates beside an ambiguous part", w-writes)
		}
		e.yt.ReleaseDelayed() // the first POST lands late
		e.clock.Advance(time.Hour)
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
	})

	// A split never creates a family root (review of PR #511, r4615894731,
	// r4615902725, r4615892450, r4615892462): a root POST whose outcome is
	// unknown could land after a later search and leave two roots. With no
	// root the op waits, not sent and with no request; once the family has
	// its root the family goes under it.
	t.Run("family_without_root_waits_and_creates_no_root", func(t *testing.T) {
		e := newSplitEnv(t, nl, 2)
		e.family = "note"
		op := e.prepare()
		for i := 0; i < 3; i++ {
			e.drive()
			e.clock.Advance(time.Hour)
		}
		if w := len(e.yt.ArticleWrites()); w != 0 {
			t.Fatalf("%d creates (%+v) with no family root", w, e.yt.ArticleWrites())
		}
		e.wantState(op, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
		root := e.yt.InjectArticle(splitProject, "note", ytwrite.FamilyRootContent("note"))
		e.clock.Advance(48 * time.Hour)
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
		roots := 0
		for _, a := range e.yt.Articles(splitProject) {
			if a.Summary == "note" {
				roots++
			}
		}
		if ix, _ := e.yt.Article(got.YTResultID); ix.ParentID != root || roots != 1 {
			t.Fatalf("index under %q, %d roots; want one root %s", ix.ParentID, roots, root)
		}
	})

	// An index moved to another project is not the op's target: recovery
	// holds and creates nothing under it (review of PR #511, r4615892366).
	t.Run("index_moved_to_another_project_is_held", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: DialFail})
		op := e.prepare()
		e.drive()
		got := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.yt.MoveArticle(got.YTResultID, "0-99")
		writes := len(e.yt.ArticleWrites())
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
		if w := len(e.yt.ArticleWrites()); w != writes {
			t.Fatalf("%d creates under a moved index", w-writes)
		}
	})

	// An unrecorded index renamed between the search that found it and its
	// direct read is not the op's index: recovery holds before any part
	// request (review of PR #511, r4615892376).
	t.Run("index_renamed_after_its_search_is_held", func(t *testing.T) {
		e := newSplitEnv(t, nl, 2)
		e.yt.Queue(Fault{Kind: NoID})
		op := e.prepare()
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonNoID)
		e.yt.BeforeReadArticle = func(id string) {
			e.yt.BeforeReadArticle = nil
			e.yt.SetArticleSummary(id, "renamed by a person")
		}
		writes := len(e.yt.ArticleWrites())
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
		if w := len(e.yt.ArticleWrites()); w != writes {
			t.Fatalf("%d part creates under a renamed index", w-writes)
		}
	})

	// S2 (4604032030, 4603875797, 4603875859): a dial error on part k ≥ 1 after the
	// index exists is remote_unknown, never prepared/not_sent.
	t.Run("dial_error_on_part_2_is_remote_unknown", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: DialFail})
		op := e.prepare()
		e.drive()
		got := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		if got.YTResultID == "" {
			t.Fatal("the index id is not recorded")
		}
	})

	// S2: a dial error on the op's first create sent nothing: not_sent.
	t.Run("dial_error_on_the_index_is_not_sent", func(t *testing.T) {
		e := newSplitEnv(t, nl, 2)
		e.yt.Queue(Fault{Kind: DialFail})
		op := e.prepare()
		e.drive()
		e.wantState(op, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
		e.clock.Advance(time.Hour)
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
	})

	// S3 (4603799062): two parts with the same text are two parts; part 1
	// is never taken for part 2.
	t.Run("identical_chunks_are_distinct_parts", func(t *testing.T) {
		e := newSplitEnvBody(t, nl, identicalChunksBody())
		if e.parts != 2 {
			t.Fatalf("%d parts, want 2", e.parts)
		}
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: RateLimit, RetryAfter: time.Minute})
		op := e.prepare()
		e.drive()
		e.wantState(op, ytwrite.StatePrepared, ytwrite.ReasonRateLimited)
		e.clock.Advance(time.Hour)
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
	})

	// S3 (4603696335): a part carrying its marker with other content
	// (truncated, edited) is not accepted and not replaced: the op holds for
	// an operator and sends nothing more.
	t.Run("edited_part_is_never_accepted_or_duplicated", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: ServerError})
		op := e.prepare()
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
		part1 := e.partIDs(op, 1)
		if len(part1) != 1 {
			t.Fatalf("part 1 = %v", part1)
		}
		a, _ := e.yt.Article(part1[0])
		e.yt.EditArticle(part1[0], strings.Replace(a.Content, "a", "b", 1))
		writes := len(e.yt.ArticleWrites())
		for i := 0; i < 3; i++ {
			e.clock.Advance(time.Hour)
			e.drive()
		}
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonMarkerMismatch)
		if w := len(e.yt.ArticleWrites()); w != writes {
			t.Fatalf("%d more creates beside an edited part", w-writes)
		}
	})

	// S5: applied needs the final read to show every part. A part that a
	// person deletes before that read leaves the op remote_unknown, and the
	// direct children read lets recovery make it again.
	t.Run("part_gone_before_the_final_read_is_not_applied", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		op := e.prepare()
		e.yt.BeforeArticleCreate = func(_ context.Context, in ytwrite.ArticleCreate) {
			if strings.Contains(in.Content, ytwrite.PartMarker(op.ID, 3, 3)) {
				for _, id := range e.partIDs(op, 1) {
					e.yt.DeleteArticle(id)
				}
			}
		}
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonPartialWrite)
		e.yt.BeforeArticleCreate = nil
		e.clock.Advance(time.Hour)
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
	})

	// S1 at the ledger: a plan that creates the index is refused once a plan
	// of the op marked an index create whose outcome is not definite, or
	// once an index id is recorded, whatever the protocol asks.
	t.Run("ledger_refuses_a_second_index_plan", func(t *testing.T) {
		e := newSplitEnv(t, nl, 2)
		sl := e.ledger.(ytwrite.SplitLedger)
		ctx := context.Background()
		op := e.prepare()
		cur, outcome, err := e.ledger.Claim(ctx, e.clock.Now(), op.ID)
		if err != nil || outcome != ytwrite.ClaimClaimed {
			t.Fatalf("claim %v %v", outcome, err)
		}
		f := ytwrite.LinkFence{Attempt: cur.Attempts}
		create := ytwrite.SplitPlan{Kind: ytwrite.SplitPlanTag, Parts: 2, Steps: []int{0, 1, 2}}
		if err := sl.PlanSplit(ctx, e.clock.Now(), op.ID, f, create); err != nil {
			t.Fatal(err)
		}
		if err := sl.MarkSplitStep(ctx, e.clock.Now(), op.ID, f, 0, ""); err != nil {
			t.Fatal(err)
		}
		if err := sl.MarkSplitStep(ctx, e.clock.Now(), op.ID, f, 1, ""); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("a part marked before its index was recorded: %v", err)
		}
		if _, err := sl.SettleSplit(ctx, e.clock.Now(), op.ID, f, ytwrite.LinkSettlement{To: ytwrite.StateRemoteUnknown, Outcome: ytwrite.StepAmbiguous,
			Next: e.clock.Now(), Reason: ytwrite.ReasonServerError}); err != nil {
			t.Fatal(err)
		}
		lease, ok, err := sl.LeaseLinkRecovery(ctx, e.clock.Now(), op.ID, 0, e.clock.Now().Add(time.Minute))
		if err != nil || !ok {
			t.Fatalf("lease %v %v", ok, err)
		}
		if err := sl.PlanSplit(ctx, e.clock.Now(), op.ID, ytwrite.LinkFence{Attempt: cur.Attempts, Recover: 1, Epoch: lease}, create); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("a second index plan after an ambiguous index create: %v", err)
		}
		plans, err := sl.SplitPlans(ctx, op.ID)
		if err != nil || len(plans) != 1 || plans[0].Marked != 1 || plans[0].Outcome != ytwrite.StepAmbiguous || !ytwrite.IndexMaybeSent(plans) {
			t.Fatalf("plans %+v %v", plans, err)
		}
		// A recorded index is the only one the op may name.
		f2 := ytwrite.LinkFence{Attempt: cur.Attempts, Recover: 1, Epoch: lease}
		if err := sl.PlanSplit(ctx, e.clock.Now(), op.ID, f2, ytwrite.SplitPlan{Kind: ytwrite.SplitPlanTag, Index: "3-77", Parts: 2, Steps: []int{1, 2}}); err != nil {
			t.Fatal(err)
		}
		if err := sl.MarkSplitStep(ctx, e.clock.Now(), op.ID, f2, 0, "3-77"); err != nil {
			t.Fatal(err)
		}
		if got := e.op(op); got.YTResultID != "3-77" {
			t.Fatalf("index id %q, want 3-77 recorded at the mark", got.YTResultID)
		}
		if _, err := sl.SettleSplit(ctx, e.clock.Now(), op.ID, f2, ytwrite.LinkSettlement{To: ytwrite.StateRemoteUnknown, YTID: "3-78",
			Next: e.clock.Now(), Reason: ytwrite.ReasonServerError}); !errors.Is(err, ytwrite.ErrConflictingResult) {
			t.Fatalf("another index id settled: %v", err)
		}
	})

	// S5 (4603394669, 4603444899): an index with a part missing is not
	// applied; the missing part is created under the same index.
	t.Run("index_with_parts_missing_is_not_applied", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: DialFail})
		op := e.prepare()
		e.drive()
		got := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		if got.YTResultID == "" || e.part(op, 2) != 0 {
			t.Fatalf("op %+v, part 2 made %d times", got, e.part(op, 2))
		}
		e.clock.Advance(time.Hour)
		e.drive()
		got = e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
	})

	// S6 (4604035276): a 502 on the read in a later send converges on a
	// later pass, never stuck on marker_mismatch.
	t.Run("502_on_the_family_read_converges", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: DialFail})
		op := e.prepare()
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.queueReads(errBadGateway)
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonReconcileFailed)
		e.clock.Advance(time.Hour)
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
	})

	// S6, S8 (4604150360): a 429 on a part during recovery waits and the
	// next pass completes the family.
	t.Run("429_on_a_part_in_recovery_converges", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: DialFail}, Fault{Kind: RateLimit, RetryAfter: time.Minute})
		op := e.prepare()
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonRateLimited)
		e.clock.Advance(time.Hour)
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
	})

	// S8: a 429 on part 3 defers the op at its step; the next attempt reads
	// the family and creates only part 3.
	t.Run("429_on_part_3_defers_then_applies", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{}, Fault{Kind: RateLimit, RetryAfter: time.Minute})
		op := e.prepare()
		e.drive()
		got := e.wantState(op, ytwrite.StatePrepared, ytwrite.ReasonRateLimited)
		if got.YTResultID == "" {
			t.Fatal("the deferred op lost its index id")
		}
		e.clock.Advance(2 * time.Minute)
		e.drive()
		got = e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
		if w := len(e.yt.ArticleWrites()); w != 5 {
			t.Fatalf("%d creates, want the index, 3 parts and the refused one", w)
		}
	})

	// S9 (4604032038): a 4xx on part 2 after the index exists rejects
	// partial_write with the index recorded; the document's op is never
	// prepared again, so nothing more is sent.
	t.Run("4xx_on_part_2_rejects_partial_write", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: Refuse, Status: http.StatusBadRequest})
		op := e.prepare()
		e.drive()
		got := e.wantState(op, ytwrite.StateRejected, ytwrite.ReasonPartialWrite)
		if got.YTResultID == "" || !strings.Contains(got.LastError, "part 2 of 3") || !strings.Contains(got.LastError, got.YTResultID) {
			t.Fatalf("rejected op %+v: want the index id and the failed part recorded", got)
		}
		writes := len(e.yt.ArticleWrites())
		again := e.prepare()
		if again.ID != op.ID {
			t.Fatalf("a second split op %d for the document", again.ID)
		}
		e.clock.Advance(time.Hour)
		e.drive()
		if w := len(e.yt.ArticleWrites()); w != writes || e.indexes(op) != 1 {
			t.Fatalf("%d more requests, %d indexes", w-writes, e.indexes(op))
		}
	})

	// A 4xx on the op's first create made nothing: rejected with its status.
	t.Run("4xx_on_the_index_rejects", func(t *testing.T) {
		e := newSplitEnv(t, nl, 2)
		e.yt.Queue(Fault{Kind: Refuse, Status: http.StatusBadRequest})
		op := e.prepare()
		e.drive()
		got := e.wantState(op, ytwrite.StateRejected, "http_400")
		if got.YTResultID != "" || e.indexes(op) != 0 {
			t.Fatalf("op %+v", got)
		}
	})

	// S7: a create whose outcome was never recorded (the process died
	// before its settlement), and that the direct read does not show, makes
	// no request: remote_unknown outcome_lost.
	t.Run("lost_outcome_with_part_absent_makes_no_request", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		crash := &CrashLedger{Ledger: e.ledger, Method: "SettleSplit"}
		e.restart(crash)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: Refuse, Status: http.StatusBadRequest})
		op := e.prepare()
		_, _ = e.p.Drive(context.Background(), 10)
		if !crash.Dead() {
			t.Fatal("the process did not crash")
		}
		e.restart(e.ledger)
		e.clock.Advance(2 * time.Minute) // past StaleAfter
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonOutcomeLost)
		writes := len(e.yt.ArticleWrites())
		e.clock.Advance(48 * time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonOutcomeLost)
		if w := len(e.yt.ArticleWrites()); w != writes {
			t.Fatalf("a held op made %d requests", w-writes)
		}
	})

	// S7's other side: every create landed and the process died before the
	// apply, so the last create's outcome was never recorded; the read shows
	// it, so it is not lost and recovery applies.
	t.Run("lost_outcome_with_part_present_continues", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		crash := &CrashLedger{Ledger: e.ledger, Method: "ApplySplit"}
		e.restart(crash)
		op := e.prepare()
		_, _ = e.p.Drive(context.Background(), 10)
		if !crash.Dead() {
			t.Fatal("the process did not crash")
		}
		e.restart(e.ledger)
		e.clock.Advance(2 * time.Minute)
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		e.wantOneFamily(op, got.YTResultID)
	})

	// The operator's applied is fenced in the ledger, not checked on a
	// snapshot (review of PR #511, r4615543229): a recovery pass that leased
	// the op, recorded its index and marked a create after the operator's
	// read makes the apply fail, and nothing is bound to the operator's id.
	t.Run("operator_apply_after_recovery_took_the_op_is_refused", func(t *testing.T) {
		e := newSplitEnv(t, nl, 2)
		e.yt.Queue(Fault{Kind: ServerError})
		op := e.prepare()
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
		e.clock.Advance(time.Hour)
		cur := e.op(op)
		sl := e.ledger.(ytwrite.SplitLedger)
		race := &racingLedger{splitLedger: e.ledger.(splitLedger), afterByID: func() {
			ctx := context.Background()
			epoch, ok, err := sl.LeaseLinkRecovery(ctx, e.clock.Now(), op.ID, cur.Reconciles, e.clock.Now().Add(time.Minute))
			if err != nil || !ok {
				t.Fatalf("lease %v %v", ok, err)
			}
			f := ytwrite.LinkFence{Attempt: cur.Attempts, Recover: 1, Epoch: epoch}
			if err := sl.PlanSplit(ctx, e.clock.Now(), op.ID, f, ytwrite.SplitPlan{Kind: ytwrite.SplitPlanTag, Index: "3-77", Parts: 2, Steps: []int{1, 2}}); err != nil {
				t.Fatal(err)
			}
			if err := sl.MarkSplitStep(ctx, e.clock.Now(), op.ID, f, 0, "3-77"); err != nil {
				t.Fatal(err)
			}
		}}
		e.restart(race)
		if _, err := e.p.Resolve(context.Background(), op.ID, ytwrite.Resolution{Applied: true, YTID: "3-99", By: "operator", Note: "looked"}); err == nil {
			t.Fatal("the operator's apply went through while a recovery pass held the op")
		}
		got := e.op(op)
		if got.State == ytwrite.StateApplied || got.YTResultID != "3-77" {
			t.Fatalf("op %s with index %q", got.State, got.YTResultID)
		}
		if m, ok, _ := e.ledger.MapGet(context.Background(), ytwrite.ArticleEntity, op.EntityID); ok {
			t.Fatalf("document mapped to %s", m.YTID)
		}
	})

	// The operator's decision is anchored at its first read (review of PR
	// #511, r4615618102): a whole recovery pass that leases the op, marks a
	// part create, gets an ambiguous answer and settles back to
	// remote_unknown between that read and the decision leaves the same
	// recorded index and a plan record the decision could match, but the
	// part may still land, so the apply is refused and nothing is bound.
	t.Run("operator_apply_after_a_finished_recovery_pass_is_refused", func(t *testing.T) {
		e := newSplitEnv(t, nl, 3)
		e.yt.Queue(Fault{}, Fault{}, Fault{Kind: DialFail})
		op := e.prepare()
		e.drive()
		before := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.clock.Advance(time.Hour)
		race := &racingLedger{splitLedger: e.ledger.(splitLedger), afterByID: func() {
			e.yt.Queue(Fault{Kind: DelayCommit})
			e.drive() // one recovery pass: part 2 answered ambiguously
		}}
		e.restart(race)
		_, err := e.p.Resolve(context.Background(), op.ID, ytwrite.Resolution{Applied: true, YTID: before.YTResultID, By: "operator", Note: "looked"})
		if !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("the operator's apply after a recovery pass it did not see: %v", err)
		}
		got := e.op(op)
		if got.State != ytwrite.StateRemoteUnknown || got.YTResultID != before.YTResultID {
			t.Fatalf("op %s/%q index %q", got.State, got.Reason, got.YTResultID)
		}
		if m, ok, _ := e.ledger.MapGet(context.Background(), ytwrite.ArticleEntity, op.EntityID); ok {
			t.Fatalf("document mapped to %s", m.YTID)
		}
		// A decision made from a current read applies.
		e.yt.ReleaseDelayed()
		if _, err := e.p.Resolve(context.Background(), op.ID, ytwrite.Resolution{Applied: true, YTID: before.YTResultID, By: "operator", Note: "looked again"}); err != nil {
			t.Fatal(err)
		}
		e.wantState(op, ytwrite.StateApplied, "")
	})

	// The operator's word on a held op: not applied rejects it and leaves
	// the family; nothing is sent.
	t.Run("operator_not_applied_rejects", func(t *testing.T) {
		e := newSplitEnv(t, nl, 2)
		e.yt.Queue(Fault{Kind: ServerError})
		op := e.prepare()
		e.drive()
		for i := 0; i < 2; i++ {
			e.clock.Advance(time.Hour)
			e.drive()
		}
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonIndexNotFound)
		if _, err := e.p.Resolve(context.Background(), op.ID, ytwrite.Resolution{By: "operator", Note: "checked"}); err != nil {
			t.Fatal(err)
		}
		e.wantState(op, ytwrite.StateRejected, ytwrite.ReasonOperatorNotDone)
	})
}

var errBadGateway = &ytwrite.RemoteError{Status: http.StatusBadGateway, Message: "bad gateway"}

type splitEnv struct {
	t      *testing.T
	clock  *Clock
	yt     *FakeYouTrack
	ledger ytwrite.Ledger
	p      *ytwrite.Protocol
	doc    int64
	body   string
	parts  int
	// family is the document's family (its parent root), "" for none.
	family string
}

const (
	splitProject = "0-30"
	splitSummary = "[design] design/yt/long.md [note]"
	splitActor   = "hk:documents"
)

// splitBody is a body of about parts × SplitChunkMaxBytes of numbered lines.
func splitBody(parts int) string {
	var b strings.Builder
	for i := 0; b.Len() < (parts-1)*ytwrite.SplitChunkMaxBytes+ytwrite.SplitChunkMaxBytes/2; i++ {
		b.WriteString("line " + itoa(int64(i)) + " " + strings.Repeat("a", 200) + "\n")
	}
	return b.String()
}

// identicalChunksBody is a body whose two parts have the same text.
func identicalChunksBody() string {
	x := strings.Repeat("a", ytwrite.SplitChunkMaxBytes-1) + "\n"
	return x + x
}

func newSplitEnv(t *testing.T, nl NewSplitLedger, parts int) *splitEnv {
	return newSplitEnvBody(t, nl, splitBody(parts))
}

func newSplitEnvBody(t *testing.T, nl NewSplitLedger, body string) *splitEnv {
	t.Helper()
	l, doc := nl(t)
	if _, ok := l.(ytwrite.SplitLedger); !ok {
		t.Fatalf("%T is not a ytwrite.SplitLedger", l)
	}
	e := &splitEnv{t: t, clock: NewClock(), yt: NewFake(), ledger: l, doc: doc, body: body, parts: len(ytwrite.SplitArticleBody(body))}
	e.restart(l)
	return e
}

func (e *splitEnv) restart(l ytwrite.Ledger) {
	e.t.Helper()
	p, err := ytwrite.New(ytwrite.Config{Ledger: l, Remote: e.yt, Now: e.clock.Now, DispatchTimeout: 30 * time.Second, StaleAfter: time.Minute})
	if err != nil {
		e.t.Fatal(err)
	}
	e.p = p
}

func (e *splitEnv) prepare() ytwrite.Op {
	e.t.Helper()
	n, err := ytwrite.ArticleSplitOp(ytwrite.ArticleRequest{Principal: splitActor, Actor: splitActor, YTProjectID: splitProject, YTProjectShort: "SHRDOC",
		Tenant: "tenant.youtrack.cloud", DocumentID: e.doc, Summary: splitSummary, Body: e.body, ParentFamily: e.family})
	if err != nil {
		e.t.Fatal(err)
	}
	op, _, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil {
		e.t.Fatal(err)
	}
	return op
}

func (e *splitEnv) drive() {
	e.t.Helper()
	if _, err := e.p.Drive(context.Background(), 10); err != nil && !errors.Is(err, ytwrite.ErrState) {
		e.t.Fatal(err)
	}
}

// queueReads queues ReadArticle results: nil passes one read through.
func (e *splitEnv) queueReads(errs ...error) {
	for _, err := range errs {
		e.yt.QueueRead(err)
	}
}

func (e *splitEnv) op(op ytwrite.Op) ytwrite.Op {
	e.t.Helper()
	got, err := e.ledger.ByID(context.Background(), op.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}

func (e *splitEnv) wantState(op ytwrite.Op, state ytwrite.State, reason string) ytwrite.Op {
	e.t.Helper()
	got := e.op(op)
	if got.State != state || got.Reason != reason {
		e.t.Fatalf("op %d = %s/%q (%q), want %s/%q", op.ID, got.State, got.Reason, got.LastError, state, reason)
	}
	return got
}

// indexes counts the articles carrying the op's index marker.
func (e *splitEnv) indexes(op ytwrite.Op) int {
	n := 0
	for _, a := range e.yt.Articles(splitProject) {
		if strings.Contains(a.Content, ytwrite.OpMarker(op.ID)) {
			n++
		}
	}
	return n
}

func (e *splitEnv) partIDs(op ytwrite.Op, i int) []string {
	var out []string
	for _, a := range e.yt.Articles(splitProject) {
		if strings.Contains(a.Content, ytwrite.PartMarker(op.ID, i, e.parts)) {
			out = append(out, a.ID)
		}
	}
	return out
}

func (e *splitEnv) part(op ytwrite.Op, i int) int { return len(e.partIDs(op, i)) }

// wantOneFamily checks the remote holds exactly one index (index) and one
// exact article per part, each a child of the index.
func (e *splitEnv) wantOneFamily(op ytwrite.Op, index string) {
	e.t.Helper()
	got := e.op(op)
	at := got.CreatedAt.UTC().Truncate(time.Second)
	if e.indexes(op) != 1 {
		e.t.Fatalf("%d indexes", e.indexes(op))
	}
	ix, ok := e.yt.Article(index)
	if !ok || ix.Summary != splitSummary || ix.Content != ytwrite.RenderSplitIndex(splitActor, at, op.ID, splitSummary, e.parts) {
		e.t.Fatalf("index %s = %+v", index, ix)
	}
	chunks := ytwrite.SplitArticleBody(e.body)
	for i := 1; i <= e.parts; i++ {
		ids := e.partIDs(op, i)
		if len(ids) != 1 {
			e.t.Fatalf("part %d: %v", i, ids)
		}
		a, _ := e.yt.Article(ids[0])
		if a.ParentID != index || a.Summary != ytwrite.SplitPartSummary(splitSummary, i, e.parts) ||
			a.Content != ytwrite.RenderSplitPart(splitActor, at, op.ID, i, e.parts, chunks[i-1]) {
			e.t.Fatalf("part %d = %+v", i, a)
		}
	}
	if strings.Join(chunks, "") != ytwrite.EscapeArticleBody(e.body) {
		e.t.Fatal("the parts do not hold the body")
	}
}

// splitLedger is a ledger with the split side.
type splitLedger interface {
	ytwrite.Ledger
	ytwrite.SplitLedger
}

// racingLedger runs afterByID once, right after the first ByID returns: a
// writer that acts between an operator's read and its decision.
type racingLedger struct {
	splitLedger
	afterByID func()
}

func (r *racingLedger) ByID(ctx context.Context, id int64) (ytwrite.Op, error) {
	op, err := r.splitLedger.ByID(ctx, id)
	if f := r.afterByID; f != nil {
		r.afterByID = nil
		f()
	}
	return op, err
}
