package ytwrite

import (
	"context"
	"fmt"
	"time"
)

// The planned send of issue_links (hk 4638 S3; design/2026-11-13/yt-issue-links
// §5.4, §5.5). A planned kind is not sent from a stored request: every attempt
// and every recovery pass reads the issue, has the ledger plan the diff
// against its ownership record in one transaction (the ownership of each link
// it will add is committed before the request that adds it, I6), sends only
// that diff, and reads again. Adding a link the read shows present is never
// planned, so a replan cannot duplicate anything; that is why recovery may
// mutate, which no other kind does (I5).
//
// What a read cannot tell is who made a present link, or whether a request
// that left was applied (review of PR #425, consult-hk4638-links-root). So
// the ledger also keeps what each executor did: before each request the
// executor marks its step in flight (MarkLinkStep), and its settlement
// records the outcome of that step together with the release of the adds it
// did not make and the op's state change, in one transaction (SettleLinks).
// Ownership of an add is closed from that durable record, never from memory:
// every add after the in-flight step was never sent, and the in-flight add
// was not made when its outcome is definite. An in-flight add with no
// recorded outcome (the executor stopped between the request and its
// settlement) is lost: recovery holds the op for an operator rather than
// guess (ReasonOutcomeLost).
//
// Every ledger write of an executor is fenced (LinkFence): the send by its
// dispatch attempt, a recovery pass by the epoch its lease issued. A writer
// that was superseded changes nothing. The fence cannot stop a request a
// superseded executor already sent; the work deadline, which ends before
// the lease, bounds that.

// Reasons of the planned path.
const (
	// ReasonPartialWrite: a planned send completed a mutation and then
	// failed, or its final read did not show the plan done.
	ReasonPartialWrite = "partial_write"
	// ReasonUnverified: the final read after a planned send failed.
	ReasonUnverified = "unverified"
	// ReasonRecoverExhausted: LinkRecoverMax recovery plans did not converge;
	// the op stays remote_unknown for an operator (I9).
	ReasonRecoverExhausted = "recover_exhausted"
	// ReasonLinkRecovery: a recovery pass holds the op's lease (until its
	// NextAttemptAt); the pass's settlement replaces it. An operator's
	// resolve waits for it (CheckLinkFence).
	ReasonLinkRecovery = "link_recovery"
	// ReasonOutcomeLost: the latest plan's in-flight step is an add whose
	// outcome was never recorded. A 4xx followed by a person making the same
	// link and an add that landed read the same, so recovery makes no
	// request and the op stays remote_unknown for an operator's resolve.
	ReasonOutcomeLost = "outcome_lost"
)

// LinkRecoverMax bounds the recovery plans of one op (I9). After it the op
// makes no request; it stays remote_unknown / recover_exhausted, which the
// stuck-op alert reports, until an operator resolves it.
const LinkRecoverMax = 5

// PlannedKind is a kind sent and recovered by read, plan, execute, read.
type PlannedKind interface {
	Kind
	// Read reads the target's current links; no mutation. An error is
	// "nothing sent".
	Read(ctx context.Context, remote Remote, op Op) (IssueLinksRead, error)
	// Execute sends plan's diff in ExecuteOrder and returns the mutations it
	// completed. It calls mark(i) before step i's request and stops, sending
	// nothing more, when mark fails.
	Execute(ctx context.Context, remote Remote, op Op, types LinkTypes, plan LinkPlan, mark func(step int) error) (int, error)
	// Converged reports whether cur shows plan done.
	Converged(op Op, plan LinkPlan, cur []IssueLink) (bool, error)
}

