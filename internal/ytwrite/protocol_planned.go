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

// sendPlanned is send for a PlannedKind (I5, I6, I8, I12): read, plan in the
// ledger, execute the plan's diff, read again. Nothing is resent from a stored
// request: each step works from the read just made. The remote steps share one
// DispatchTimeout window, so a send is never in flight past StaleAfter, when
// stale recovery could start a second executor of the same op. Every ledger
// write is fenced to this dispatch attempt (LinkFence epoch 0): once stale
// recovery has taken the op, they change nothing.
func (p *Protocol) sendPlanned(ctx context.Context, op Op, kind PlannedKind) Result {
	ll, ok := p.Ledger.(LinksLedger)
	if !ok {
		sctx, cancel := p.settling(ctx)
		defer cancel()
		return p.record(op, p.reject(sctx, op, ReasonUnsupportedKind, "the ledger cannot plan "+op.Kind))
	}
	rctx, cancelRemote := p.detached(ctx)
	defer cancelRemote()
	st := &linkSettler{p: p, ll: ll, f: LinkFence{Attempt: op.Attempts}}
	settle := func(f func(context.Context) settled) Result {
		sctx, cancel := p.settling(ctx)
		defer cancel()
		return p.record(op, f(sctx))
	}

	// 1. Read: nothing is sent yet.
	read, err := kind.Read(rctx, p.Remote, op)
	if err != nil {
		var refused *RefusedError
		if errors.As(err, &refused) {
			return settle(func(c context.Context) settled { return st.reject(c, op, refused.Reason, refused.Detail) })
		}
		return settle(func(c context.Context) settled { return p.notSent(c, st, op, &NotSentError{Err: err}) })
	}

	// 2. Plan, committed before any mutation (I6).
	sctx, cancel := p.settling(ctx)
	plan, err := ll.PlanLinks(sctx, p.now(), op.ID, st.f, read.Links)
	cancel()
	if err != nil {
		if errors.Is(err, ErrState) {
			// The op left this dispatch (stale recovery took it): it is
			// recovered there, from its own read.
			return p.record(op, settled{err: err})
		}
		// Nothing was sent. A plan that did commit is superseded by the next
		// attempt's, which first closes ownership of its adds (none sent).
		return settle(func(c context.Context) settled {
			return st.deferOp(c, op, p.now().Add(p.retryDelay(max(op.Attempts-1, 0))), ReasonNotSent, "plan not recorded: "+err.Error())
		})
	}

	// 3. Execute the diff (I12 order), each step marked in flight first.
	n, markErr, err := p.executePlanned(ctx, rctx, st, kind, op, read.Types, plan)
	if err != nil {
		if markErr != nil {
			if errors.Is(markErr, ErrState) {
				return p.record(op, settled{err: markErr})
			}
			// Step n was not sent (its mark may still have committed);
			// every step before it succeeded.
			st.stopped, st.step = true, n
			if n > 0 {
				st.outcome = StepOK
				return settle(func(c context.Context) settled {
					return st.markUnknown(c, op, ReasonPartialWrite, "step not marked: "+markErr.Error())
				})
			}
			return settle(func(c context.Context) settled {
				return st.deferOp(c, op, p.now().Add(p.retryDelay(max(op.Attempts-1, 0))), ReasonNotSent, "step not marked: "+markErr.Error())
			})
		}
		st.outcome = stepOutcome(err)
		var partial *PartialWriteError
		var refused *RefusedError
		switch {
		case n > 0 || errors.As(err, &partial):
			return settle(func(c context.Context) settled { return st.markUnknown(c, op, ReasonPartialWrite, err.Error()) })
		case errors.As(err, &refused):
			return settle(func(c context.Context) settled { return st.reject(c, op, refused.Reason, refused.Detail) })
		}
		return settle(func(c context.Context) settled { return p.writeFailed(c, st, op, err) })
	}
	st.outcome = StepOK

	// 4. Read again; applied only when the read shows the plan done.
	cur, readErr := read.Links, error(nil)
	if n > 0 {
		var final IssueLinksRead
		final, readErr = kind.Read(rctx, p.Remote, op)
		cur = final.Links
	}
	return settle(func(c context.Context) settled {
		if readErr != nil {
			return st.markUnknown(c, op, ReasonUnverified, "final read: "+readErr.Error())
		}
		done, err := kind.Converged(op, plan, cur)
		if err != nil {
			return st.markUnknown(c, op, ReasonUnverified, err.Error())
		}
		if !done {
			return st.markUnknown(c, op, ReasonPartialWrite, "the read after the plan does not show it done")
		}
		return st.apply(c, op, cur)
	})
}

