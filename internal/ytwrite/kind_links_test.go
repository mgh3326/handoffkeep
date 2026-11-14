package ytwrite_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

func linksReq() ytwrite.IssueLinksRequest {
	return ytwrite.IssueLinksRequest{Principal: "hk:task-links", Actor: "hk", Tenant: "yt.example", IssueID: "2-10", TaskID: 10,
		Desired: ytwrite.IssueLinksDesired{
			Parent:     &ytwrite.IssueLinkTarget{TaskID: 30, IssueID: "2-30"},
			Related:    []ytwrite.IssueLinkTarget{{TaskID: 21, IssueID: "2-21"}, {TaskID: 20, IssueID: "2-20"}},
			Duplicates: []ytwrite.IssueLinkTarget{{TaskID: 40, IssueID: "2-40"}},
		},
		Held: []ytwrite.IssueLinkHeld{{Kind: "related", TaskID: 23}, {Kind: "duplicate_of", TaskID: 41}},
	}
}

func linksOpOf(t *testing.T, n ytwrite.NewOp) ytwrite.Op {
	t.Helper()
	return ytwrite.Op{ID: 1, Kind: n.Kind, Payload: n.Payload, EntityKind: n.EntityKind, EntityID: n.EntityID, YTTargetID: n.YTTargetID, Tenant: n.Tenant}
}