// LinkPlan is one plan of an issue_links op: what the read showed and what
// the plan does about each link of it. Every list is sorted.
type LinkPlan struct {
	Current []IssueLink `json:"current"`
	// Add are desired links the read did not show; Remove are links this
	// writer owns that it no longer wants (I1).
	Add    []IssueLink `json:"add"`
	Remove []IssueLink `json:"remove"`
	// Unowned are links on a managed type nobody in hk created: recorded,
	// left (I7). Peer are links another writer task owns or writes: left,
	// not reported (I2). Kept are links this writer owns and no longer
	// wants, left because their edge is still live with its target unbound
	// or rebound (I14).
	Unowned []IssueLink `json:"unowned"`
	Peer    []IssueLink `json:"peer"`
	Kept    []IssueLink `json:"kept"`
}

// LinkPlanRecord is one stored plan and what its executor recorded.
type LinkPlanRecord struct {
	Attempt   int
	Recover   int
	Plan      LinkPlan
	CreatedAt time.Time
	// Marked is how many steps of the plan (ExecuteOrder) were marked in
	// flight; step Marked-1 is the in-flight one. Every step before it
	// succeeded (the next was marked only after it), none after it was sent.
	Marked int
	// Outcome is the in-flight step's outcome as its settlement recorded
	// it; "" when none was recorded.
	Outcome StepOutcome
}

// StepOutcome is what the executor of a plan saw of its in-flight step.
type StepOutcome string

const (
	// StepOK: the step succeeded (the plan ran to its end, or stopped
	// before the next step was sent).
	StepOK StepOutcome = "ok"
	// StepDefinite: the step was not applied (refused before sending, or a
	// 4xx other than 408).
	StepDefinite StepOutcome = "definite"
	// StepAmbiguous: the step may have been applied (5xx, 408, transport).
	StepAmbiguous StepOutcome = "ambiguous"
)

// LostAdd reports a plan whose in-flight step is an add with no recorded
// outcome (ReasonOutcomeLost).
func (r LinkPlanRecord) LostAdd() bool {
	steps := ExecuteOrder(r.Plan)
	return r.Outcome == "" && r.Marked > 0 && r.Marked <= len(steps) && steps[r.Marked-1].Add
}

// UnsentLinkAdds are the adds of r's plan that were not made, by its durable
// record: every add after the in-flight step (never sent), and the in-flight
// step's own add when its outcome is definite. Both ledgers close their
// ownership whenever anything settles or supersedes the plan. An add whose
// outcome is ok, ambiguous or lost stays owned.
func UnsentLinkAdds(r LinkPlanRecord) []IssueLink {
	var out []IssueLink
	for i, s := range ExecuteOrder(r.Plan) {
		if s.Add && (i >= r.Marked || (i == r.Marked-1 && r.Outcome == StepDefinite)) {
			out = append(out, s.Link)
		}
	}
	return out
}

// LinkFence names the executor a ledger write of a planned kind comes from.
// Attempt and Recover name its plan (Recover 0: the send of dispatch Attempt).
// Epoch 0 is that send: the op must still be dispatched in Attempt. Epoch n >
// 0 is a recovery pass: the op must be remote_unknown in Attempt with
// reconciles n, the epoch LeaseLinkRecovery issued it. EpochOperator is an
// operator's resolve: the op must be remote_unknown in Attempt and no
// recovery pass may hold its lease (reason link_recovery, due after now),
// since that pass may have a request in flight.
type LinkFence struct {
	Attempt, Recover, Epoch int
}

// EpochOperator is the fence epoch of an operator's resolve.
const EpochOperator = -1

// CheckLinkFence is the fence both ledgers check, under the op's row lock,
// before any write of a planned kind, now being the write's time. A refusal
// is ErrState.
func CheckLinkFence(op Op, f LinkFence, now time.Time) error {
	ok := op.Attempts == f.Attempt
	switch {
	case f.Epoch == 0:
		ok = ok && op.State == StateDispatched
	case f.Epoch == EpochOperator:
		ok = ok && op.State == StateRemoteUnknown && !(op.Reason == ReasonLinkRecovery && op.NextAttemptAt.After(now))
	default:
		ok = ok && op.State == StateRemoteUnknown && op.Reconciles == f.Epoch
	}
	if !ok {
		return fmt.Errorf("%w: op %d is %s at attempt %d, reconciles %d; the writer holds attempt %d, epoch %d",
			ErrState, op.ID, op.State, op.Attempts, op.Reconciles, f.Attempt, f.Epoch)
	}
	return nil
}