// executePlanned runs kind.Execute for st's executor, marking each step in
// the ledger before its request (on a settling context of ctx). markErr is the
// mark that stopped it, if one did.
func (p *Protocol) executePlanned(ctx, rctx context.Context, st *linkSettler, kind PlannedKind, op Op, types LinkTypes, plan LinkPlan) (n int, markErr, err error) {
	mark := func(i int) error {
		mctx, cancel := p.settling(ctx)
		defer cancel()
		if err := st.ll.MarkLinkStep(mctx, p.now(), op.ID, st.f, i); err != nil {
			markErr = err
			return err
		}
		return nil
	}
	n, err = kind.Execute(rctx, p.Remote, op, types, plan, mark)
	return n, markErr, err
}

// reconcilePlanned is reconcile for a PlannedKind (I5, I8, I9). It replaces
// the evidence search: read; applied when the read shows this attempt's
// latest plan done; otherwise, while fewer than LinkRecoverMax recovery plans
// were made, a new plan from this read, its diff executed, and a read again.
// A recovery request answered with a 4xx other than 401, 408 or 429 rejects
// the op (terminal, the lane moves on); 429 and 401 wait; anything else is
// tried again after the reconcile backoff. An exhausted op, or one whose
// latest plan lost the outcome of an add (ReasonOutcomeLost), makes no
// request.
func (p *Protocol) reconcilePlanned(ctx context.Context, op Op, kind PlannedKind) (bool, error) {
	ll, ok := p.Ledger.(LinksLedger)
	if !ok {
		return false, p.touchUnknown(ctx, op, 0, ReasonReconcileFailed, "the ledger cannot plan "+op.Kind)
	}
	// One recovery at a time per op (review of PR #425): Drive reconciles a
	// remote_unknown op without a claim, and a recovery mutates. The lease
	// is a compare-and-set on the op's due time and reconcile count, taken
	// before anything else this pass reads or writes, and it issues the
	// pass's epoch: every later ledger write of the pass is fenced by it, so
	// a pass that was superseded changes nothing. The pass's work (lease
	// call included) runs under one deadline set before the lease call, a
	// SettleTimeout before the lease ends, so no later holder starts while
	// this one can still act; a lease call that returned late starts no work.
	start := p.now()
	wctx, cancelWork := context.WithTimeout(context.WithoutCancel(ctx), p.DispatchTimeout+p.SettleTimeout)
	defer cancelWork()
	epoch, leased, err := ll.LeaseLinkRecovery(wctx, start, op.ID, op.Reconciles, start.Add(p.linkRecoveryLease()))
	if err != nil || !leased {
		return false, err
	}
	if late := p.now().Sub(start); late > p.SettleTimeout {
		log.Printf("ytwrite.links_lease_late op_id=%d after=%q", op.ID, late)
		return false, nil
	}
	ctx = wctx
	st := &linkSettler{p: p, ll: ll, f: LinkFence{Attempt: op.Attempts, Epoch: epoch}}
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
	latest, found, err := ll.LatestLinkPlan(ctx, op.ID)
	if err != nil {
		return pass(0, ReasonReconcileFailed, err.Error())
	}
	// Only a plan of the op's last dispatch describes what it may have sent;
	// an earlier attempt ended deferred, settled with its outcome.
	if found && latest.Attempt != op.Attempts {
		found = false
	}
	recovers := 0
	if found {
		recovers = latest.Recover
		st.f.Recover = latest.Recover
	}
	if recovers >= LinkRecoverMax {
		log.Printf("ytwrite.links_recover_exhausted op_id=%d recovers=%d", op.ID, recovers)
		return pass(0, ReasonRecoverExhausted, fmt.Sprintf("%d recovery plans did not converge; an operator resolves it", recovers))
	}
	if found && latest.LostAdd() {
		step := ExecuteOrder(latest.Plan)[latest.Marked-1]
		log.Printf("ytwrite.links_outcome_lost op_id=%d attempt=%d recover=%d step=%d", op.ID, latest.Attempt, latest.Recover, latest.Marked-1)
		return pass(0, ReasonOutcomeLost, fmt.Sprintf("plan %d.%d sent add %s %s and its outcome was never recorded; an operator resolves it (applied: it landed; not done: it did not)",
			latest.Attempt, latest.Recover, step.Link.Kind, step.Link.Target))
	}
	rctx, cancelRemote := context.WithTimeout(wctx, p.DispatchTimeout)
	defer cancelRemote()
	// failed classifies a recovery request's error.
	failed := func(err error) (bool, error) {
		var refused *RefusedError
		var re *RemoteError
		switch {
		case errors.As(err, &refused):
			return end(st.reject(ctx, op, refused.Reason, refused.Detail))
		case errors.As(err, &re) && re.Status == http.StatusTooManyRequests:
			wait := re.RetryAfter
			if wait <= 0 {
				wait = p.RateLimitWait
			}
			return pass(wait, ReasonRateLimited, re.Error())
		case errors.As(err, &re) && re.Status == http.StatusUnauthorized:
			return pass(p.AuthWait, ReasonUnauthorized, re.Error())
		case errors.As(err, &re) && re.Status >= 400 && re.Status < 500 && re.Status != http.StatusRequestTimeout:
			return end(st.reject(ctx, op, "http_"+strconv.Itoa(re.Status), re.Error()))
		}
		return pass(0, ReasonReconcileFailed, err.Error())
	}
	read, err := kind.Read(rctx, p.Remote, op)
	if err != nil {
		return failed(err)
	}
	if found {
		done, err := kind.Converged(op, latest.Plan, read.Links)
		if err != nil {
			return pass(0, ReasonReconcileFailed, err.Error())
		}
		if done {
			return end(st.apply(ctx, op, read.Links))
		}
	}
	st.f.Recover = recovers + 1
	plan, err := ll.PlanLinks(ctx, p.now(), op.ID, st.f, read.Links)
	if errors.Is(err, ErrState) {
		return false, nil // resolved or recovered elsewhere meanwhile
	}
	if err != nil {
		return pass(0, ReasonReconcileFailed, "plan not recorded: "+err.Error())
	}
	n, markErr, err := p.executePlanned(ctx, rctx, st, kind, op, read.Types, plan)
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
		st.outcome = stepOutcome(err)
		var partial *PartialWriteError
		if errors.As(err, &partial) {
			err = partial.Err
		}
		return failed(err)
	}
	st.outcome = StepOK
	cur := read.Links
	if n > 0 {
		final, err := kind.Read(rctx, p.Remote, op)
		if err != nil {
			return failed(err)
		}
		cur = final.Links
	}
	done, err := kind.Converged(op, plan, cur)
	if err != nil {
		return pass(0, ReasonReconcileFailed, err.Error())
	}
	if !done {
		return pass(0, ReasonPartialWrite, fmt.Sprintf("recovery plan %d: the read after it does not show it done", recovers+1))
	}
	return end(st.apply(ctx, op, cur))
}

