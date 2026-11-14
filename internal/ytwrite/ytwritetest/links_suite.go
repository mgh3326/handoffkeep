package ytwritetest

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// RunLinksProtocol is the issue_links contract (hk 4638 S3; design
// 2026-11-13/yt-issue-links §4, §9 S3) over a Ledger that is also a
// ytwrite.LinksLedger: the planned send and recovery, ownership, unowned
// links, the recovery bound and the mutation order. The in-memory ledger runs
// it in ytwrite's tests; the Postgres ledger in internal/store's (CI).
func RunLinksProtocol(t *testing.T, nl NewLedger) {
	t.Run("applied_end_to_end_adds_and_owns", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		op := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{
			Parent:     &ytwrite.IssueLinkTarget{TaskID: 30, IssueID: "2-30"},
			Related:    []ytwrite.IssueLinkTarget{{TaskID: 20, IssueID: "2-20"}},
			Duplicates: []ytwrite.IssueLinkTarget{{TaskID: 21, IssueID: "2-21"}},
		}, nil)
		e.drive()
		got := e.wantState(op, ytwrite.StateApplied, "")
		if got.YTResultID != lIssue {
			t.Fatalf("yt_result_id = %q", got.YTResultID)
		}
		e.wantLinks(lIssue, sub("2-30"), rel("2-20"), dup("2-21"))
		rec := e.records(lIssue)
		if len(rec.Owned) != 3 || len(rec.Unowned) != 0 {
			t.Fatalf("records = %+v", rec)
		}
		for _, o := range rec.Owned {
			if o.IntendedOp != op.ID || o.WriterTask != lWriter || o.From != lIssue || o.ReleasedOp != 0 || !o.Open() || o.IntendedAt.IsZero() {
				t.Fatalf("owned row = %+v", o)
			}
		}
		plan := e.latestPlan(op)
		if plan.Attempt != 1 || plan.Recover != 0 || len(plan.Plan.Add) != 3 || len(plan.Plan.Current) != 0 {
			t.Fatalf("plan = %+v", plan)
		}
		// The same desired set again plans nothing and sends nothing.
		before := len(e.mutations())
		op2 := e.prepareKey("again", lWriter, lIssue, ytwrite.IssueLinksDesired{
			Parent:     &ytwrite.IssueLinkTarget{TaskID: 30, IssueID: "2-30"},
			Related:    []ytwrite.IssueLinkTarget{{TaskID: 20, IssueID: "2-20"}},
			Duplicates: []ytwrite.IssueLinkTarget{{TaskID: 21, IssueID: "2-21"}},
		}, nil)
		e.drive()
		e.wantState(op2, ytwrite.StateApplied, "")
		if n := len(e.mutations()); n != before {
			t.Fatalf("mutations after an unchanged set: %d, want %d", n, before)
		}
	})

	// I7: a person-made link on a managed type is never removed, never holds
	// the op, and is recorded once.
	t.Run("person_made_link_survives_and_is_recorded_once", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.SeedIssueLink(lIssue, rel("2-99"))
		op1 := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(op1, ytwrite.StateApplied, "")
		op2 := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{}, nil)
		e.drive()
		e.wantState(op2, ytwrite.StateApplied, "")
		e.wantLinks(lIssue, rel("2-99"))
		rec := e.records(lIssue)
		if len(rec.Unowned) != 1 {
			t.Fatalf("unowned rows = %+v", rec.Unowned)
		}
		u := rec.Unowned[0]
		if u.LinkType != ytwrite.LinkRelates || u.OtherIssue != "2-99" || u.FirstSeenOp != op1.ID || u.LastSeenOp != op2.ID || !u.GoneAt.IsZero() {
			t.Fatalf("unowned row = %+v", u)
		}
		for _, c := range e.mutations() {
			if c.Link.Target == "2-99" {
				t.Fatalf("the person-made link was mutated: %+v", c)
			}
		}
		// A person removes it: the next read closes the row.
		if err := e.yt.RemoveIssueLink(context.Background(), lIssue, e.types(), rel("2-99")); err != nil {
			t.Fatal(err)
		}
		op3 := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(op3, ytwrite.StateApplied, "")
		if rec := e.records(lIssue); len(rec.Unowned) != 1 || rec.Unowned[0].GoneAt.IsZero() {
			t.Fatalf("unowned rows after the person removed it = %+v", rec.Unowned)
		}
	})

	// I1, I2: a link owned by lane A is not removed by lane B, even when B's
	// desired set is empty.
	t.Run("lane_b_never_removes_lane_a_link", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		a := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(a, ytwrite.StateApplied, "")
		b := e.prepare(20, "2-20", ytwrite.IssueLinksDesired{}, nil)
		e.drive()
		e.wantState(b, ytwrite.StateApplied, "")
		e.wantLinks("2-20", rel(lIssue))
		if p := e.latestPlan(b).Plan; len(p.Remove) != 0 || !slices.Equal(p.Peer, []ytwrite.IssueLink{rel(lIssue)}) || len(p.Unowned) != 0 {
			t.Fatalf("lane B plan = %+v", p)
		}
		if rec := e.records("2-20"); len(rec.Unowned) != 0 {
			t.Fatalf("lane B recorded A's link as unowned: %+v", rec.Unowned)
		}
	})

	// I5: an add whose reply was lost is found by recovery's read; nothing is
	// sent again, the link stays owned, and a later op can remove it.
	t.Run("lost_reply_recovery_sends_no_second_add", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpAdd, Landed: true, Count: 1})
		op := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonTransport)
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		if adds := e.count(LinkOpAdd); adds != 1 {
			t.Fatalf("adds = %d, want 1", adds)
		}
		e.wantOwnedOpen(lIssue, rel("2-20"))
		e.removeAll(lWriter, lIssue)
	})

	// I6 with the outcome lost (review of PR #425): ownership is committed
	// before the add, and the add is marked in flight before it is sent. A
	// crash between the add and its settlement leaves no outcome: recovery
	// cannot tell an add that landed from a refused one followed by a person
	// making the same link, so it holds the op for an operator and sends
	// nothing. The operator's "applied" keeps the claim, and a later op
	// removes the link.
	t.Run("crash_between_add_and_settle_holds_for_an_operator", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		crash := &CrashLedger{Ledger: e.ledger, Method: "SettleLinks"}
		e.restart(crash)
		op := e.prepare(lWriter, lIssue, related(20), nil)
		_, _ = e.p.Drive(context.Background(), 100)
		if !crash.Dead() {
			t.Fatal("the process did not crash")
		}
		e.restart(e.ledger)
		e.clock.Advance(2 * time.Minute) // past StaleAfter
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonOutcomeLost)
		calls := len(e.yt.LinkCalls())
		e.clock.Advance(48 * time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonOutcomeLost)
		if n := len(e.yt.LinkCalls()); n != calls || e.count(LinkOpAdd) != 1 {
			t.Fatalf("a held op made %d requests (%d adds)", n-calls, e.count(LinkOpAdd))
		}
		e.wantOwnedOpen(lIssue, rel("2-20"))
		e.resolve(op, true)
		e.wantState(op, ytwrite.StateApplied, "")
		e.wantOwnedOpen(lIssue, rel("2-20"))
		e.removeAll(lWriter, lIssue)
	})

	// A crash after the plan committed and before any request: recovery
	// plans again from its read and adds once.
	t.Run("crash_after_plan_recovery_adds_once", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		crash := &CrashLedger{Ledger: e.ledger, Method: "PlanLinks", After: true}
		e.restart(crash)
		op := e.prepare(lWriter, lIssue, related(20), nil)
		_, _ = e.p.Drive(context.Background(), 100)
		e.restart(e.ledger)
		e.clock.Advance(2 * time.Minute)
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		if adds := e.count(LinkOpAdd); adds != 1 {
			t.Fatalf("adds = %d, want 1", adds)
		}
		if p := e.latestPlan(op); p.Recover != 1 {
			t.Fatalf("latest plan = %+v, want recovery plan 1", p)
		}
		// The send plan's add was never marked, so recovery's plan closed
		// its row first and owns the link through its own.
		e.wantOwnedOpen(lIssue, rel("2-20"))
		if rec := e.records(lIssue); len(rec.Owned) != 2 || rec.Owned[0].Open() {
			t.Fatalf("owned rows = %+v, want the unsent plan's row closed and recovery's open", rec.Owned)
		}
	})

	// A 429 on the first add was not processed: the op waits as prepared
	// and its ownership of the add is closed (the add was definitely not
	// made); the next attempt plans again and owns the link anew.
	t.Run("rate_limited_add_releases_then_owns_again", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpAdd, Status: http.StatusTooManyRequests, RetryAfter: 10 * time.Second, Count: 1})
		op := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(op, ytwrite.StatePrepared, ytwrite.ReasonRateLimited)
		if rec := e.records(lIssue); len(rec.Owned) != 1 || rec.Owned[0].Open() || rec.Owned[0].ReleasedOp != op.ID {
			t.Fatalf("owned rows after the 429 = %+v", rec.Owned)
		}
		e.clock.Advance(11 * time.Second)
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		e.wantOwnedOpen(lIssue, rel("2-20"))
		if p := e.latestPlan(op); p.Attempt != 2 || p.Recover != 0 {
			t.Fatalf("latest plan = %+v", p)
		}
	})

	// Review of PR #425: an add refused with a 4xx was never made, so its
	// ownership is closed. A person who later makes the same link keeps it
	// when a later op wants nothing. Ownership of the adds after the
	// failing step, never sent, is closed too.
	t.Run("refused_add_leaves_no_ownership", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpAdd, Status: http.StatusBadRequest, Count: 1})
		op := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{Related: []ytwrite.IssueLinkTarget{{TaskID: 20, IssueID: "2-20"}, {TaskID: 21, IssueID: "2-21"}}}, nil)
		e.drive()
		e.wantState(op, ytwrite.StateRejected, "http_400")
		for _, o := range e.records(lIssue).Owned {
			if o.Open() {
				t.Fatalf("open row after a refused add: %+v", o)
			}
		}
		e.yt.SeedIssueLink(lIssue, rel("2-20"))
		e.yt.SeedIssueLink(lIssue, rel("2-21"))
		next := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{}, nil)
		e.drive()
		e.wantState(next, ytwrite.StateApplied, "")
		e.wantLinks(lIssue, rel("2-20"), rel("2-21"))
	})

	// An add whose answer was ambiguous (5xx) may have landed: its ownership
	// stays, and only the adds after it, never sent, are closed.
	t.Run("ambiguous_add_keeps_ownership_unsent_adds_do_not", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpAdd, Status: http.StatusBadGateway, Landed: true, Count: 1})
		op := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{Related: []ytwrite.IssueLinkTarget{{TaskID: 20, IssueID: "2-20"}, {TaskID: 21, IssueID: "2-21"}}}, nil)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonServerError)
		open := map[string]bool{}
		for _, o := range e.records(lIssue).Owned {
			open[o.To] = o.Open()
		}
		if !open["2-20"] || open["2-21"] {
			t.Fatalf("open rows = %v, want 2-20 open (ambiguous) and 2-21 closed (unsent)", open)
		}
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		e.wantLinks(lIssue, rel("2-20"), rel("2-21"))
		e.wantOwnedOpen(lIssue, rel("2-20"))
		e.wantOwnedOpen(lIssue, rel("2-21"))
		e.removeAll(lWriter, lIssue)
	})

	// I8, send path: a 4xx on the first mutation is final.
	t.Run("first_mutation_4xx_rejects", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpAdd, Status: http.StatusBadRequest, Count: 1})
		op := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(op, ytwrite.StateRejected, "http_400")
	})

	// A read before anything is sent that fails is not sent.
	t.Run("failed_first_read_is_not_sent", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpRead, Status: http.StatusBadGateway, Count: 1})
		op := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(op, ytwrite.StatePrepared, ytwrite.ReasonNotSent)
		if _, found := e.plan(op); found {
			t.Fatal("a plan was recorded without a read")
		}
	})

	// Partial write: the add lands, the remove answers 503; the op is
	// remote_unknown and recovery's own plan finishes it.
	t.Run("partial_write_recovers_by_a_new_plan", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.applyRelated(20)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpRemove, Status: http.StatusServiceUnavailable, Count: 1})
		op := e.prepare(lWriter, lIssue, related(21), nil)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonPartialWrite)
		e.wantLinks(lIssue, rel("2-20"), rel("2-21"))
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		e.wantLinks(lIssue, rel("2-21"))
		if p := e.latestPlan(op); p.Recover != 1 || !slices.Equal(p.Plan.Remove, []ytwrite.IssueLink{rel("2-20")}) || len(p.Plan.Add) != 0 {
			t.Fatalf("recovery plan = %+v", p)
		}
		if adds := e.count(LinkOpAdd); adds != 2 {
			t.Fatalf("adds = %d, want 2 (one per link, none resent)", adds)
		}
		rec := e.records(lIssue)
		for _, o := range rec.Owned {
			if o.To == "2-20" && (o.Open() || o.ReleasedOp != op.ID) {
				t.Fatalf("released row = %+v", o)
			}
		}
	})

	// I8, recovery path: a 4xx on a recovery request is terminal and the
	// lane's next op runs.
	t.Run("recovery_4xx_rejects_and_the_lane_moves_on", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.applyRelated(20)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpRemove, Status: http.StatusServiceUnavailable, Count: 1},
			LinkCallFault{Op: LinkOpRemove, Status: http.StatusBadRequest, Count: 1})
		op := e.prepare(lWriter, lIssue, related(21), nil)
		next := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{}, nil)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonPartialWrite)
		e.wantState(next, ytwrite.StatePrepared, "")
		e.clock.Advance(time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateRejected, "http_400")
		e.drive()
		e.wantState(next, ytwrite.StateApplied, "")
		e.wantLinks(lIssue)
	})

	// I9: at most LinkRecoverMax recovery plans; the next reconcile makes no
	// request at all.
	t.Run("sixth_reconcile_makes_no_request", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.applyRelated(20)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpRemove, Status: http.StatusServiceUnavailable, Count: 1 + ytwrite.LinkRecoverMax})
		op := e.prepare(lWriter, lIssue, related(21), nil)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonPartialWrite)
		for i := 1; i <= ytwrite.LinkRecoverMax; i++ {
			e.clock.Advance(48 * time.Hour)
			e.drive()
			e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonReconcileFailed)
			if p := e.latestPlan(op); p.Recover != i {
				t.Fatalf("after reconcile %d: latest plan %+v", i, p)
			}
		}
		calls := len(e.yt.LinkCalls())
		e.clock.Advance(48 * time.Hour)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonRecoverExhausted)
		if n := len(e.yt.LinkCalls()); n != calls {
			t.Fatalf("the sixth reconcile made %d requests", n-calls)
		}
		if p := e.latestPlan(op); p.Recover != ytwrite.LinkRecoverMax {
			t.Fatalf("latest plan = %+v", p)
		}
	})

	// Review of PR #425: one recovery at a time per op. Worker B has listed
	// the op as due; worker A takes the recovery lease before B reconciles
	// it. B then makes no request and changes nothing. A stale reconcile
	// count or a second lease loses the compare-and-set.
	t.Run("recovery_is_leased_to_one_worker", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.applyRelated(20)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpRemove, Status: http.StatusServiceUnavailable, Count: 1})
		op := e.prepare(lWriter, lIssue, related(21), nil)
		e.drive()
		held := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonPartialWrite)
		e.clock.Advance(time.Hour)
		ctx := context.Background()
		leasedByA := false
		e.restart(&dueHook{CrashLedger: &CrashLedger{Ledger: e.ledger}, after: func(due []ytwrite.Op) {
			for _, d := range due {
				if d.ID != op.ID || leasedByA {
					continue
				}
				now := e.clock.Now()
				if _, ok, err := e.ll.LeaseLinkRecovery(ctx, now, op.ID, held.Reconciles+1, now.Add(time.Minute)); err != nil || ok {
					t.Errorf("lease with a stale reconcile count = %v %v", ok, err)
				}
				if epoch, ok, err := e.ll.LeaseLinkRecovery(ctx, now, op.ID, held.Reconciles, now.Add(time.Minute)); err != nil || !ok || epoch != held.Reconciles+1 {
					t.Errorf("worker A's lease = %d %v %v, want epoch %d", epoch, ok, err, held.Reconciles+1)
				}
				if _, ok, err := e.ll.LeaseLinkRecovery(ctx, now, op.ID, held.Reconciles, now.Add(time.Minute)); err != nil || ok {
					t.Errorf("a second lease = %v %v", ok, err)
				}
				leasedByA = true
			}
		}})
		calls := len(e.yt.LinkCalls())
		if _, err := e.p.Drive(ctx, 100); err != nil {
			t.Fatal(err)
		}
		if !leasedByA {
			t.Fatal("worker B never listed the op")
		}
		got := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonLinkRecovery)
		if n := len(e.yt.LinkCalls()); n != calls || got.Reconciles != held.Reconciles+1 || e.latestPlan(op).Recover != 0 {
			t.Fatalf("worker B acted without the lease: %d requests, reconciles %d, plan %+v", n-calls, got.Reconciles, e.latestPlan(op))
		}
		e.restart(e.ledger)
		e.clock.Advance(2 * time.Minute) // worker A's lease ends
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		e.wantLinks(lIssue, rel("2-21"))
	})

	// Review of PR #425: the lease comes before every other ledger step of
	// a recovery pass. Worker B lost the lease and its plan lookup fails
	// (transient); B must not touch the op, which would shorten A's lease
	// and move its reconcile count.
	t.Run("worker_without_the_lease_touches_nothing", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.applyRelated(20)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpRemove, Status: http.StatusServiceUnavailable, Count: 1})
		op := e.prepare(lWriter, lIssue, related(21), nil)
		e.drive()
		held := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonPartialWrite)
		e.clock.Advance(time.Hour)
		ctx := context.Background()
		until := e.clock.Now().Add(time.Minute)
		e.restart(&dueHook{CrashLedger: &CrashLedger{Ledger: e.ledger}, failLatest: true, after: func(due []ytwrite.Op) {
			for _, d := range due {
				if d.ID == op.ID {
					if _, ok, err := e.ll.LeaseLinkRecovery(ctx, e.clock.Now(), op.ID, held.Reconciles, until); err != nil || !ok {
						t.Errorf("worker A's lease = %v %v", ok, err)
					}
				}
			}
		}})
		_, _ = e.p.Drive(ctx, 100)
		got := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonLinkRecovery)
		if got.Reconciles != held.Reconciles+1 || !got.NextAttemptAt.Equal(until) {
			t.Fatalf("worker B touched the op: reconciles %d -> %d, next attempt %s (A's lease until %s)",
				held.Reconciles, got.Reconciles, got.NextAttemptAt, until)
		}
	})

	// Review of PR #425 (r4574642169): the add is refused with a 400 and
	// the settlement that would record it fails, then the process stops. A
	// person makes the same link before recovery. Recovery must not take
	// the read for the add having landed: the outcome is lost, so it holds
	// the op, sends nothing, and the lane's next op (which wants nothing)
	// waits. The operator's "not done" closes the claim; the next op then
	// leaves the person's link alone.
	t.Run("lost_refusal_never_claims_a_person_made_link", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpAdd, Status: http.StatusBadRequest, Count: 1})
		e.restart(&dueHook{CrashLedger: &CrashLedger{Ledger: e.ledger}, failSettle: 1})
		op := e.prepare(lWriter, lIssue, related(20), nil)
		_, _ = e.p.Drive(context.Background(), 100)
		e.wantState(op, ytwrite.StateDispatched, "")
		e.wantOwnedOpen(lIssue, rel("2-20"))
		e.restart(e.ledger)
		e.yt.SeedIssueLink(lIssue, rel("2-20")) // the person's link
		next := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{}, nil)
		e.clock.Advance(2 * time.Minute) // past StaleAfter
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonOutcomeLost)
		e.wantState(next, ytwrite.StatePrepared, "")
		e.wantLinks(lIssue, rel("2-20"))
		if adds, removes := e.count(LinkOpAdd), e.count(LinkOpRemove); adds != 1 || removes != 0 {
			t.Fatalf("adds %d, removes %d; want the one refused add", adds, removes)
		}
		e.resolve(op, false)
		e.wantState(op, ytwrite.StateRejected, ytwrite.ReasonOperatorNotDone)
		for _, o := range e.records(lIssue).Owned {
			if o.Open() {
				t.Fatalf("open row after the operator's not done: %+v", o)
			}
		}
		e.drive()
		e.wantState(next, ytwrite.StateApplied, "")
		e.wantLinks(lIssue, rel("2-20"))
		if e.count(LinkOpRemove) != 0 {
			t.Fatalf("the person's link was removed: %+v", e.mutations())
		}
	})

	// The same refusal with its settlement recorded: the definite outcome is
	// durable, the claim closes in the settlement's transaction, and the
	// op is rejected without an operator.
	t.Run("recorded_refusal_closes_the_claim_with_the_settlement", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpAdd, Status: http.StatusBadRequest, Count: 1})
		op := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(op, ytwrite.StateRejected, "http_400")
		if p := e.latestPlan(op); p.Marked != 1 || p.Outcome != ytwrite.StepDefinite {
			t.Fatalf("plan record = %+v, want step 0 marked with a definite outcome", p)
		}
		for _, o := range e.records(lIssue).Owned {
			if o.Open() {
				t.Fatalf("open row after a recorded refusal: %+v", o)
			}
		}
	})

	// Review of PR #425 (r4574642162): every ledger write of a superseded
	// executor changes nothing. Recovery pass A leased with epoch a; its
	// lease ran out and pass B leased with epoch b. A's plan, mark and
	// settlement are refused (ErrState), and so is a send-path write once
	// the op left its dispatch.
	t.Run("superseded_executor_writes_nothing", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.applyRelated(20)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpRemove, Status: http.StatusServiceUnavailable, Count: 1})
		op := e.prepare(lWriter, lIssue, related(21), nil)
		e.drive()
		held := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonPartialWrite)
		ctx := context.Background()
		e.clock.Advance(time.Hour)
		now := e.clock.Now()
		a, ok, err := e.ll.LeaseLinkRecovery(ctx, now, op.ID, held.Reconciles, now.Add(time.Minute))
		if err != nil || !ok {
			t.Fatalf("lease A = %v %v", ok, err)
		}
		e.clock.Advance(2 * time.Minute)
		now = e.clock.Now()
		b, ok, err := e.ll.LeaseLinkRecovery(ctx, now, op.ID, a, now.Add(time.Minute))
		if err != nil || !ok || b == a {
			t.Fatalf("lease B = %d %v %v (A had %d)", b, ok, err, a)
		}
		// B plans its recovery; A's writes then differ from valid ones only
		// in their epoch.
		if _, err := e.ll.PlanLinks(ctx, now, op.ID, ytwrite.LinkFence{Attempt: held.Attempts, Recover: 1, Epoch: b}, e.yt.IssueLinks(lIssue)); err != nil {
			t.Fatalf("B's plan: %v", err)
		}
		before := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonLinkRecovery)
		rows := e.records(lIssue)
		planBefore := e.latestPlan(op)
		if planBefore.Recover != 1 || len(ytwrite.ExecuteOrder(planBefore.Plan)) == 0 {
			t.Fatalf("B's plan = %+v", planBefore)
		}
		stale := ytwrite.LinkFence{Attempt: held.Attempts, Recover: 2, Epoch: a}
		if _, err := e.ll.PlanLinks(ctx, now, op.ID, stale, nil); !errors.Is(err, ytwrite.ErrState) {
			t.Errorf("stale plan = %v", err)
		}
		stale.Recover = 1
		if err := e.ll.MarkLinkStep(ctx, now, op.ID, stale, 0); !errors.Is(err, ytwrite.ErrState) {
			t.Errorf("stale mark = %v", err)
		}
		for _, to := range []ytwrite.State{ytwrite.StateApplied, ytwrite.StateRejected, ytwrite.StateRemoteUnknown} {
			s := ytwrite.LinkSettlement{To: to, Outcome: ytwrite.StepDefinite, Read: true, YTID: lIssue, Next: now.Add(time.Second), Reason: "stale"}
			if _, err := e.ll.SettleLinks(ctx, now, op.ID, stale, s); !errors.Is(err, ytwrite.ErrState) {
				t.Errorf("stale settle to %s = %v", to, err)
			}
		}
		// The same mark with B's epoch is accepted: only the epoch refused A.
		if err := e.ll.MarkLinkStep(ctx, now, op.ID, ytwrite.LinkFence{Attempt: held.Attempts, Recover: 1, Epoch: b}, 0); err != nil {
			t.Fatalf("B's mark: %v", err)
		}
		planBefore.Marked = 1
		send := ytwrite.LinkFence{Attempt: held.Attempts}
		if _, err := e.ll.SettleLinks(ctx, now, op.ID, send, ytwrite.LinkSettlement{To: ytwrite.StateRejected, Outcome: ytwrite.StepDefinite}); !errors.Is(err, ytwrite.ErrState) {
			t.Errorf("send-path settle of a recovered op = %v", err)
		}
		after := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonLinkRecovery)
		if after.Reconciles != before.Reconciles || !after.NextAttemptAt.Equal(before.NextAttemptAt) {
			t.Fatalf("op changed: %+v -> %+v", before, after)
		}
		if p := e.latestPlan(op); p.Recover != planBefore.Recover || p.Marked != planBefore.Marked || p.Outcome != planBefore.Outcome {
			t.Fatalf("plan record changed: %+v -> %+v", planBefore, p)
		}
		if got := e.records(lIssue); !slices.Equal(got.Owned, rows.Owned) {
			t.Fatalf("ownership changed: %+v -> %+v", rows.Owned, got.Owned)
		}
	})

	// Review of PR #425 (r4574642162): a lease call that returns after its
	// margin starts no work; the pass's lease runs out and the next pass
	// recovers the op.
	t.Run("late_lease_starts_no_work", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.applyRelated(20)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpRemove, Status: http.StatusServiceUnavailable, Count: 1})
		op := e.prepare(lWriter, lIssue, related(21), nil)
		e.drive()
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonPartialWrite)
		e.clock.Advance(time.Hour)
		e.restart(&dueHook{CrashLedger: &CrashLedger{Ledger: e.ledger}, slowLease: func() { e.clock.Advance(11 * time.Second) }})
		calls := len(e.yt.LinkCalls())
		if _, err := e.p.Drive(context.Background(), 100); err != nil {
			t.Fatal(err)
		}
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonLinkRecovery)
		if n := len(e.yt.LinkCalls()); n != calls || e.latestPlan(op).Recover != 0 {
			t.Fatalf("a late lease worked: %d requests, plan %+v", n-calls, e.latestPlan(op))
		}
		e.restart(e.ledger)
		e.clock.Advance(2 * time.Minute)
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		e.wantLinks(lIssue, rel("2-21"))
	})

	// Review of PR #425 (r4574806299, r4574806315): a mark whose
	// transaction commits while its reply is lost stops Execute before that
	// step's request. The settlement records the step as definite (never
	// sent), so its add's ownership closes; a person who then makes the
	// link keeps it. On the first step the op is deferred; on a later step
	// the earlier add stays owned.
	t.Run("mark_committed_with_its_reply_lost_records_the_step_unsent", func(t *testing.T) {
		for _, c := range []struct {
			name  string
			step  int
			state ytwrite.State
			owned []ytwrite.IssueLink
		}{
			{"first", 0, ytwrite.StatePrepared, nil},
			{"second", 1, ytwrite.StateRemoteUnknown, []ytwrite.IssueLink{rel("2-20")}},
		} {
			t.Run(c.name, func(t *testing.T) {
				e := newLinksEnv(t, nl)
				e.restart(&dueHook{CrashLedger: &CrashLedger{Ledger: e.ledger}, loseMark: c.step + 1})
				op := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{Related: []ytwrite.IssueLinkTarget{{TaskID: 20, IssueID: "2-20"}, {TaskID: 21, IssueID: "2-21"}}}, nil)
				_, _ = e.p.Drive(context.Background(), 100)
				got := e.wantState(op, c.state, map[ytwrite.State]string{ytwrite.StatePrepared: ytwrite.ReasonNotSent, ytwrite.StateRemoteUnknown: ytwrite.ReasonPartialWrite}[c.state])
				if p := e.latestPlan(op); p.Marked != c.step+1 || p.Outcome != ytwrite.StepDefinite {
					t.Fatalf("plan record = marked %d, outcome %q; want step %d definite", p.Marked, p.Outcome, c.step)
				}
				if adds := e.count(LinkOpAdd); adds != c.step {
					t.Fatalf("adds = %d, want %d", adds, c.step)
				}
				var open []ytwrite.IssueLink
				for _, o := range e.records(lIssue).Owned {
					if o.Open() {
						open = append(open, ytwrite.IssueLink{Kind: o.LinkType, Target: o.To})
					}
				}
				if !slices.Equal(open, c.owned) {
					t.Fatalf("open rows = %+v, want %+v", open, c.owned)
				}
				// A person makes the unsent link; the op finishes without
				// claiming it, and a later empty op leaves it.
				e.restart(e.ledger)
				e.yt.SeedIssueLink(lIssue, rel("2-21"))
				e.clock.Advance(48 * time.Hour)
				e.drive()
				e.wantState(got, ytwrite.StateApplied, "")
				e.drive()
				next := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{}, nil)
				e.drive()
				e.wantState(next, ytwrite.StateApplied, "")
				e.wantLinks(lIssue, rel("2-21"))
			})
		}
	})

	// Review of PR #425 (r4574806302): an operator's resolve waits while a
	// recovery pass holds the lease (it may have a request in flight), and
	// is accepted once the lease ran out.
	t.Run("operator_resolve_waits_for_the_recovery_lease", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.applyRelated(20)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpRemove, Status: http.StatusServiceUnavailable, Count: 1})
		op := e.prepare(lWriter, lIssue, related(21), nil)
		e.drive()
		held := e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonPartialWrite)
		e.clock.Advance(time.Hour)
		ctx := context.Background()
		now := e.clock.Now()
		if _, ok, err := e.ll.LeaseLinkRecovery(ctx, now, op.ID, held.Reconciles, now.Add(time.Minute)); err != nil || !ok {
			t.Fatalf("lease = %v %v", ok, err)
		}
		r := ytwrite.Resolution{Applied: false, By: "operator", Note: "looked in YouTrack"}
		if _, err := e.p.Resolve(ctx, op.ID, r); !errors.Is(err, ytwrite.ErrState) {
			t.Fatalf("resolve during the lease = %v, want ErrState", err)
		}
		e.wantState(op, ytwrite.StateRemoteUnknown, ytwrite.ReasonLinkRecovery)
		e.clock.Advance(2 * time.Minute)
		e.resolve(op, false)
		e.wantState(op, ytwrite.StateRejected, ytwrite.ReasonOperatorNotDone)
	})

	// I12: a re-parent removes the old parent before it adds the new one.
	t.Run("reparent_removes_old_parent_first", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		first := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{Parent: &ytwrite.IssueLinkTarget{TaskID: 30, IssueID: "2-30"}}, nil)
		e.drive()
		e.wantState(first, ytwrite.StateApplied, "")
		mark := len(e.mutations())
		op := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{Parent: &ytwrite.IssueLinkTarget{TaskID: 31, IssueID: "2-31"}}, nil)
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		got := e.mutations()[mark:]
		want := []LinkCall{{Op: LinkOpRemove, Issue: lIssue, Link: sub("2-30")}, {Op: LinkOpAdd, Issue: lIssue, Link: sub("2-31")}}
		if !slices.Equal(got, want) {
			t.Fatalf("mutations = %+v, want %+v", got, want)
		}
		e.wantLinks(lIssue, sub("2-31"))
	})

	// PR #387 finding 4562615944: two snapshots prepared before the first
	// applies; the second removes what the first added (no baseline).
	t.Run("second_snapshot_removes_what_the_first_added", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		first := e.prepare(lWriter, lIssue, related(20), nil)
		second := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{}, nil)
		e.drive()
		e.wantState(first, ytwrite.StateApplied, "")
		e.wantState(second, ytwrite.StateApplied, "")
		e.wantLinks(lIssue)
		if p := e.latestPlan(second).Plan; !slices.Equal(p.Remove, []ytwrite.IssueLink{rel("2-20")}) {
			t.Fatalf("second plan = %+v", p)
		}
	})

	// I14: an owned link whose edge is still live with its target unbound
	// (held) is kept.
	t.Run("held_edge_keeps_its_owned_link", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.applyRelated(20)
		op := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{}, []ytwrite.IssueLinkHeld{{Kind: ytwrite.LinkEdgeRelated, TaskID: 20}})
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		e.wantLinks(lIssue, rel("2-20"))
		if p := e.latestPlan(op).Plan; len(p.Remove) != 0 || !slices.Equal(p.Kept, []ytwrite.IssueLink{rel("2-20")}) {
			t.Fatalf("plan = %+v", p)
		}
		e.wantOwnedOpen(lIssue, rel("2-20"))
	})

	// A desired link already present with no row is left as it is: hk does
	// not claim it, so it is not removed later either (design §3, option B).
	t.Run("present_desired_link_is_not_claimed", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.SeedIssueLink(lIssue, rel("2-20"))
		op := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		if rec := e.records(lIssue); len(rec.Owned) != 0 {
			t.Fatalf("owned rows = %+v", rec.Owned)
		}
		op2 := e.prepare(lWriter, lIssue, ytwrite.IssueLinksDesired{}, nil)
		e.drive()
		e.wantState(op2, ytwrite.StateApplied, "")
		e.wantLinks(lIssue, rel("2-20"))
		if e.count(LinkOpAdd)+e.count(LinkOpRemove) != 0 {
			t.Fatalf("mutations = %+v", e.mutations())
		}
	})
}

