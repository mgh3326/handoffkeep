package ytwrite

import (
	"context"
	"fmt"
	"time"
)

// The ledger side of article_split (hk 5085). A split op's plans and its
// executor record share the planned-op tables of issue_links (yt_link_plans,
// yt_link_steps: keyed by op, the plan a JSON value), and the same fence
// (CheckLinkFence), plan compare-and-set (CheckLinkPlanStep), lease
// (LeaseLinkRecovery) and step record (LinkSettlement.StepRecord). What is
// the split's own: the plan is a list of creates (SplitPlan), there is no
// ownership record, and the index id is recorded on the op (its YTResultID)
// before the first request that depends on it.

// SplitPlanTag marks a stored plan as an article_split plan.
const SplitPlanTag = "article_split"

// SplitPlan is one plan of an article_split op: the index the read found
// ("" for none) and the creates the plan makes, in order. Step value 0
// creates the index; i ≥ 1 creates part i of Parts.
type SplitPlan struct {
	Kind  string `json:"kind"`
	Index string `json:"index,omitempty"`
	Parts int    `json:"parts"`
	Steps []int  `json:"steps"`
}

// PlanFamily is the plan for a read: the index when the read shows none,
// then every part it does not show, ascending.
func planFamily(r familyRead) SplitPlan {
	p := SplitPlan{Kind: SplitPlanTag, Index: r.index, Parts: len(r.parts), Steps: []int{}}
	if r.index == "" {
		p.Steps = append(p.Steps, 0)
	}
	for i, id := range r.parts {
		if id == "" || r.index == "" {
			p.Steps = append(p.Steps, i+1)
		}
	}
	return p
}

// CreatesIndex reports whether the plan's first create is the index.
func (p SplitPlan) CreatesIndex() bool { return len(p.Steps) > 0 && p.Steps[0] == 0 }

// SplitPlanRecord is one stored split plan and what its executor recorded
// (LinkPlanRecord's Marked and Outcome).
type SplitPlanRecord struct {
	Attempt int
	Recover int
	Plan    SplitPlan
	Marked  int
	Outcome StepOutcome
}

// linkRecord is the record in LinkPlanRecord's shape, for the checks the two
// planned kinds share.
func (r SplitPlanRecord) linkRecord() LinkPlanRecord {
	return LinkPlanRecord{Attempt: r.Attempt, Recover: r.Recover, Marked: r.Marked, Outcome: r.Outcome}
}

// Lost reports a plan whose in-flight create has no recorded outcome: its
// executor stopped between the request and its settlement (S7).
func (r SplitPlanRecord) Lost() bool {
	return r.Outcome == "" && r.Marked > 0 && r.Marked <= len(r.Plan.Steps)
}

// Uncertain reports a plan whose in-flight create may have been made and may
// still land: its outcome is ambiguous (a 5xx, 408, timeout or transport
// failure after the request left) or was never recorded (Lost). A read that
// does not show it is not proof it failed: an article create is not
// deduplicated, so a later commit beside a second create is a duplicate part
// (review of PR #511).
func (r SplitPlanRecord) Uncertain() bool {
	return r.Marked > 0 && r.Marked <= len(r.Plan.Steps) && (r.Outcome == "" || r.Outcome == StepAmbiguous)
}

// InFlight is the create of the plan's in-flight step (0: the index).
func (r SplitPlanRecord) InFlight() int { return r.Plan.Steps[r.Marked-1] }

// IndexMaybeSent reports whether any plan of the op marked its index create
// with an outcome other than definite (refused, or never sent): such a create
// may have made the index, and only a read that finds it may continue the
// family. Search absence never makes a second one (S1).
func IndexMaybeSent(history []SplitPlanRecord) bool {
	for _, r := range history {
		if r.Plan.CreatesIndex() && r.Marked >= 1 && !(r.Marked == 1 && r.Outcome == StepDefinite) {
			return true
		}
	}
	return false
}

// AnySent reports whether any plan of the op may have sent a create: a step
// marked with an outcome other than definite, or a step after the first.
func AnySent(history []SplitPlanRecord) bool {
	for _, r := range history {
		if r.Marked > 1 || (r.Marked == 1 && r.Outcome != StepDefinite) {
			return true
		}
	}
	return false
}

// LatestSplitPlan is the newest record of history (oldest first).
func LatestSplitPlan(history []SplitPlanRecord) (SplitPlanRecord, bool) {
	if len(history) == 0 {
		return SplitPlanRecord{}, false
	}
	return history[len(history)-1], true
}

// CheckSplitPlan is the compare-and-set both ledgers apply before PlanSplit
// writes: the plan step of CheckLinkPlanStep, a well-formed plan, and no index
// create once one may have been sent or an index id is recorded (S1). A
// refusal is ErrState; an index other than the recorded one is
// ErrConflictingResult.
func CheckSplitPlan(op Op, history []SplitPlanRecord, f LinkFence, plan SplitPlan) error {
	latest, found := LatestSplitPlan(history)
	if err := CheckLinkPlanStep(op, f.Attempt, f.Recover, latest.linkRecord(), found); err != nil {
		return err
	}
	if plan.Kind != SplitPlanTag || plan.Parts < 2 {
		return fmt.Errorf("%w: op %d: not a split plan of two or more parts", ErrInvalid, op.ID)
	}
	prev := -1
	for i, s := range plan.Steps {
		if s < 0 || s > plan.Parts || s <= prev || (s == 0) != (i == 0 && plan.Index == "") {
			return fmt.Errorf("%w: op %d: plan steps %v are not the index then ascending parts", ErrInvalid, op.ID, plan.Steps)
		}
		prev = s
	}
	if plan.Index == "" && !plan.CreatesIndex() {
		return fmt.Errorf("%w: op %d: a plan without an index must create it first", ErrInvalid, op.ID)
	}
	if plan.CreatesIndex() && (op.YTResultID != "" || IndexMaybeSent(history)) {
		return fmt.Errorf("%w: op %d may already have made its index (recorded %q): no second index", ErrState, op.ID, op.YTResultID)
	}
	if plan.Index != "" {
		if err := CheckSplitIndex(op, plan.Index); err != nil {
			return err
		}
	}
	return nil
}

