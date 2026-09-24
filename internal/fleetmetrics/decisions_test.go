package fleetmetrics

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func relay(id int64, kind, job, lane string, h float64) store.RelayEvent {
	return store.RelayEvent{ID: id, Kind: kind, JobID: job, OwnerLane: lane, ReceivedAt: at(h)}
}

// decisionsFixture: task 20 answered by the operator on the web after 2h;
// task 21 still open; escalation 500 answered by CLI after 1h; escalation 501
// closed by its job without a recorded answer; lane decision 600 answered
// after 1h; lane decision 601 open.
func decisionsFixture() Snapshot {
	s := snap(0, 48)
	answer := ev(3, "needs_decision", "claimed", nil, "A — via web")
	answer.By = "operator:operator@example.com"
	s.Tasks = []store.Task{
		task(20, "in_progress", 0, store.TaskRefs{},
			ev(0.5, "backlog", "claimed", nil), ev(1, "claimed", "needs_decision", nil, "[decision-needed] A or B?"),
			answer, ev(4, "claimed", "in_progress", nil)),
		task(21, "needs_decision", 0, store.TaskRefs{},
			ev(0.5, "backlog", "claimed", nil), ev(5, "claimed", "needs_decision", nil, "which lane?")),
	}
	webAnswer := relay(700, "lane.event", "", "director-1", 3)
	webAnswer.EventID, webAnswer.Text, webAnswer.DeliveredAt = "web-decision-task-20-abcd", "[decision] #20: A (from operator(web) x)", atp(3.1)
	cliAnswer := relay(701, "lane.event", "", "b-esc", 11)
	cliAnswer.EventID, cliAnswer.Text, cliAnswer.DeliveredAt = "cli-decision-escalation-500-ffff", "[decision] #500: go", atp(11.2)
	laneAsk := relay(600, "lane.event", "", "b-x", 30)
	laneAsk.Text = "[decision-needed] pick A or B"
	laneAnswer := relay(702, "lane.event", "", "b-x", 31)
	laneAnswer.EventID, laneAnswer.Text = "desk-answer-1", "[decision-answered] #600: A"
	laneOpen := relay(601, "lane.event", "", "b-y", 40)
	laneOpen.Text = "[decision-needed] rollback?"
	s.Relay = []store.RelayEvent{
		relay(500, "job.escalate", "j-esc", "b-esc", 10),
		relay(501, "job.escalate", "j-esc2", "b-esc2", 20),
		laneAsk, laneOpen, webAnswer, cliAnswer, laneAnswer,
		relay(703, "job.joined", "j-esc", "b-esc", 12),
		relay(704, "job.completed", "j-esc2", "b-esc2", 22),
	}
	return s
}

func TestDecisionDwell(t *testing.T) {
	r := Compute(decisionsFixture()).Decisions
	if r.Requests != 6 || r.ByKind["task"] != 2 || r.ByKind["escalation"] != 2 || r.ByKind["lane"] != 2 {
		t.Fatalf("requests = %d %v, want 6 (task 2, escalation 2, lane 2)", r.Requests, r.ByKind)
	}
	if r.ToAnswer == nil || r.ToAnswer.N != 3 || r.ToAnswer.P50 != 1 || r.ToAnswer.Max != 2 {
		t.Fatalf("request→answer = %+v, want n=3 p50=1 max=2", r.ToAnswer)
	}
	if r.AnswerToDeliver == nil || r.AnswerToDeliver.N != 2 || r.AnswerToDeliver.Max != 0.2 {
		t.Fatalf("answer→delivery = %+v, want n=2 max=0.2", r.AnswerToDeliver)
	}
	if r.AnswerToConsume == nil || r.AnswerToConsume.N != 2 || r.AnswerToConsume.P50 != 1 {
		t.Fatalf("answer→consumption = %+v, want n=2 p50=1", r.AnswerToConsume)
	}
	if r.AnsweredBy["operator(web)"] != 1 || r.AnsweredBy["resolver"] != 2 {
		t.Fatalf("answered by = %v", r.AnsweredBy)
	}
	if r.Open != 2 || r.OldestOpen == nil || *r.OldestOpen != 43 || r.OldestOpenRef != "task#21" {
		t.Fatalf("open = %d oldest %v %s, want 2, 43h task#21", r.Open, r.OldestOpen, r.OldestOpenRef)
	}
	ans := findCoverage(r.Coverage, "decision requests")
	if ans.Linked != 3 || ans.Total != 6 || ans.Detail["closed without recorded answer"] != 1 || ans.Detail["still open"] != 2 {
		t.Fatalf("answer coverage = %+v", ans)
	}
	cons := findCoverage(r.Coverage, "answered requests with a consumption")
	if cons.Linked != 2 || cons.Detail["no consumption record for lane decisions"] != 1 {
		t.Fatalf("consumption coverage = %+v", cons)
	}
}