const (
	lTenant = "yt.example"
	lWriter = int64(10)
	lIssue  = "2-10"
)

func rel(id string) ytwrite.IssueLink {
	return ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: id}
}
func sub(id string) ytwrite.IssueLink {
	return ytwrite.IssueLink{Kind: ytwrite.LinkSubtask, Target: id}
}
func dup(id string) ytwrite.IssueLink {
	return ytwrite.IssueLink{Kind: ytwrite.LinkDuplicate, Target: id}
}

// related is a desired set of one related edge to task id, bound to "2-<id>".
func related(id int64) ytwrite.IssueLinksDesired {
	return ytwrite.IssueLinksDesired{Related: []ytwrite.IssueLinkTarget{{TaskID: id, IssueID: "2-" + itoa(id)}}}
}

type linksEnv struct {
	t      *testing.T
	clock  *Clock
	yt     *FakeYouTrack
	ledger ytwrite.Ledger
	ll     ytwrite.LinksLedger
	p      *ytwrite.Protocol
	seq    int
}

func newLinksEnv(t *testing.T, nl NewLedger) *linksEnv {
	t.Helper()
	l := nl(t)
	ll, ok := l.(ytwrite.LinksLedger)
	if !ok {
		t.Fatalf("%T is not a ytwrite.LinksLedger", l)
	}
	e := &linksEnv{t: t, clock: NewClock(), yt: NewFake(), ledger: l, ll: ll}
	e.restart(l)
	return e
}

