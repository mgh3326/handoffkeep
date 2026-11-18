package store

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The issue-link area's dialect-neutral cases (yt_area_cases_test.go): the
// plan record of a send, the sweep clock and its status read, and the plan
// step/release record. They use only the store's public methods, so
// TestYTAreaPostgres and TestMySQLYTArea run the same cases, and they need no
// task or binding row: an issue_links op's lane and plan tables do not
// reference them.

const ytLinksCaseTenant = "links-area.youtrack.cloud"

// ytLinksCaseOp is one issue_links op of task task for its bound issue.
func ytLinksCaseOp(t *testing.T, task int64, issue string, desired ytwrite.IssueLinksDesired) ytwrite.NewOp {
	t.Helper()
	n, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{
		Principal: YTLinksPrincipal, Actor: YTLinksPrincipal, Tenant: ytLinksCaseTenant,
		IssueID: issue, TaskID: task, Desired: desired,
	})
	if err != nil {
		t.Fatalf("IssueLinksOp: %v", err)
	}
	// IssueLinksOp leaves the idempotency key to the snapshot helper, which
	// keys the op by its payload sha (prepareYTSnapshotTx).
	n.Key = n.PayloadSHA
	return n
}

// The planned send's record: the plan a dispatched op wrote reads back with
// the same attempt and diff, and the ownership row it inserted is open.
func init() {
	registerYTAreaCase("links/plan-write-read", func(t *testing.T, s *Store) {
		ctx, now := t.Context(), time.Now().UTC()
		ledger := s.YTWriteLedger()
		n := ytLinksCaseOp(t, 4101, "AREA-4102", ytwrite.IssueLinksDesired{
			Related: []ytwrite.IssueLinkTarget{{TaskID: 4102, IssueID: "AREA-4103"}},
		})
		op, created, err := ledger.Prepare(ctx, now, n)
		if err != nil || !created || op.ID < 1 {
			t.Fatalf("Prepare = %+v created=%v err=%v", op, created, err)
		}
		claimed, outcome, err := ledger.Claim(ctx, now, op.ID)
		if err != nil || outcome != ytwrite.ClaimClaimed || claimed.Attempts != 1 || claimed.State != ytwrite.StateDispatched {
			t.Fatalf("Claim = %+v outcome=%q err=%v", claimed, outcome, err)
		}
		plan, err := ledger.PlanLinks(ctx, now, op.ID, ytwrite.LinkFence{Attempt: 1}, nil)
		if err != nil {
			t.Fatalf("PlanLinks: %v", err)
		}
		if len(plan.Current) != 0 || len(plan.Add) != 1 || plan.Add[0].Target != "AREA-4103" || plan.Add[0].Kind != ytwrite.LinkRelates {
			t.Fatalf("plan = %+v, want one add of AREA-4103", plan)
		}
		rec, found, err := ledger.LatestLinkPlan(ctx, op.ID)
		if err != nil || !found {
			t.Fatalf("LatestLinkPlan found=%v err=%v", found, err)
		}
		if rec.Attempt != 1 || rec.Recover != 0 || rec.Marked != 0 || rec.CreatedAt.IsZero() {
			t.Fatalf("plan record = %+v", rec)
		}
		if len(rec.Plan.Add) != 1 || rec.Plan.Add[0] != plan.Add[0] {
			t.Fatalf("stored plan diff = %+v, want %+v", rec.Plan.Add, plan.Add)
		}
		records, err := ledger.LinkRecords(ctx, ytLinksCaseTenant, "AREA-4102")
		if err != nil {
			t.Fatalf("LinkRecords: %v", err)
		}
		if len(records.Owned) != 1 || !records.Owned[0].Open() || records.Owned[0].IntendedOp != op.ID {
			t.Fatalf("owned rows = %+v, want one open row of op %d", records.Owned, op.ID)
		}
	})
}