// CheckSplitIndex refuses to record an index id other than the one op holds.
func CheckSplitIndex(op Op, index string) error {
	if !resultIDRE.MatchString(index) {
		return fmt.Errorf("%w: index id %q is malformed", ErrInvalid, index)
	}
	if op.YTResultID != "" && op.YTResultID != index {
		return fmt.Errorf("%w: op %d recorded index %s, not %s", ErrConflictingResult, op.ID, op.YTResultID, index)
	}
	return nil
}

// CheckSplitMark is MarkSplitStep's check under the op's lock: latest is f's
// plan, step is its next, and every step after an index create carries the
// index id (recorded before the first part's request).
func CheckSplitMark(op Op, latest SplitPlanRecord, found bool, f LinkFence, step int, index string) error {
	if !found || latest.Attempt != f.Attempt || latest.Recover != f.Recover || step != latest.Marked || step >= len(latest.Plan.Steps) {
		return fmt.Errorf("%w: mark step %d of plan %d.%d; latest is %d.%d (found %v), marked %d",
			ErrState, step, f.Attempt, f.Recover, latest.Attempt, latest.Recover, found, latest.Marked)
	}
	if latest.Plan.Steps[step] != 0 && index == "" {
		return fmt.Errorf("%w: op %d: a part is requested before its index is recorded", ErrState, op.ID)
	}
	if latest.Plan.Index != "" && index != latest.Plan.Index {
		return fmt.Errorf("%w: op %d: plan index %s, step index %q", ErrState, op.ID, latest.Plan.Index, index)
	}
	if index != "" {
		return CheckSplitIndex(op, index)
	}
	return nil
}

// CheckSplitSettlement checks a split settlement: never applied (an applied
// split goes through Ledger.Apply, which publishes the binding), and an index
// id only the one op holds.
func CheckSplitSettlement(op Op, s LinkSettlement) error {
	if s.To == StateApplied {
		return fmt.Errorf("%w: a split op applies through Apply", ErrState)
	}
	if err := CheckLinkSettlement(op, s); err != nil {
		return err
	}
	if s.YTID != "" {
		return CheckSplitIndex(op, s.YTID)
	}
	return nil
}

// CheckSplitApply is ApplySplit's check under the op's lock: f is still the
// op's executor (CheckLinkFence: for an operator, no recovery pass holds the
// lease), the index is the one the op recorded if it recorded one, and an
// operator's apply was made from the latest plan as it is now
// (CheckOperatorSaw), so no create began after the operator looked (review
// of PR #511).
func CheckSplitApply(op Op, history []SplitPlanRecord, f LinkFence, s LinkSettlement, now time.Time) error {
	if err := CheckLinkFence(op, f, now); err != nil {
		return err
	}
	if err := CheckSplitIndex(op, s.YTID); err != nil {
		return err
	}
	if f.Epoch == EpochOperator {
		latest, found := LatestSplitPlan(history)
		return CheckOperatorSaw(s, latest.linkRecord(), found)
	}
	return nil
}

// SplitLedger is the ledger side of article_split. A Ledger that does not
// implement it cannot send one (the op is rejected unsupported_kind before
// anything is read).
type SplitLedger interface {
	// PlanSplit appends plan as op id's plan (f.Attempt, f.Recover) after
	// CheckLinkFence and CheckSplitPlan over the op's whole history, in one
	// transaction under the op's lane lock. Else ErrState and nothing
	// changes.
	PlanSplit(ctx context.Context, now time.Time, id int64, f LinkFence, plan SplitPlan) error
	// SplitPlans is op id's every plan, oldest first, with its executor's
	// record.
	SplitPlans(ctx context.Context, id int64) ([]SplitPlanRecord, error)
	// MarkSplitStep records that f's executor is about to send step of its
	// plan and, when index is set, records it as the op's index (YTResultID),
	// in one transaction: CheckLinkFence and CheckSplitMark, else ErrState
	// (ErrConflictingResult for another index) and nothing changes.
	MarkSplitStep(ctx context.Context, now time.Time, id int64, f LinkFence, step int, index string) error
	// SettleSplit settles op id for the executor f in one transaction: the
	// outcome of its in-flight step (s.StepRecord) when f's plan is the
	// latest, the index id (s.YTID) when set, and the op's move
	// (CheckSplitSettlement). An operator's settlement (EpochOperator) is
	// refused when the latest plan is no longer the one it saw
	// (CheckOperatorSaw). f must pass CheckLinkFence, else ErrState.
	SettleSplit(ctx context.Context, now time.Time, id int64, f LinkFence, s LinkSettlement) (Op, error)
	// ApplySplit applies op id with index s.YTID and its map rows as
	// Ledger.Apply does (binding included), in one transaction that first
	// checks, under the op's lock, CheckSplitApply for the executor f. An op
	// already applied with s.YTID is returned as it is. Else ErrState,
	// ErrConflictingResult, and nothing changes.
	ApplySplit(ctx context.Context, now time.Time, id int64, f LinkFence, s LinkSettlement, maps []MapRow) (Op, error)
	// LeaseLinkRecovery is the recovery lease issue_links uses.
	LeaseLinkRecovery(ctx context.Context, now time.Time, id int64, reconciles int, until time.Time) (epoch int, ok bool, err error)
}