// resolvePlanned is Resolve for a planned kind: the operator's word settles
// the latest plan's in-flight step (applied: it landed; not done: it did
// not, so its add's ownership closes) through the same fenced settlement.
func (p *Protocol) resolvePlanned(ctx context.Context, op Op, r Resolution) (Result, error) {
	ll, ok := p.Ledger.(LinksLedger)
	if !ok {
		return Result{}, fmt.Errorf("%w: the ledger cannot settle %s", ErrInvalid, op.Kind)
	}
	if r.Adopted != nil {
		return Result{}, fmt.Errorf("%w: %s creates nothing to adopt", ErrInvalid, op.Kind)
	}
	if r.Applied && r.YTID != op.YTTargetID {
		return Result{}, fmt.Errorf("%w: op %d applies to its own issue %s", ErrInvalid, op.ID, op.YTTargetID)
	}
	latest, found, err := ll.LatestLinkPlan(ctx, op.ID)
	if err != nil {
		return Result{}, err
	}
	st := &linkSettler{p: p, ll: ll, f: LinkFence{Attempt: op.Attempts, Epoch: EpochOperator}}
	if found && latest.Attempt == op.Attempts {
		st.f.Recover = latest.Recover
	}
	var s settled
	if r.Applied {
		st.outcome = StepOK
		s = st.settle(ctx, op, LinkSettlement{To: StateApplied, YTID: op.YTTargetID})
	} else {
		st.outcome = StepDefinite
		s = st.reject(ctx, op, ReasonOperatorNotDone, "by "+r.By+": "+r.Note)
	}
	if s.err != nil {
		return Result{}, s.err
	}
	logTransition(s.op)
	return p.result(s.op, false), nil
}