// LinkSettlement is one settlement of a planned op: the outcome of the
// executor's in-flight step, and the state the op moves to.
type LinkSettlement struct {
	// To is applied, rejected, prepared (defer, from dispatched only) or
	// remote_unknown (from dispatched: mark unknown; from remote_unknown:
	// the end of a recovery pass, due again at Next; the lease already
	// counted the pass).
	To State
	// Outcome is the in-flight step's outcome; "" when this executor sent
	// nothing.
	Outcome StepOutcome
	// Stopped says the executor stopped before sending step Step because
	// its mark failed. The mark may have committed with its reply lost:
	// the ledger then records that step as definite (never sent).
	Stopped bool
	Step    int
	// Read and Cur: for applied, the read that showed the plan done. An
	// operator's apply has no read.
	Read bool
	Cur  []IssueLink
	// YTID is the applied op's result id (its issue).
	YTID           string
	Next           time.Time
	Reason, Detail string
}

// StepRecord is the outcome s records for its executor's plan, which has
// marked steps: the stopped step as definite when its mark committed, else
// s.Outcome for the in-flight step. ok is false when there is nothing to
// record.
func (s LinkSettlement) StepRecord(marked int) (step int, outcome StepOutcome, ok bool) {
	if s.Stopped && marked == s.Step+1 {
		return s.Step, StepDefinite, true
	}
	if s.Outcome != "" && marked > 0 {
		return marked - 1, s.Outcome, true
	}
	return 0, "", false
}

// CheckLinkSettlement checks that s can move op (already fenced). A refusal is
// ErrState.
func CheckLinkSettlement(op Op, s LinkSettlement) error {
	ok := false
	switch s.To {
	case StateApplied, StateRejected, StateRemoteUnknown:
		ok = op.State == StateDispatched || op.State == StateRemoteUnknown
	case StatePrepared:
		ok = op.State == StateDispatched
	}
	if !ok {
		return fmt.Errorf("%w: settle op %d from %s to %s", ErrState, op.ID, op.State, s.To)
	}
	return nil
}

// OwnedLink is one yt_link_owned row: a link hk planned to create, at a
// moment its plan's read showed it absent. From is the writer's issue; for a
// relates link To is the other end, for subtask the parent, for duplicate
// the original.
type OwnedLink struct {
	ID         int64
	Tenant     string
	LinkType   LinkKind
	From, To   string
	WriterTask int64
	TargetTask int64
	// Edge is the hk edge kind (parent, related, duplicate_of).
	Edge       string
	IntendedOp int64
	IntendedAt time.Time
	ReleasedOp int64     // 0: not released
	ReleasedAt time.Time // zero: open
}

// Open reports a row the plan still counts: not closed by a read.
func (o OwnedLink) Open() bool { return o.ReleasedAt.IsZero() }

// UnownedLink is one yt_link_unowned row: a managed-type link seen from
// IssueID that hk did not create. Side is the role IssueID plays (UnownedSide).
type UnownedLink struct {
	ID          int64
	Tenant      string
	IssueID     string
	LinkType    LinkKind
	Side        string
	OtherIssue  string
	FirstSeenOp int64
	LastSeenOp  int64
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	GoneAt      time.Time // zero: still seen
}

// UnownedSide is the role the reading issue plays in a link of kind.
func UnownedSide(kind LinkKind) string {
	switch kind {
	case LinkSubtask:
		return "subtask of"
	case LinkDuplicate:
		return "duplicates"
	}
	return "relates to"
}