// I4: the payload is the desired set and the held list only, canonical, and
// it round-trips; the same set in another order has the same PayloadSHA.
func TestIssueLinksPayloadRoundTripAndSHA(t *testing.T) {
	n, err := ytwrite.IssueLinksOp(linksReq())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"v":2,"desired":{"parent":{"task_id":30,"issue":"2-30"},"related":[{"task_id":20,"issue":"2-20"},{"task_id":21,"issue":"2-21"}],"duplicates":[{"task_id":40,"issue":"2-40"}]},"held":[{"kind":"duplicate_of","task_id":41},{"kind":"related","task_id":23}]}`
	if string(n.Payload) != want {
		t.Fatalf("payload = %s\nwant      %s", n.Payload, want)
	}
	if n.Kind != ytwrite.KindIssueLinks || n.EntityKind != "task" || n.EntityID != "10" || n.YTTargetID != "2-10" || n.Tenant != "yt.example" || n.Key != "" {
		t.Fatalf("op = %+v", n)
	}
	got, err := ytwrite.IssueLinksPayload(linksOpOf(t, n))
	if err != nil {
		t.Fatal(err)
	}
	if got.V != 2 || got.Desired.Parent.IssueID != "2-30" || len(got.Desired.Related) != 2 || len(got.Held) != 2 {
		t.Fatalf("decoded = %+v", got)
	}

	r := linksReq()
	r.Desired.Related = []ytwrite.IssueLinkTarget{r.Desired.Related[1], r.Desired.Related[0]}
	r.Held = []ytwrite.IssueLinkHeld{r.Held[1], r.Held[0]}
	again, err := ytwrite.IssueLinksOp(r)
	if err != nil {
		t.Fatal(err)
	}
	if again.PayloadSHA != n.PayloadSHA || string(again.Payload) != string(n.Payload) {
		t.Fatal("the same set in another order changed the payload")
	}
	for name, change := range map[string]func(*ytwrite.IssueLinksRequest){
		"issue":   func(r *ytwrite.IssueLinksRequest) { r.IssueID = "2-11" },
		"tenant":  func(r *ytwrite.IssueLinksRequest) { r.Tenant = "yt.other" },
		"desired": func(r *ytwrite.IssueLinksRequest) { r.Desired.Parent = nil },
		"held":    func(r *ytwrite.IssueLinksRequest) { r.Held = nil },
	} {
		r := linksReq()
		change(&r)
		other, err := ytwrite.IssueLinksOp(r)
		if err != nil {
			t.Fatal(err)
		}
		if other.PayloadSHA == n.PayloadSHA {
			t.Errorf("%s: PayloadSHA unchanged", name)
		}
	}
	empty, err := ytwrite.IssueLinksOp(ytwrite.IssueLinksRequest{Principal: "p", Actor: "a", Tenant: "t", IssueID: "2-1", TaskID: 1})
	if err != nil || string(empty.Payload) != `{"v":2,"desired":{"related":[],"duplicates":[]},"held":[]}` {
		t.Fatalf("empty set = %s %v", empty.Payload, err)
	}
}

// I4: the checker refuses a baseline (PR #387's `previous`), any other
// unknown field, and another version.
func TestIssueLinksPayloadRefusesABaseline(t *testing.T) {
	n, err := ytwrite.IssueLinksOp(linksReq())
	if err != nil {
		t.Fatal(err)
	}
	for name, payload := range map[string]string{
		"previous":  `{"v":2,"previous":{"related":[]},"desired":{"related":[],"duplicates":[]},"held":[]}`,
		"unknown":   `{"v":2,"desired":{"related":[],"duplicates":[]},"held":[],"extra":1}`,
		"v1":        `{"v":1,"desired":{"related":[],"duplicates":[]},"held":[]}`,
		"pr387":     `{"previous":{"related":[]},"desired":{"related":[],"duplicates":[]}}`,
		"trailing":  `{"v":2,"desired":{"related":[],"duplicates":[]},"held":[]} {}`,
		"self":      `{"v":2,"desired":{"related":[{"task_id":20,"issue":"2-10"}],"duplicates":[]},"held":[]}`,
		"writer":    `{"v":2,"desired":{"related":[{"task_id":10,"issue":"2-20"}],"duplicates":[]},"held":[]}`,
		"twice":     `{"v":2,"desired":{"related":[{"task_id":20,"issue":"2-20"},{"task_id":21,"issue":"2-20"}],"duplicates":[]},"held":[]}`,
		"heldkind":  `{"v":2,"desired":{"related":[],"duplicates":[]},"held":[{"kind":"blocks","task_id":5}]}`,
		"twoparent": `{"v":2,"desired":{"parent":{"task_id":30,"issue":"2-30"},"related":[],"duplicates":[]},"held":[{"kind":"parent","task_id":31}]}`,
		"heldtwice": `{"v":2,"desired":{"related":[{"task_id":20,"issue":"2-20"}],"duplicates":[]},"held":[{"kind":"related","task_id":20}]}`,
	} {
		op := linksOpOf(t, n)
		op.Payload = []byte(payload)
		if _, err := ytwrite.IssueLinksPayload(op); !errors.Is(err, ytwrite.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	op := linksOpOf(t, n)
	op.EntityKind = "decision"
	if _, err := ytwrite.IssueLinksPayload(op); !errors.Is(err, ytwrite.ErrInvalid) {
		t.Errorf("decision lane: err = %v", err)
	}
	r := linksReq()
	r.Tenant = ""
	if _, err := ytwrite.IssueLinksOp(r); err == nil || !strings.Contains(err.Error(), "tenant") {
		t.Errorf("no tenant: %v", err)
	}
}

// I12: the order Execute sends a plan in.
func TestExecuteOrderRemovesTheOldParentFirst(t *testing.T) {
	l := func(k ytwrite.LinkKind, id string) ytwrite.IssueLink { return ytwrite.IssueLink{Kind: k, Target: id} }
	plan := ytwrite.LinkPlan{
		Add:    []ytwrite.IssueLink{l(ytwrite.LinkSubtask, "2-31"), l(ytwrite.LinkRelates, "2-21"), l(ytwrite.LinkDuplicate, "2-41")},
		Remove: []ytwrite.IssueLink{l(ytwrite.LinkSubtask, "2-30"), l(ytwrite.LinkRelates, "2-20")},
	}
	got := ytwrite.ExecuteOrder(plan)
	want := []ytwrite.LinkStep{
		{Add: false, Link: l(ytwrite.LinkSubtask, "2-30")},
		{Add: true, Link: l(ytwrite.LinkSubtask, "2-31")},
		{Add: true, Link: l(ytwrite.LinkRelates, "2-21")},
		{Add: true, Link: l(ytwrite.LinkDuplicate, "2-41")},
		{Add: false, Link: l(ytwrite.LinkRelates, "2-20")},
	}
	if len(got) != len(want) {
		t.Fatalf("order = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// The plan step's compare-and-set: one send plan per attempt, recovery
// plans numbered 1..LinkRecoverMax in order and only from remote_unknown.
func TestCheckLinkPlanStep(t *testing.T) {
	op := ytwrite.Op{ID: 1, State: ytwrite.StateDispatched, Attempts: 2}
	none := ytwrite.LinkPlanRecord{}
	ok := func(err error) bool { return err == nil }
	state := func(err error) bool { return errors.Is(err, ytwrite.ErrState) }
	for _, c := range []struct {
		name    string
		op      ytwrite.Op
		attempt int
		recover int
		latest  ytwrite.LinkPlanRecord
		found   bool
		want    func(error) bool
	}{
		{"send", op, 2, 0, none, false, ok},
		{"send after an earlier attempt's plan", op, 2, 0, ytwrite.LinkPlanRecord{Attempt: 1}, true, ok},
		{"second send plan of one attempt", op, 2, 0, ytwrite.LinkPlanRecord{Attempt: 2}, true, state},
		{"stale attempt", op, 1, 0, none, false, state},
		{"send from remote_unknown", ytwrite.Op{ID: 1, State: ytwrite.StateRemoteUnknown, Attempts: 2}, 2, 0, none, false, state},
		{"first recovery, no plan", ytwrite.Op{ID: 1, State: ytwrite.StateRemoteUnknown, Attempts: 2}, 2, 1, none, false, ok},
		{"first recovery after the send plan", ytwrite.Op{ID: 1, State: ytwrite.StateRemoteUnknown, Attempts: 2}, 2, 1, ytwrite.LinkPlanRecord{Attempt: 2}, true, ok},
		{"recovery out of order", ytwrite.Op{ID: 1, State: ytwrite.StateRemoteUnknown, Attempts: 2}, 2, 3, ytwrite.LinkPlanRecord{Attempt: 2, Recover: 1}, true, state},
		{"recovery past the bound", ytwrite.Op{ID: 1, State: ytwrite.StateRemoteUnknown, Attempts: 2}, 2, ytwrite.LinkRecoverMax + 1, ytwrite.LinkPlanRecord{Attempt: 2, Recover: ytwrite.LinkRecoverMax}, true, state},
		{"recovery of a dispatched op", op, 2, 1, none, false, state},
	} {
		if err := ytwrite.CheckLinkPlanStep(c.op, c.attempt, c.recover, c.latest, c.found); !c.want(err) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

// Review of PR #425: what a plan's durable record says was not made, and
// when its in-flight add was lost. Order: add 2-21, add 2-22, remove 2-20.
func TestUnsentLinkAddsAndLostAdd(t *testing.T) {
	l := func(id string) ytwrite.IssueLink { return ytwrite.IssueLink{Kind: ytwrite.LinkRelates, Target: id} }
	plan := ytwrite.LinkPlan{Add: []ytwrite.IssueLink{l("2-21"), l("2-22")}, Remove: []ytwrite.IssueLink{l("2-20")}}
	for _, c := range []struct {
		name    string
		marked  int
		outcome ytwrite.StepOutcome
		unsent  []ytwrite.IssueLink
		lost    bool
	}{
		{"nothing marked", 0, "", []ytwrite.IssueLink{l("2-21"), l("2-22")}, false},
		{"first add lost", 1, "", []ytwrite.IssueLink{l("2-22")}, true},
		{"first add refused", 1, ytwrite.StepDefinite, []ytwrite.IssueLink{l("2-21"), l("2-22")}, false},
		{"first add ambiguous", 1, ytwrite.StepAmbiguous, []ytwrite.IssueLink{l("2-22")}, false},
		{"second add ok", 2, ytwrite.StepOK, nil, false},
		{"remove lost", 3, "", nil, false},
	} {
		r := ytwrite.LinkPlanRecord{Plan: plan, Marked: c.marked, Outcome: c.outcome}
		if got := ytwrite.UnsentLinkAdds(r); !slices.Equal(got, c.unsent) {
			t.Errorf("%s: unsent = %+v, want %+v", c.name, got, c.unsent)
		}
		if got := r.LostAdd(); got != c.lost {
			t.Errorf("%s: lost = %v", c.name, got)
		}
	}
}

// Review of PR #425: the fence of every planned-kind ledger write.
func TestCheckLinkFence(t *testing.T) {
	dispatched := ytwrite.Op{ID: 1, State: ytwrite.StateDispatched, Attempts: 2, Reconciles: 0}
	unknown := ytwrite.Op{ID: 1, State: ytwrite.StateRemoteUnknown, Attempts: 2, Reconciles: 3}
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	leased := unknown
	leased.Reason, leased.NextAttemptAt = ytwrite.ReasonLinkRecovery, now.Add(time.Minute)
	leaseOver := leased
	leaseOver.NextAttemptAt = now
	for _, c := range []struct {
		name string
		op   ytwrite.Op
		f    ytwrite.LinkFence
		ok   bool
	}{
		{"send", dispatched, ytwrite.LinkFence{Attempt: 2}, true},
		{"send of an earlier attempt", dispatched, ytwrite.LinkFence{Attempt: 1}, false},
		{"send after stale recovery", unknown, ytwrite.LinkFence{Attempt: 2}, false},
		{"recovery holding the lease", unknown, ytwrite.LinkFence{Attempt: 2, Epoch: 3}, true},
		{"superseded recovery", unknown, ytwrite.LinkFence{Attempt: 2, Epoch: 2}, false},
		{"recovery of a dispatched op", dispatched, ytwrite.LinkFence{Attempt: 2, Epoch: 3}, false},
		{"operator", unknown, ytwrite.LinkFence{Attempt: 2, Epoch: ytwrite.EpochOperator}, true},
		{"operator on a dispatched op", dispatched, ytwrite.LinkFence{Attempt: 2, Epoch: ytwrite.EpochOperator}, false},
		{"operator while a pass holds the lease", leased, ytwrite.LinkFence{Attempt: 2, Epoch: ytwrite.EpochOperator}, false},
		{"operator after the lease ran out", leaseOver, ytwrite.LinkFence{Attempt: 2, Epoch: ytwrite.EpochOperator}, true},
		{"the pass holding the lease", leased, ytwrite.LinkFence{Attempt: 2, Epoch: 3}, true},
	} {
		err := ytwrite.CheckLinkFence(c.op, c.f, now)
		if (err == nil) != c.ok || (err != nil && !errors.Is(err, ytwrite.ErrState)) {
			t.Errorf("%s: err = %v", c.name, err)
		}
	}
}

// Review of PR #425: a failed mark whose transaction committed records its
// step as definite (never sent); one that did not commit leaves the earlier
// step's outcome.
func TestLinkSettlementStepRecord(t *testing.T) {
	for _, c := range []struct {
		name    string
		s       ytwrite.LinkSettlement
		marked  int
		step    int
		outcome ytwrite.StepOutcome
		ok      bool
	}{
		{"in-flight outcome", ytwrite.LinkSettlement{Outcome: ytwrite.StepAmbiguous}, 2, 1, ytwrite.StepAmbiguous, true},
		{"nothing sent", ytwrite.LinkSettlement{}, 0, 0, "", false},
		{"first mark committed, reply lost", ytwrite.LinkSettlement{Stopped: true, Step: 0}, 1, 0, ytwrite.StepDefinite, true},
		{"first mark not committed", ytwrite.LinkSettlement{Stopped: true, Step: 0}, 0, 0, "", false},
		{"later mark committed, reply lost", ytwrite.LinkSettlement{Outcome: ytwrite.StepOK, Stopped: true, Step: 1}, 2, 1, ytwrite.StepDefinite, true},
		{"later mark not committed", ytwrite.LinkSettlement{Outcome: ytwrite.StepOK, Stopped: true, Step: 1}, 1, 0, ytwrite.StepOK, true},
	} {
		step, outcome, ok := c.s.StepRecord(c.marked)
		if step != c.step || outcome != c.outcome || ok != c.ok {
			t.Errorf("%s: = %d %q %v, want %d %q %v", c.name, step, outcome, ok, c.step, c.outcome, c.ok)
		}
	}
}