func (e *linksEnv) restart(l ytwrite.Ledger) {
	e.t.Helper()
	p, err := ytwrite.New(ytwrite.Config{Ledger: l, Remote: e.yt, Now: e.clock.Now, DispatchTimeout: 30 * time.Second, StaleAfter: time.Minute})
	if err != nil {
		e.t.Fatal(err)
	}
	e.p = p
}

func (e *linksEnv) prepare(task int64, issue string, d ytwrite.IssueLinksDesired, held []ytwrite.IssueLinkHeld) ytwrite.Op {
	e.t.Helper()
	e.seq++
	return e.prepareKey("k"+itoa(int64(e.seq)), task, issue, d, held)
}

func (e *linksEnv) prepareKey(key string, task int64, issue string, d ytwrite.IssueLinksDesired, held []ytwrite.IssueLinkHeld) ytwrite.Op {
	e.t.Helper()
	n, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: "hk:task-links", Actor: "hk", Tenant: lTenant,
		IssueID: issue, TaskID: task, Desired: d, Held: held})
	if err != nil {
		e.t.Fatal(err)
	}
	n.Key = key
	op, created, err := e.ledger.Prepare(context.Background(), e.clock.Now(), n)
	if err != nil || !created {
		e.t.Fatalf("Prepare = %+v %v %v", op, created, err)
	}
	return op
}

