package ytwrite

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// The planned send and recovery of article_split (hk 5085; design
// 2026-11-18/hk5085-yt-split S1-S9). It follows sendPlanned and
// reconcilePlanned: read, plan committed, each create marked before its
// request, read again, every ledger write fenced to its executor. What is
// the split's own is the rule that keeps one family per document:
//
//   - The index is created only by a plan whose op has no recorded index and
//     no earlier index create that may have been sent (IndexMaybeSent); both
//     ledgers refuse any other (CheckSplitPlan). Once a create may have been
//     sent the index is found by a read, never assumed absent.
//   - "Not sent" (back to prepared, not_sent) is used only while no create
//     of the op may have been sent (AnySent, and nothing before the failing
//     step in this attempt); after that every failure is remote_unknown, a
//     4xx rejected partial_write with the index recorded, and 429/401
//     deferred at the current step (S2, S8, S9).
//   - applied needs a read that shows the index and every part exact; a 2xx
//     without an id is never done (S5).

// sendSplit is send for an article_split op.
func (p *Protocol) sendSplit(ctx context.Context, op Op, k articleSplit) Result {
	sl, ok := p.Ledger.(SplitLedger)
	ar, rok := p.Remote.(ArticleRemote)
	settle := func(f func(context.Context) settled) Result {
		sctx, cancel := p.settling(ctx)
		defer cancel()
		return p.record(op, f(sctx))
	}
	if !ok || !rok {
		return settle(func(c context.Context) settled {
			return p.reject(c, op, ReasonUnsupportedKind, "the ledger or remote cannot split "+op.Kind)
		})
	}
	st := &splitSettler{p: p, sl: sl, f: LinkFence{Attempt: op.Attempts}}
	d, err := renderSplit(op)
	if err != nil {
		return settle(func(c context.Context) settled { return st.reject(c, op, ReasonInvalidIssue, err.Error()) })
	}
	rctx, cancelRemote := p.detached(ctx)
	defer cancelRemote()

	// What earlier attempts did decides what a failure before this attempt's
	// first request means. The first attempt has no earlier one.
	var history []SplitPlanRecord
	if op.Attempts > 1 {
		hctx, cancel := p.settling(ctx)
		history, err = sl.SplitPlans(hctx, op.ID)
		cancel()
		if err != nil {
			return settle(func(c context.Context) settled {
				return st.markUnknown(c, op, ReasonReconcileFailed, "plans: "+err.Error())
			})
		}
	}
	sent := AnySent(history)
	// early settles a failure before this attempt sent anything.
	early := func(err error) Result {
		var refused *RefusedError
		var hold *HoldError
		switch {
		case errors.As(err, &hold):
			return settle(func(c context.Context) settled { return st.markUnknown(c, op, hold.Reason, hold.Detail) })
		case sent:
			return settle(func(c context.Context) settled { return st.markUnknown(c, op, ReasonReconcileFailed, err.Error()) })
		case errors.As(err, &refused):
			return settle(func(c context.Context) settled { return st.reject(c, op, refused.Reason, refused.Detail) })
		}
		var ns *NotSentError
		if !errors.As(err, &ns) {
			ns = &NotSentError{Err: err}
		}
		return settle(func(c context.Context) settled { return p.notSent(c, st, op, ns) })
	}

	// 1. Read: nothing is sent yet.
	read, err := k.readFamily(rctx, ar, op, d, op.YTResultID, IndexMaybeSent(history))
	if err != nil {
		return early(err)
	}
	if read.index == "" && IndexMaybeSent(history) {
		return early(&HoldError{Reason: ReasonIndexNotFound, Detail: "an index create of this op may have been sent and no read finds the index"})
	}
	if read.complete() {
		return settle(func(c context.Context) settled { return st.apply(c, op, read.index) })
	}
	parent := ""
	if read.index == "" {
		// The index's parent, read only: a split never creates a family
		// root. A root POST whose outcome is unknown could land after a
		// later search and leave the family with two roots (review of PR
		// #511), so the split waits, not sent, until the family has one
		// (article_create makes it).
		parent, err = k.splitParent(rctx, ar, op, d)
		if err != nil {
			return early(err)
		}
	}

	// 2. Plan, committed before any create.
	plan := planFamily(read)
	sctx, cancel := p.settling(ctx)
	err = sl.PlanSplit(sctx, p.now(), op.ID, st.f, plan)
	cancel()
	if err != nil {
		if errors.Is(err, ErrState) {
			// Stale recovery took the op, or the ledger refused a second
			// index: either way nothing is sent from here.
			return p.record(op, settled{err: err})
		}
		return early(fmt.Errorf("plan not recorded: %w", err))
	}

	// 3. Execute, each create marked first.
	n, index, markErr, err := p.executeSplit(ctx, rctx, st, k, ar, op, d, parent, plan)
	st.index = index
	if err != nil {
		if markErr != nil {
			if errors.Is(markErr, ErrState) {
				return p.record(op, settled{err: markErr})
			}
			// Step n was not sent; every step before it succeeded.
			st.stopped, st.step = true, n
			if n == 0 && !sent {
				return settle(func(c context.Context) settled {
					return p.deferNotSent(c, st, op, p.reconcileDelay(op.Attempts), ReasonNotSent, "step not marked: "+markErr.Error())
				})
			}
			if n > 0 {
				st.outcome = StepOK
			}
			return settle(func(c context.Context) settled {
				return st.markUnknown(c, op, ReasonPartialWrite, "step not marked: "+markErr.Error())
			})
		}
		st.outcome = splitStepOutcome(err)
		return settle(func(c context.Context) settled { return st.failed(c, op, plan, n, sent || n > 0, err) })
	}
	st.outcome = StepOK

	// 4. Read again: applied only when the whole family reads back (S5).
	final, readErr := k.readFamily(rctx, ar, op, d, index, false)
	return settle(func(c context.Context) settled {
		var hold *HoldError
		switch {
		case errors.As(readErr, &hold):
			return st.markUnknown(c, op, hold.Reason, hold.Detail)
		case readErr != nil:
			return st.markUnknown(c, op, ReasonUnverified, "final read: "+readErr.Error())
		case !final.complete():
			return st.markUnknown(c, op, ReasonPartialWrite, "the read after the plan does not show every part")
		}
		return st.apply(c, op, final.index)
	})
}