// The sweep clock: a feed pass creates the links cursor, which moves the
// sweep generation (I13); a slice at that generation completes the walk and
// the status read reports it, and the next slice has nothing to do.
func init() {
	registerYTAreaCase("links/sweep-generation-status", func(t *testing.T, s *Store) {
		ctx := t.Context()
		s.SetYTLinksMode(YTLinksOn, ytLinksCaseTenant)
		if _, err := s.RunYTFeedConsumers(ctx, 10); err != nil {
			t.Fatalf("RunYTFeedConsumers: %v", err)
		}
		st, err := s.YTLinksStatusOf(ctx, 0, 10)
		if err != nil {
			t.Fatalf("YTLinksStatusOf: %v", err)
		}
		if !st.Open || st.Mode != YTLinksOn || st.Tenant != ytLinksCaseTenant {
			t.Fatalf("status gate = %+v", st)
		}
		if st.Sweep.Generation != 1 || st.Sweep.Complete {
			t.Fatalf("sweep after the cursor was created = %+v, want generation 1 not done", st.Sweep)
		}
		pass, err := s.RunYTLinksSweep(ctx, time.Now().UTC(), 10)
		if err != nil {
			t.Fatalf("RunYTLinksSweep: %v", err)
		}
		if pass.Idle || !pass.Complete || pass.Restarted || pass.Visited != 0 || pass.Generation != 1 {
			t.Fatalf("sweep slice = %+v, want a complete walk of an empty tenant at generation 1", pass)
		}
		st, err = s.YTLinksStatusOf(ctx, 0, 10)
		if err != nil {
			t.Fatalf("YTLinksStatusOf after slice: %v", err)
		}
		if !st.Sweep.Complete || st.Sweep.DoneGeneration != st.Sweep.Generation || st.Sweep.AfterTaskID != 0 {
			t.Fatalf("sweep state after the slice = %+v", st.Sweep)
		}
		idle, err := s.RunYTLinksSweep(ctx, time.Now().UTC(), 10)
		if err != nil || !idle.Idle {
			t.Fatalf("sweep with nothing owed = %+v err=%v, want idle", idle, err)
		}
	})
}

// The plan's step and release record: a marked step counts, and a settlement
// that records it releases the adds the plan never sent, leaving one open and
// one released ownership row.
func init() {
	registerYTAreaCase("links/plan-step-release", func(t *testing.T, s *Store) {
		ctx, now := t.Context(), time.Now().UTC()
		ledger := s.YTWriteLedger()
		n := ytLinksCaseOp(t, 4201, "AREA-4202", ytwrite.IssueLinksDesired{
			Parent:  &ytwrite.IssueLinkTarget{TaskID: 4203, IssueID: "AREA-4204"},
			Related: []ytwrite.IssueLinkTarget{{TaskID: 4205, IssueID: "AREA-4206"}},
		})
		op, _, err := ledger.Prepare(ctx, now, n)
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if _, outcome, err := ledger.Claim(ctx, now, op.ID); err != nil || outcome != ytwrite.ClaimClaimed {
			t.Fatalf("Claim outcome=%q err=%v", outcome, err)
		}
		fence := ytwrite.LinkFence{Attempt: 1}
		plan, err := ledger.PlanLinks(ctx, now, op.ID, fence, nil)
		if err != nil || len(plan.Add) != 2 {
			t.Fatalf("PlanLinks = %+v err=%v, want two adds", plan, err)
		}
		if err := ledger.MarkLinkStep(ctx, now, op.ID, fence, 0); err != nil {
			t.Fatalf("MarkLinkStep: %v", err)
		}
		rec, found, err := ledger.LatestLinkPlan(ctx, op.ID)
		if err != nil || !found || rec.Marked != 1 {
			t.Fatalf("plan record after the mark = %+v found=%v err=%v", rec, found, err)
		}
		settled, err := ledger.SettleLinks(ctx, now, op.ID, fence, ytwrite.LinkSettlement{
			To: ytwrite.StatePrepared, Outcome: ytwrite.StepOK, Next: now.Add(time.Minute), Reason: "defer",
		})
		if err != nil || settled.State != ytwrite.StatePrepared {
			t.Fatalf("SettleLinks = %+v err=%v", settled, err)
		}
		records, err := ledger.LinkRecords(ctx, ytLinksCaseTenant, "AREA-4202")
		if err != nil {
			t.Fatalf("LinkRecords: %v", err)
		}
		open, released := 0, 0
		for _, o := range records.Owned {
			if o.Open() {
				open++
				continue
			}
			if o.ReleasedOp != op.ID || o.ReleasedAt.IsZero() {
				t.Fatalf("released row = %+v, want op %d and a release time", o, op.ID)
			}
			released++
		}
		if open != 1 || released != 1 {
			t.Fatalf("owned rows after the settlement = %+v, want one open and one released", records.Owned)
		}
	})
}