// IssueLinkRecords is what the ledger holds about one issue's links: the
// ownership rows naming it on either side and the unowned rows seen from it,
// each oldest first.
type IssueLinkRecords struct {
	Owned   []OwnedLink
	Unowned []UnownedLink
}

// LinksLedger is the ledger side of a planned kind. A Ledger that does not
// implement it cannot send one (the op is rejected unsupported_kind before
// anything is read).
type LinksLedger interface {
	// PlanLinks plans op id from cur, the links a read of its target just
	// showed, in one transaction under the op's lane lock, for the executor
	// f. It first closes ownership of the latest plan's unsent adds
	// (UnsentLinkAdds), then records ownership of each link it adds (I6),
	// marks each link it removes as releasing, records unowned links, closes
	// rows the read shows gone, and appends the plan (f.Attempt, f.Recover).
	// f must pass CheckLinkFence and the plan CheckLinkPlanStep; anything
	// else is ErrState and changes nothing.
	PlanLinks(ctx context.Context, now time.Time, id int64, f LinkFence, cur []IssueLink) (LinkPlan, error)
	// LatestLinkPlan is op id's newest plan with its executor's record;
	// found is false for none.
	LatestLinkPlan(ctx context.Context, id int64) (rec LinkPlanRecord, found bool, err error)
	// MarkLinkStep records that f's executor is about to send step of its
	// plan: f must pass CheckLinkFence, its plan must be op id's latest, and
	// step must be the plan's next (Marked). Else ErrState.
	MarkLinkStep(ctx context.Context, now time.Time, id int64, f LinkFence, step int) error
	// SettleLinks settles op id for the executor f in one transaction: it
	// records s.StepRecord as the outcome of that step when f's plan is the
	// latest, closes ownership of the latest plan's unsent adds
	// (UnsentLinkAdds, from the record just written), and moves the op
	// (CheckLinkSettlement). An applied op with a read also closes the
	// writer's releasing rows and the issue's unowned rows that s.Cur no
	// longer shows. f must pass CheckLinkFence, else ErrState and nothing
	// changes.
	SettleLinks(ctx context.Context, now time.Time, id int64, f LinkFence, s LinkSettlement) (Op, error)
	// LeaseLinkRecovery gives one recovery pass of op id exclusive use of it
	// until until: a compare-and-set that holds only while the op is
	// remote_unknown, due at or before now, and has reconciles as listed. It
	// adds one to reconciles (one per pass), sets reason link_recovery and
	// moves the due time to until;
	// epoch is the new reconciles, the pass's fence. ok is false when another
	// pass won it or the op moved on; that pass makes no request.
	LeaseLinkRecovery(ctx context.Context, now time.Time, id int64, reconciles int, until time.Time) (epoch int, ok bool, err error)
	// LinkRecords reads what the ledger holds about issueID on tenant.
	LinkRecords(ctx context.Context, tenant, issueID string) (IssueLinkRecords, error)
}

// LinkRowChanges is what one plan writes to the ownership record.
type LinkRowChanges struct {
	// Insert are new open rows, one per added link with no open row.
	Insert []OwnedLink
	// Release are row ids whose link the plan removes: released_op_id is set
	// to the op; the row stays open until a read shows the link gone.
	Release []int64
	// Unrelease are releasing row ids whose link is present and wanted (or
	// kept) again: released_op_id is cleared.
	Unrelease []int64
	// Close are row ids whose link the read shows gone and the plan does not
	// re-add or keep: released_op_id is set to the op if unset and
	// released_at to now.
	Close []int64
}