func (e *linksEnv) drive() {
	e.t.Helper()
	if _, err := e.p.Drive(context.Background(), 100); err != nil {
		e.t.Fatalf("Drive: %v", err)
	}
}

func (e *linksEnv) wantState(op ytwrite.Op, state ytwrite.State, reason string) ytwrite.Op {
	e.t.Helper()
	got, err := e.ledger.ByID(context.Background(), op.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if got.State != state || got.Reason != reason {
		e.t.Fatalf("op %d = %s/%q (%s), want %s/%q", op.ID, got.State, got.Reason, got.LastError, state, reason)
	}
	return got
}

func (e *linksEnv) wantLinks(issue string, want ...ytwrite.IssueLink) {
	e.t.Helper()
	if want == nil {
		want = []ytwrite.IssueLink{}
	}
	ytwrite.SortLinks(want)
	if got := e.yt.IssueLinks(issue); !slices.Equal(got, want) {
		e.t.Fatalf("links of %s = %+v, want %+v", issue, got, want)
	}
}

func (e *linksEnv) records(issue string) ytwrite.IssueLinkRecords {
	e.t.Helper()
	rec, err := e.ll.LinkRecords(context.Background(), lTenant, issue)
	if err != nil {
		e.t.Fatal(err)
	}
	return rec
}

// wantOwnedOpen checks that the writer holds one open, unreleased row for
// link from issue.
func (e *linksEnv) wantOwnedOpen(issue string, link ytwrite.IssueLink) {
	e.t.Helper()
	n := 0
	for _, o := range e.records(issue).Owned {
		if o.From == issue && o.LinkType == link.Kind && o.To == link.Target && o.Open() && o.ReleasedOp == 0 {
			n++
		}
	}
	if n != 1 {
		e.t.Fatalf("open rows for %s %+v = %d, want 1 (%+v)", issue, link, n, e.records(issue).Owned)
	}
}

func (e *linksEnv) plan(op ytwrite.Op) (ytwrite.LinkPlanRecord, bool) {
	e.t.Helper()
	rec, found, err := e.ll.LatestLinkPlan(context.Background(), op.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return rec, found
}

func (e *linksEnv) latestPlan(op ytwrite.Op) ytwrite.LinkPlanRecord {
	e.t.Helper()
	rec, found := e.plan(op)
	if !found {
		e.t.Fatalf("op %d has no plan", op.ID)
	}
	return rec
}

func (e *linksEnv) mutations() []LinkCall {
	var out []LinkCall
	for _, c := range e.yt.LinkCalls() {
		if c.Op != LinkOpRead {
			out = append(out, c)
		}
	}
	return out
}

func (e *linksEnv) count(op LinkOp) int {
	n := 0
	for _, c := range e.yt.LinkCalls() {
		if c.Op == op {
			n++
		}
	}
	return n
}

func (e *linksEnv) types() ytwrite.LinkTypes {
	e.t.Helper()
	types, err := ytwrite.ResolveLinkTypes(DefaultLinkTypes())
	if err != nil {
		e.t.Fatal(err)
	}
	return types
}

// applyRelated applies an op that wants one related link to task id.
func (e *linksEnv) applyRelated(id int64) {
	e.t.Helper()
	op := e.prepare(lWriter, lIssue, related(id), nil)
	e.drive()
	e.wantState(op, ytwrite.StateApplied, "")
}

// resolve settles op on an operator's word: applied (the in-flight add
// landed) or not done.
func (e *linksEnv) resolve(op ytwrite.Op, applied bool) {
	e.t.Helper()
	r := ytwrite.Resolution{Applied: applied, By: "operator", Note: "looked in YouTrack"}
	if applied {
		r.YTID = op.YTTargetID
	}
	if _, err := e.p.Resolve(context.Background(), op.ID, r); err != nil {
		e.t.Fatalf("Resolve: %v", err)
	}
}

// removeAll applies an op of writer that wants nothing, and checks that it
// removed every link the writer owned.
func (e *linksEnv) removeAll(writer int64, issue string) {
	e.t.Helper()
	op := e.prepare(writer, issue, ytwrite.IssueLinksDesired{}, nil)
	e.drive()
	e.wantState(op, ytwrite.StateApplied, "")
	e.wantLinks(issue)
	for _, o := range e.records(issue).Owned {
		if o.WriterTask == writer && o.From == issue && o.Open() {
			e.t.Fatalf("row still open after the removal: %+v", o)
		}
	}
}

// dueHook is a ledger whose Due runs after on the ops it lists before the
// protocol sees them (another worker acting between one worker's Due and
// its reconcile), that can fail LatestLinkPlan always and SettleLinks a
// number of times (a transient database failure), and whose
// LeaseLinkRecovery can run slowLease after it leased (a slow reply).
type dueHook struct {
	*CrashLedger
	after      func([]ytwrite.Op)
	failLatest bool
	failSettle int
	slowLease  func()
	// loseMark n > 0: the n-th MarkLinkStep commits and then answers an
	// error (its reply lost).
	loseMark int
	marks    int
}

var errHookTransient = errors.New("ytwritetest: transient ledger failure")

func (d *dueHook) Due(ctx context.Context, now time.Time, limit int) ([]ytwrite.Op, error) {
	ops, err := d.CrashLedger.Due(ctx, now, limit)
	if err == nil && d.after != nil {
		d.after(ops)
	}
	return ops, err
}

func (d *dueHook) LatestLinkPlan(ctx context.Context, id int64) (ytwrite.LinkPlanRecord, bool, error) {
	if d.failLatest {
		return ytwrite.LinkPlanRecord{}, false, errHookTransient
	}
	return d.CrashLedger.LatestLinkPlan(ctx, id)
}

func (d *dueHook) SettleLinks(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, ls ytwrite.LinkSettlement) (ytwrite.Op, error) {
	if d.failSettle > 0 {
		d.failSettle--
		return ytwrite.Op{}, errHookTransient
	}
	return d.CrashLedger.SettleLinks(ctx, now, id, f, ls)
}

func (d *dueHook) MarkLinkStep(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, step int) error {
	err := d.CrashLedger.MarkLinkStep(ctx, now, id, f, step)
	d.marks++
	if err == nil && d.marks == d.loseMark {
		return errHookTransient
	}
	return err
}

func (d *dueHook) LeaseLinkRecovery(ctx context.Context, now time.Time, id int64, reconciles int, until time.Time) (int, bool, error) {
	epoch, ok, err := d.CrashLedger.LeaseLinkRecovery(ctx, now, id, reconciles, until)
	if ok && d.slowLease != nil {
		d.slowLease()
	}
	return epoch, ok, err
}