// executeSplit runs k.executeSplit for st's executor, marking each create in
// the ledger before its request. markErr is the mark that stopped it.
func (p *Protocol) executeSplit(ctx, rctx context.Context, st *splitSettler, k articleSplit, ar ArticleRemote, op Op, d splitDoc, parent string, plan SplitPlan) (n int, index string, markErr, err error) {
	mark := func(i int, index string) error {
		mctx, cancel := p.settling(ctx)
		defer cancel()
		if err := st.sl.MarkSplitStep(mctx, p.now(), op.ID, st.f, i, index); err != nil {
			markErr = err
			return err
		}
		return nil
	}
	n, index, err = k.executeSplit(rctx, ar, op, d, parent, plan, mark)
	return n, index, markErr, err
}

// splitStepOutcome is the outcome of the create a request error came from:
// definite when the request provably was not processed (a 4xx other than
// 408, which includes 429 and 401, or a failure before it left), ambiguous
// otherwise.
func splitStepOutcome(err error) StepOutcome {
	if beforeSend(err) {
		return StepDefinite
	}
	return stepOutcome(err)
}

// reconcileSplit is reconcile for an article_split op: a fenced recovery pass
// (S6). It reads the family; applies when the read shows it whole; otherwise,
// while fewer than LinkRecoverMax recovery plans were made, plans the missing
// creates from the read, executes them and reads again. A latest plan whose
// in-flight create has no recorded outcome makes no request unless the read
// shows that create (S7); an index whose create may have been sent and that no
// read finds makes none either (S1).
func (p *Protocol) reconcileSplit(ctx context.Context, op Op, k articleSplit) (bool, error) {
	sl, ok := p.Ledger.(SplitLedger)
	ar, rok := p.Remote.(ArticleRemote)
	if !ok || !rok {
		return false, p.touchUnknown(ctx, op, 0, ReasonReconcileFailed, "the ledger or remote cannot split "+op.Kind)
	}
	// The lease, its deadline and its epoch are reconcilePlanned's.
	start := p.now()
	wctx, cancelWork := context.WithTimeout(context.WithoutCancel(ctx), p.DispatchTimeout+p.SettleTimeout)
	defer cancelWork()
	epoch, leased, err := sl.LeaseLinkRecovery(wctx, start, op.ID, op.Reconciles, start.Add(p.linkRecoveryLease()))
	if err != nil || !leased {
		return false, err
	}
	if late := p.now().Sub(start); late > p.SettleTimeout {
		log.Printf("ytwrite.split_lease_late op_id=%d after=%q", op.ID, late)
		return false, nil
	}
	ctx = wctx
	st := &splitSettler{p: p, sl: sl, f: LinkFence{Attempt: op.Attempts, Epoch: epoch}, index: op.YTResultID}
	end := func(s settled) (bool, error) {
		if s.err != nil {
			return false, s.err
		}
		logTransition(s.op)
		return s.op.State == StateApplied, nil
	}
	pass := func(wait time.Duration, reason, detail string) (bool, error) {
		return end(st.pass(ctx, op, wait, reason, detail))
	}
	apply := func(index string) (bool, error) {
		s := st.apply(ctx, op, index)
		if errors.Is(s.err, ErrState) {
			return false, nil
		}
		return end(s)
	}
	d, err := renderSplit(op)
	if err != nil {
		return pass(0, ReasonReconcileFailed, err.Error())
	}
	history, err := sl.SplitPlans(ctx, op.ID)
	if err != nil {
		return pass(0, ReasonReconcileFailed, err.Error())
	}
	latest, found := LatestSplitPlan(history)
	recovers := 0
	if found && latest.Attempt == op.Attempts {
		recovers = latest.Recover
		st.f.Recover = latest.Recover
	} else {
		found = false
	}
	rctx, cancelRemote := context.WithTimeout(wctx, p.DispatchTimeout)
	defer cancelRemote()
	// failed classifies a recovery request's error, as reconcilePlanned's
	// does, except that a 4xx after the index exists rejects partial_write
	// (S9).
	failed := func(err error) (bool, error) {
		var hold *HoldError
		var re *RemoteError
		switch {
		case errors.As(err, &hold):
			return pass(0, hold.Reason, hold.Detail)
		case errors.Is(err, errSplitNoID):
			return pass(0, ReasonNoID, err.Error())
		case errors.As(err, &re) && re.Status == http.StatusTooManyRequests:
			wait := re.RetryAfter
			if wait <= 0 {
				wait = p.RateLimitWait
			}
			return pass(wait, ReasonRateLimited, re.Error())
		case errors.As(err, &re) && re.Status == http.StatusUnauthorized:
			return pass(p.AuthWait, ReasonUnauthorized, re.Error())
		}
		return pass(0, ReasonReconcileFailed, err.Error())
	}
	read, err := k.readFamily(rctx, ar, op, d, op.YTResultID, IndexMaybeSent(history))
	if err != nil {
		var re *RemoteError
		if errors.As(err, &re) && re.Status >= 400 && re.Status < 500 && re.Status != http.StatusTooManyRequests && re.Status != http.StatusUnauthorized {
			// A read refused: nothing to act on; the operator looks.
			return pass(0, "http_"+strconv.Itoa(re.Status), err.Error())
		}
		return failed(err)
	}
	if read.complete() {
		return apply(read.index)
	}
	if read.index == "" && IndexMaybeSent(history) {
		return pass(0, ReasonIndexNotFound, "an index create of this op may have been sent and no read finds the index")
	}
	if found && latest.Uncertain() && !read.has(latest.InFlight()) {
		// The create may still land: absence in this read is not proof it
		// failed, so nothing is created beside it until the read shows it or
		// an operator resolves the op (S7; review of PR #511).
		reason, what := ReasonOutcomeLost, "its outcome was never recorded"
		if latest.Outcome == StepAmbiguous {
			reason, what = ReasonOutcomeAmbiguous, "its outcome was ambiguous"
		}
		log.Printf("ytwrite.split_outcome_uncertain op_id=%d attempt=%d recover=%d step=%d reason=%s", op.ID, latest.Attempt, latest.Recover, latest.Marked-1, reason)
		return pass(0, reason, fmt.Sprintf("plan %d.%d sent the create of %s and %s; the read does not show it, so nothing is created beside it until it appears or an operator resolves it",
			latest.Attempt, latest.Recover, splitStepName(latest.InFlight(), d.parts), what))
	}
	if recovers >= LinkRecoverMax {
		log.Printf("ytwrite.split_recover_exhausted op_id=%d recovers=%d", op.ID, recovers)
		return pass(0, ReasonRecoverExhausted, fmt.Sprintf("%d recovery plans did not converge; an operator resolves it", recovers))
	}
	parent := ""
	if read.index == "" {
		parent, err = k.splitParent(rctx, ar, op, d)
		var ns *NotSentError
		if errors.As(err, &ns) {
			return pass(0, ReasonParentNotFound, err.Error())
		}
		if err != nil {
			return failed(err)
		}
	}
	st.f.Recover = recovers + 1
	plan := planFamily(read)
	if err := sl.PlanSplit(ctx, p.now(), op.ID, st.f, plan); err != nil {
		if errors.Is(err, ErrState) {
			return false, nil // resolved or recovered elsewhere meanwhile
		}
		return pass(0, ReasonReconcileFailed, "plan not recorded: "+err.Error())
	}
	n, index, markErr, err := p.executeSplit(ctx, rctx, st, k, ar, op, d, parent, plan)
	if index != "" {
		st.index = index
	}
	if err != nil {
		if markErr != nil {
			if errors.Is(markErr, ErrState) {
				return false, nil
			}
			st.stopped, st.step = true, n
			if n > 0 {
				st.outcome = StepOK
			}
			return pass(0, ReasonReconcileFailed, "step not marked: "+markErr.Error())
		}
		st.outcome = splitStepOutcome(err)
		var re *RemoteError
		if errors.As(err, &re) && re.Status >= 400 && re.Status < 500 && re.Status != http.StatusRequestTimeout &&
			re.Status != http.StatusTooManyRequests && re.Status != http.StatusUnauthorized {
			if st.index != "" {
				return end(st.reject(ctx, op, ReasonPartialWrite, st.rejectDetail(plan, n, err, d.parts)))
			}
			return end(st.reject(ctx, op, "http_"+strconv.Itoa(re.Status), re.Error()))
		}
		return failed(err)
	}
	st.outcome = StepOK
	final, err := k.readFamily(rctx, ar, op, d, index, false)
	if err != nil {
		return failed(err)
	}
	if !final.complete() {
		return pass(0, ReasonPartialWrite, fmt.Sprintf("recovery plan %d: the read after it does not show every part", recovers+1))
	}
	return apply(final.index)
}

