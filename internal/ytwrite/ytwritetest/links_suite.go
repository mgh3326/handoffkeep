package ytwritetest

import (
	"context"
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
		e.yt.RemoveIssueLink(context.Background(), lIssue, e.types(), rel("2-99"))
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

	// I6: ownership is committed before the add, so a crash between the add
	// and its settlement leaves the claim, recovery applies without a second
	// add, and a later op removes the link.
	t.Run("crash_between_add_and_settle_keeps_ownership", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		crash := &CrashLedger{Ledger: e.ledger, Method: "ApplyLinks"}
		e.restart(crash)
		op := e.prepare(lWriter, lIssue, related(20), nil)
		_, _ = e.p.Drive(context.Background(), 100)
		if !crash.Dead() {
			t.Fatal("the process did not crash")
		}
		e.restart(e.ledger)
		e.clock.Advance(2 * time.Minute) // past StaleAfter
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		if adds := e.count(LinkOpAdd); adds != 1 {
			t.Fatalf("adds = %d, want 1", adds)
		}
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
		rec := e.records(lIssue)
		if len(rec.Owned) != 1 {
			t.Fatalf("owned rows = %+v, want the one row reused", rec.Owned)
		}
	})

	// A 429 on the first add was not processed: the op waits as prepared,
	// the next attempt plans again and reuses the ownership row.
	t.Run("rate_limited_add_replans_and_reuses_the_row", func(t *testing.T) {
		e := newLinksEnv(t, nl)
		e.yt.QueueLinkCallFaults(LinkCallFault{Op: LinkOpAdd, Status: http.StatusTooManyRequests, RetryAfter: 10 * time.Second, Count: 1})
		op := e.prepare(lWriter, lIssue, related(20), nil)
		e.drive()
		e.wantState(op, ytwrite.StatePrepared, ytwrite.ReasonRateLimited)
		e.clock.Advance(11 * time.Second)
		e.drive()
		e.wantState(op, ytwrite.StateApplied, "")
		if rec := e.records(lIssue); len(rec.Owned) != 1 || !rec.Owned[0].Open() {
			t.Fatalf("owned rows = %+v", rec.Owned)
		}
		if p := e.latestPlan(op); p.Attempt != 2 || p.Recover != 0 {
			t.Fatalf("latest plan = %+v", p)
		}
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