// Escalation 500 losing its answer event (the identifier) moves it from the
// value into coverage; the remaining distribution does not gain a 0h entry.
func TestDecisionMissingAnswerLowersCoverageNotValue(t *testing.T) {
	s := decisionsFixture()
	for i := range s.Relay {
		if s.Relay[i].ID == 701 {
			s.Relay[i].EventID = "unrelated"
		}
	}
	r := Compute(s).Decisions
	if r.ToAnswer == nil || r.ToAnswer.N != 2 || r.ToAnswer.P50 != 1 || r.ToAnswer.Max != 2 {
		t.Fatalf("request→answer = %+v, want n=2 (task 20: 2h, lane 600: 1h)", r.ToAnswer)
	}
	ans := findCoverage(r.Coverage, "decision requests")
	if ans.Linked != 2 || ans.Detail["closed without recorded answer"] != 2 {
		t.Fatalf("answer coverage = %+v, want 2 linked, 2 closed without answer", ans)
	}
}

// dev is a #618 decision row: a request recorded (or resolved) with no state
// change, from == to == the task's current state.
func dev(h float64, state string) store.TaskEvent {
	e := ev(h, state, state, nil, "decision-request dr-x-1: q")
	e.Kind = store.TaskEventDecision
	return e
}

// Decision rows are not state changes: a superseding request recorded while
// the task sits in needs_decision is not a second entry nor an answer, and a
// request closed on a merged task does not move the task's terminal time.
func TestDecisionRowsAreNotStateEvents(t *testing.T) {
	s := snap(0, 48)
	s.Tasks = []store.Task{
		task(30, "claimed", 0, store.TaskRefs{},
			ev(0.5, "backlog", "claimed", nil), ev(1, "claimed", "needs_decision", nil, "A or B?"),
			dev(2, "needs_decision"), ev(3, "needs_decision", "claimed", nil, "A")),
	}
	r := Compute(s).Decisions
	if r.Requests != 1 || r.ByKind["task"] != 1 {
		t.Fatalf("requests = %d %v, want 1 task request (the decision row is not a new entry)", r.Requests, r.ByKind)
	}
	if r.ToAnswer == nil || r.ToAnswer.N != 1 || r.ToAnswer.Max != 2 {
		t.Fatalf("request→answer = %+v, want n=1 max=2 (answered at the transition, not the decision row)", r.ToAnswer)
	}
	merged := task(31, "merged", 0, store.TaskRefs{}, append(mergedChain(1, 2, 3, nil), dev(10, "merged"))...)
	if at := terminalAt(&merged); at == nil || !at.Equal(t0.Add(3*time.Hour)) {
		t.Fatalf("terminalAt = %v, want the merge at +3h, not the decision row at +10h", at)
	}
	if st := stateAt(&merged, t0.Add(11*time.Hour)); st != "merged" {
		t.Fatalf("stateAt = %q", st)
	}
}