// resolveSplit is Resolve for an article_split op. Applied names the index
// (the recorded one, when the op recorded one) and publishes the binding
// through Apply, unless a recovery pass holds the op's lease; not applied
// rejects the op through the fenced settlement, leaving the family as it is.
func (p *Protocol) resolveSplit(ctx context.Context, op Op, r Resolution) (Result, error) {
	sl, ok := p.Ledger.(SplitLedger)
	if !ok {
		return Result{}, fmt.Errorf("%w: the ledger cannot settle %s", ErrInvalid, op.Kind)
	}
	if r.Adopted != nil && *r.Adopted {
		return Result{}, fmt.Errorf("%w: %s creates its family; it adopts nothing", ErrInvalid, op.Kind)
	}
	if r.Applied && !resultIDRE.MatchString(r.YTID) {
		return Result{}, fmt.Errorf("%w: youtrack id is empty or malformed", ErrInvalid)
	}
	// op is the operator's first read (Resolve's ByID). A recovery pass that
	// holds it then may still be sending; one leased after it moves its
	// epoch, which the ledger compares under the op's lock.
	if op.Reason == ReasonLinkRecovery && op.NextAttemptAt.After(p.now()) {
		return Result{}, fmt.Errorf("%w: a recovery pass holds op %d until %s", ErrState, op.ID, op.NextAttemptAt.Format(time.RFC3339))
	}
	history, err := sl.SplitPlans(ctx, op.ID)
	if err != nil {
		return Result{}, err
	}
	latest, found := LatestSplitPlan(history)
	st := &splitSettler{p: p, sl: sl, f: LinkFence{Attempt: op.Attempts, Epoch: EpochOperator}, outcome: StepDefinite}
	if found && latest.Attempt == op.Attempts {
		st.f.Recover = latest.Recover
	}
	saw := LinkSettlement{SawPlan: found, SawAttempt: latest.Attempt, SawRec: latest.Recover, SawMarked: latest.Marked, SawOutcome: latest.Outcome,
		SawReconciles: op.Reconciles}
	if r.Applied {
		// Fenced like the rejection: the ledger refuses it under the op's
		// lock if a recovery pass holds the lease, the op recorded another
		// index, or a plan was made or a create marked after this read.
		saw.YTID = r.YTID
		got, err := sl.ApplySplit(ctx, p.now(), op.ID, st.f, saw, articleSplit{}.MapRows(op, r.YTID))
		if err != nil {
			return Result{}, err
		}
		logTransition(got)
		return p.result(got, false), nil
	}
	ls := saw
	ls.To, ls.Reason, ls.Detail = StateRejected, ReasonOperatorNotDone, "by "+r.By+": "+r.Note
	s := st.settle(ctx, op, ls)
	if s.err != nil {
		return Result{}, s.err
	}
	logTransition(s.op)
	return p.result(s.op, false), nil
}