// PlanIssueLinks is the plan both ledgers make (I1, I2, I6, I7, I14), a pure
// function of the op's desired set, the read, and the open ownership rows
// that name the op's issue on either side (owned). peerEdges are links of the
// issue that another task writes through a live hk edge (the related edges
// where the writer is the higher id), which a ledger that knows edges passes;
// they are left alone like owned peers.
//
// For each link the read shows:
//   - desired: nothing to do (an open releasing row of it is unreleased); a
//     desired link already present with no row is not claimed;
//   - owned by this writer from this issue, not desired: kept when its (edge,
//     target task) is still in the desired or held set, else removed (row
//     releasing);
//   - owned or written by another task: peer;
//   - otherwise: unowned.
//
// For each desired link the read does not show: peer if another task owns
// it, else added; its open row is reused, a releasing row of it is closed and
// a new one inserted. Any other open row of this writer whose link is absent
// is closed, unless its edge is still desired or held.
func PlanIssueLinks(op Op, s IssueLinksSnapshot, cur []IssueLink, owned []OwnedLink, peerEdges []IssueLink) (LinkPlan, LinkRowChanges, error) {
	writer, err := LinkWriter(op)
	if err != nil {
		return LinkPlan{}, LinkRowChanges{}, err
	}
	issue := op.YTTargetID
	plan := LinkPlan{Current: sortedLinks(cur), Add: []IssueLink{}, Remove: []IssueLink{}, Unowned: []IssueLink{}, Peer: []IssueLink{}, Kept: []IssueLink{}}
	var ch LinkRowChanges

	desired := map[IssueLink]DesiredLink{}
	protected := map[[2]any]bool{}
	for _, d := range s.Links() {
		desired[d.Link] = d
		protected[[2]any{d.Edge, d.TaskID}] = true
	}
	for _, h := range s.Held {
		protected[[2]any{h.Kind, h.TaskID}] = true
	}
	mine := map[IssueLink]OwnedLink{}
	peer := linkSet(peerEdges)
	for _, o := range owned {
		if !o.Open() || o.Tenant != op.Tenant {
			continue
		}
		switch {
		case o.From == issue && o.WriterTask == writer:
			l := IssueLink{o.LinkType, o.To}
			if have, dup := mine[l]; dup && have.ID < o.ID {
				// Two open rows of one link: the older is closed below
				// when absent and otherwise counted once.
				continue
			}
			mine[l] = o
		case o.From == issue:
			peer[IssueLink{o.LinkType, o.To}] = true
		case o.To == issue && o.LinkType == LinkRelates:
			// A relates link is seen from both ends; the other end owns it.
			peer[IssueLink{LinkRelates, o.From}] = true
		}
	}
	present := linkSet(plan.Current)
	for _, l := range plan.Current {
		if _, ok := desired[l]; ok {
			if o, ok := mine[l]; ok && o.ReleasedOp != 0 {
				ch.Unrelease = append(ch.Unrelease, o.ID)
			}
			continue
		}
		if o, ok := mine[l]; ok {
			if protected[[2]any{o.Edge, o.TargetTask}] {
				plan.Kept = append(plan.Kept, l)
				if o.ReleasedOp != 0 {
					ch.Unrelease = append(ch.Unrelease, o.ID)
				}
				continue
			}
			plan.Remove = append(plan.Remove, l)
			ch.Release = append(ch.Release, o.ID)
			continue
		}
		if peer[l] {
			plan.Peer = append(plan.Peer, l)
			continue
		}
		plan.Unowned = append(plan.Unowned, l)
	}
	for _, d := range s.Links() {
		if present[d.Link] {
			continue
		}
		o, isMine := mine[d.Link]
		if !isMine && peer[d.Link] {
			plan.Peer = append(plan.Peer, d.Link)
			continue
		}
		plan.Add = append(plan.Add, d.Link)
		if isMine && o.ReleasedOp == 0 {
			continue // an open row from an earlier attempt or op
		}
		if isMine {
			ch.Close = append(ch.Close, o.ID)
		}
		ch.Insert = append(ch.Insert, OwnedLink{Tenant: op.Tenant, LinkType: d.Link.Kind, From: issue, To: d.Link.Target,
			WriterTask: writer, TargetTask: d.TaskID, Edge: d.Edge, IntendedOp: op.ID})
	}
	counted := map[int64]bool{}
	for _, o := range mine {
		counted[o.ID] = true
	}
	for _, o := range owned {
		if !o.Open() || o.Tenant != op.Tenant || o.From != issue || o.WriterTask != writer {
			continue
		}
		l := IssueLink{o.LinkType, o.To}
		if !counted[o.ID] {
			// A second open row of one link: close it when the link is
			// absent; when present the counted row speaks for it.
			if !present[l] {
				ch.Close = append(ch.Close, o.ID)
			}
			continue
		}
		if present[l] {
			continue
		}
		if _, want := desired[l]; want {
			continue // handled above
		}
		if o.ReleasedOp == 0 && protected[[2]any{o.Edge, o.TargetTask}] {
			continue
		}
		ch.Close = append(ch.Close, o.ID)
	}
	for _, list := range []*[]IssueLink{&plan.Add, &plan.Remove, &plan.Unowned, &plan.Peer, &plan.Kept} {
		*list = sortedLinks(*list)
	}
	return plan, ch, nil
}