// linkSettler settles a planned op for one executor (f) through the fenced
// SettleLinks, carrying the outcome of the executor's in-flight step. It is a
// settler, so the generic failure classification (writeFailed, notSent)
// settles through it.
type linkSettler struct {
	p       *Protocol
	ll      LinksLedger
	f       LinkFence
	outcome StepOutcome
	// stopped, step: Execute stopped before sending step (a failed mark).
	stopped bool
	step    int
}

var _ settler = (*linkSettler)(nil)

func (s *linkSettler) settle(ctx context.Context, op Op, ls LinkSettlement) settled {
	ls.Outcome, ls.Stopped, ls.Step = s.outcome, s.stopped, s.step
	got, err := s.ll.SettleLinks(ctx, s.p.now(), op.ID, s.f, ls)
	return settled{got, err}
}

func (s *linkSettler) reject(ctx context.Context, op Op, reason, detail string) settled {
	return s.settle(ctx, op, LinkSettlement{To: StateRejected, Reason: reason, Detail: detail})
}

func (s *linkSettler) deferOp(ctx context.Context, op Op, next time.Time, reason, detail string) settled {
	return s.settle(ctx, op, LinkSettlement{To: StatePrepared, Next: next, Reason: reason, Detail: detail})
}

func (s *linkSettler) markUnknown(ctx context.Context, op Op, reason, detail string) settled {
	return s.settle(ctx, op, LinkSettlement{To: StateRemoteUnknown, Next: s.p.now().Add(s.p.reconcileDelay(0)), Reason: reason, Detail: detail})
}

func (s *linkSettler) apply(ctx context.Context, op Op, cur []IssueLink) settled {
	return s.settle(ctx, op, LinkSettlement{To: StateApplied, Read: true, Cur: cur, YTID: op.YTTargetID})
}

// pass ends a recovery pass that did not resolve the op: due again after the
// reconcile backoff of its count (the epoch: the lease counted the pass), or
// after wait (a 429's Retry-After, a 401's AuthWait) when that is longer.
func (s *linkSettler) pass(ctx context.Context, op Op, wait time.Duration, reason, detail string) settled {
	now := s.p.now()
	delay := max(s.p.reconcileDelay(s.f.Epoch), min(wait, s.p.MaxRetryAfter))
	if s.f.Epoch >= 50 {
		log.Printf("ytwrite.reconcile_warning op_id=%d reconciles=%d next_after=%q reason=%q", op.ID, s.f.Epoch, delay, reason)
	}
	return s.settle(ctx, op, LinkSettlement{To: StateRemoteUnknown, Next: now.Add(delay), Reason: reason, Detail: detail})
}

// linkRecoveryLease is how long one recovery pass holds its op: its remote
// steps (DispatchTimeout), its ledger steps (SettleTimeout), and a
// SettleTimeout of margin for a commit whose reply was lost.
func (p *Protocol) linkRecoveryLease() time.Duration {
	return p.DispatchTimeout + 2*p.SettleTimeout
}

// stepOutcome is the outcome of the step a request error came from: definite
// for a refusal before sending or a 4xx other than 408 (not processed),
// ambiguous otherwise (it may have landed).
func stepOutcome(err error) StepOutcome {
	var refused *RefusedError
	var re *RemoteError
	switch {
	case errors.As(err, &refused):
		return StepDefinite
	case errors.As(err, &re) && re.Status >= 400 && re.Status < 500 && re.Status != http.StatusRequestTimeout:
		return StepDefinite
	}
	return StepAmbiguous
}