func splitStepName(step, parts int) string {
	if step == 0 {
		return "the index"
	}
	return fmt.Sprintf("part %d of %d", step, parts)
}

// splitSettler settles a split op for one executor (f) through the fenced
// SettleSplit, carrying the outcome of its in-flight create and the index id
// it knows. It is a settler, so writeFailed and notSent settle through it.
type splitSettler struct {
	p       *Protocol
	sl      SplitLedger
	f       LinkFence
	outcome StepOutcome
	stopped bool
	step    int
	index   string
}

var _ settler = (*splitSettler)(nil)

func (s *splitSettler) settle(ctx context.Context, op Op, ls LinkSettlement) settled {
	ls.Outcome, ls.Stopped, ls.Step = s.outcome, s.stopped, s.step
	if ls.YTID == "" {
		ls.YTID = s.index
	}
	got, err := s.sl.SettleSplit(ctx, s.p.now(), op.ID, s.f, ls)
	return settled{got, err}
}

// apply applies the op with index through the fenced ApplySplit, which
// publishes the binding (S10).
func (s *splitSettler) apply(ctx context.Context, op Op, index string) settled {
	got, err := s.sl.ApplySplit(ctx, s.p.now(), op.ID, s.f, LinkSettlement{YTID: index}, articleSplit{}.MapRows(op, index))
	if errors.Is(err, ErrConflictingResult) {
		log.Printf("ytwrite.apply_conflict_failed op_id=%d yt_id=%q", op.ID, index)
	}
	return settled{got, err}
}