func sortedLinks(in []IssueLink) []IssueLink {
	out := append([]IssueLink{}, in...)
	SortLinks(out)
	return out
}

// CheckLinkPlanStep is the compare-and-set both ledgers apply before
// PlanLinks writes anything: attempt must be the op's current attempt;
// recover 0 needs the op dispatched and no plan of this attempt yet;
// recover n > 0 needs it remote_unknown, n at most LinkRecoverMax, and
// exactly n-1 recovery plans of this attempt before it. latest is the op's
// newest plan (found false for none). A refusal is ErrState.
func CheckLinkPlanStep(op Op, attempt, recover int, latest LinkPlanRecord, found bool) error {
	if attempt != op.Attempts {
		return fmt.Errorf("%w: plan of attempt %d, op %d is at attempt %d", ErrState, attempt, op.ID, op.Attempts)
	}
	sameAttempt := found && latest.Attempt == attempt
	if recover == 0 {
		if op.State != StateDispatched || sameAttempt {
			return fmt.Errorf("%w: send plan of op %d from %s (planned this attempt: %v)", ErrState, op.ID, op.State, sameAttempt)
		}
		return nil
	}
	prior := 0
	if sameAttempt {
		prior = latest.Recover
	}
	if op.State != StateRemoteUnknown || recover > LinkRecoverMax || recover != prior+1 {
		return fmt.Errorf("%w: recovery plan %d of op %d from %s after %d", ErrState, recover, op.ID, op.State, prior)
	}
	return nil
}

// ClosedByRead is what a read that showed op's plan done closes: the
// writer's releasing ownership rows whose link cur no longer shows, and the
// open unowned rows of op's issue that cur no longer shows (ids, ascending
// as given). SettleLinks (applied) writes both; PlanLinks writes the unowned part (its
// ownership part is PlanIssueLinks's Close).
func ClosedByRead(op Op, owned []OwnedLink, unowned []UnownedLink, cur []IssueLink) (closeOwned, goneUnowned []int64) {
	writer, err := LinkWriter(op)
	if err != nil {
		return nil, nil
	}
	have := linkSet(cur)
	for _, o := range owned {
		if o.Open() && o.ReleasedOp != 0 && o.Tenant == op.Tenant && o.From == op.YTTargetID && o.WriterTask == writer && !have[IssueLink{o.LinkType, o.To}] {
			closeOwned = append(closeOwned, o.ID)
		}
	}
	for _, u := range unowned {
		if u.GoneAt.IsZero() && u.Tenant == op.Tenant && u.IssueID == op.YTTargetID && !have[IssueLink{u.LinkType, u.OtherIssue}] {
			goneUnowned = append(goneUnowned, u.ID)
		}
	}
	return closeOwned, goneUnowned
}