func (s *splitSettler) reject(ctx context.Context, op Op, reason, detail string) settled {
	return s.settle(ctx, op, LinkSettlement{To: StateRejected, Reason: reason, Detail: detail})
}

func (s *splitSettler) deferOp(ctx context.Context, op Op, next time.Time, reason, detail string) settled {
	return s.settle(ctx, op, LinkSettlement{To: StatePrepared, Next: next, Reason: reason, Detail: detail})
}

func (s *splitSettler) markUnknown(ctx context.Context, op Op, reason, detail string) settled {
	return s.settle(ctx, op, LinkSettlement{To: StateRemoteUnknown, Next: s.p.now().Add(s.p.reconcileDelay(0)), Reason: reason, Detail: detail})
}

// pass ends a recovery pass that did not resolve the op (linkSettler.pass).
func (s *splitSettler) pass(ctx context.Context, op Op, wait time.Duration, reason, detail string) settled {
	delay := max(s.p.reconcileDelay(s.f.Epoch), min(wait, s.p.MaxRetryAfter))
	if s.f.Epoch >= 50 {
		log.Printf("ytwrite.reconcile_warning op_id=%d reconciles=%d next_after=%q reason=%q", op.ID, s.f.Epoch, delay, reason)
	}
	return s.settle(ctx, op, LinkSettlement{To: StateRemoteUnknown, Next: s.p.now().Add(delay), Reason: reason, Detail: detail})
}

func (s *splitSettler) rejectDetail(plan SplitPlan, n int, err error, parts int) string {
	if s.index == "" {
		return err.Error()
	}
	return fmt.Sprintf("index %s exists; the create of %s was refused: %s; an operator resolves the family", s.index, splitStepName(plan.Steps[n], parts), err.Error())
}

// failed settles a send whose create n failed with err (sendSplit step 3).
// sent says a create of the op may already have been sent before it.
func (s *splitSettler) failed(ctx context.Context, op Op, plan SplitPlan, n int, sent bool, err error) settled {
	p := s.p
	var re *RemoteError
	switch {
	case errors.Is(err, errSplitNoID):
		return s.markUnknown(ctx, op, ReasonNoID, fmt.Sprintf("the create of %s: %s", splitStepName(plan.Steps[n], plan.Parts), err.Error()))
	case errors.As(err, &re) && (re.Status == http.StatusTooManyRequests || re.Status == http.StatusUnauthorized):
		// Not processed: deferred at this step, resumed from a read (S8).
		return p.writeFailed(ctx, s, op, err)
	case errors.As(err, &re) && re.Status >= 400 && re.Status < 500 && re.Status != http.StatusRequestTimeout:
		if s.index != "" {
			return s.reject(ctx, op, ReasonPartialWrite, s.rejectDetail(plan, n, err, plan.Parts))
		}
		return p.writeFailed(ctx, s, op, err)
	case beforeSend(err) && !sent:
		return p.writeFailed(ctx, s, op, err)
	case beforeSend(err):
		// Nothing of this request left, but an earlier create did: the op
		// is never "not sent" again (S2); recovery continues from a read.
		return s.markUnknown(ctx, op, ReasonTransport, fmt.Sprintf("the create of %s: %s", splitStepName(plan.Steps[n], plan.Parts), err.Error()))
	case errors.As(err, &re):
		return s.markUnknown(ctx, op, ReasonServerError, re.Error())
	}
	return s.markUnknown(ctx, op, ReasonTransport, err.Error())
}
