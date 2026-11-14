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
// stale recovery could start a second executor of the same op.
func (p *Protocol) sendPlanned(ctx context.Context, op Op, kind PlannedKind) Result {
	ll, ok := p.Ledger.(LinksLedger)
	if !ok {
		sctx, cancel := p.settling(ctx)
		defer cancel()
		return p.record(op, p.reject(sctx, op, ReasonUnsupportedKind, "the ledger cannot plan "+op.Kind))
	}
	rctx, cancelRemote := p.detached(ctx)
	defer cancelRemote()
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
			return settle(func(c context.Context) settled { return p.reject(c, op, refused.Reason, refused.Detail) })
		}
		return settle(func(c context.Context) settled { return p.notSent(c, op, &NotSentError{Err: err}) })
	}

	// 2. Plan, committed before any mutation (I6).
	sctx, cancel := p.settling(ctx)
	plan, err := ll.PlanLinks(sctx, p.now(), op.ID, op.Attempts, 0, read.Links)
	cancel()
	if err != nil {
		if errors.Is(err, ErrState) {
			// The op left this dispatch (stale recovery took it): it is
			// recovered there, from its own read.
			return p.record(op, settled{err: err})
		}
		// Nothing was sent. A plan that did commit is superseded by the next
		// attempt's: an add it recorded is reused, a release it recorded is
		// re-planned.
		return settle(func(c context.Context) settled {
			return p.deferOp(c, op, p.now().Add(p.retryDelay(max(op.Attempts-1, 0))), ReasonNotSent, "plan not recorded: "+err.Error())
		})
	}

	// 3. Execute the diff (I12 order).
	n, err := kind.Execute(rctx, p.Remote, op, read.Types, plan)
	if err != nil {
		var partial *PartialWriteError
		var refused *RefusedError
		switch {
		case n > 0 || errors.As(err, &partial):
			return settle(func(c context.Context) settled { return p.markUnknown(c, op, ReasonPartialWrite, err.Error()) })
		case errors.As(err, &refused):
			return settle(func(c context.Context) settled { return p.reject(c, op, refused.Reason, refused.Detail) })
		}
		return settle(func(c context.Context) settled { return p.writeFailed(c, op, err) })
	}

	// 4. Read again; applied only when the read shows the plan done.
	if n == 0 {
		// Nothing was sent: the first read is the state.
		return p.settlePlanned(ctx, ll, kind, op, plan, read.Links, nil)
	}
	final, err := kind.Read(rctx, p.Remote, op)
	return p.settlePlanned(ctx, ll, kind, op, plan, final.Links, err)
}

// settlePlanned records a planned send from its final read (readErr when that
// read failed).
func (p *Protocol) settlePlanned(ctx context.Context, ll LinksLedger, kind PlannedKind, op Op, plan LinkPlan, cur []IssueLink, readErr error) Result {
	sctx, cancel := p.settling(ctx)
	defer cancel()
	if readErr != nil {
		return p.record(op, p.markUnknown(sctx, op, ReasonUnverified, "final read: "+readErr.Error()))
	}
	done, err := kind.Converged(op, plan, cur)
	if err != nil {
		return p.record(op, p.markUnknown(sctx, op, ReasonUnverified, err.Error()))
	}
	if !done {
		return p.record(op, p.markUnknown(sctx, op, ReasonPartialWrite, "the read after the plan does not show it done"))
	}
	got, err := ll.ApplyLinks(sctx, p.now(), op.ID, op.YTTargetID, cur)
	return p.record(op, settled{got, err})
}

// reconcilePlanned is reconcile for a PlannedKind (I5, I8, I9). It replaces
// the evidence search: read; applied when the read shows this attempt's
// latest plan done; otherwise, while fewer than LinkRecoverMax recovery plans
// were made, a new plan from this read, its diff executed, and a read again.
// A recovery request answered with a 4xx other than 401, 408 or 429 rejects
// the op (terminal, the lane moves on); 429 and 401 wait; anything else is
// tried again after the reconcile backoff. An exhausted op makes no request.
func (p *Protocol) reconcilePlanned(ctx context.Context, op Op, kind PlannedKind) (bool, error) {
	touch := func(wait time.Duration, reason, detail string) (bool, error) {
		return false, p.touchUnknown(ctx, op, wait, reason, detail)
	}
	ll, ok := p.Ledger.(LinksLedger)
	if !ok {
		return touch(0, ReasonReconcileFailed, "the ledger cannot plan "+op.Kind)
	}
	latest, found, err := ll.LatestLinkPlan(ctx, op.ID)
	if err != nil {
		return touch(0, ReasonReconcileFailed, err.Error())
	}
	// Only a plan of the op's last dispatch describes what it may have sent;
	// an earlier attempt ended deferred, which was not processed.
	if found && latest.Attempt != op.Attempts {
		found = false
	}
	recovers := 0
	if found {
		recovers = latest.Recover
	}
	if recovers >= LinkRecoverMax {
		log.Printf("ytwrite.links_recover_exhausted op_id=%d recovers=%d", op.ID, recovers)
		return touch(0, ReasonRecoverExhausted, fmt.Sprintf("%d recovery plans did not converge; an operator resolves it", recovers))
	}
	// One recovery at a time per op (review of PR #425): Drive reconciles a
	// remote_unknown op without a claim, and a recovery mutates, so two
	// workers must never overlap. The lease is a compare-and-set on the op's
	// due time and reconcile count, taken before the read; the losers make
	// no request. Everything this recovery does (remote steps and ledger
	// steps) runs under a deadline that ends a SettleTimeout before the
	// lease does, so no later holder starts while this one can still act.
	start := p.now()
	leased, err := ll.LeaseLinkRecovery(ctx, start, op.ID, op.Reconciles, start.Add(p.linkRecoveryLease()))
	if err != nil {
		return false, err
	}
	if !leased {
		return false, nil
	}
	wctx, cancelWork := context.WithTimeout(context.WithoutCancel(ctx), p.DispatchTimeout+p.SettleTimeout)
	defer cancelWork()
	ctx = wctx
	rctx, cancelRemote := context.WithTimeout(wctx, p.DispatchTimeout)
	defer cancelRemote()
	// failed classifies a recovery request's error.
	failed := func(err error) (bool, error) {
		var refused *RefusedError
		var re *RemoteError
		switch {
		case errors.As(err, &refused):
			return p.rejectRecovered(ctx, op, refused.Reason, refused.Detail)
		case errors.As(err, &re) && re.Status == http.StatusTooManyRequests:
			wait := re.RetryAfter
			if wait <= 0 {
				wait = p.RateLimitWait
			}
			return touch(wait, ReasonRateLimited, re.Error())
		case errors.As(err, &re) && re.Status == http.StatusUnauthorized:
			return touch(p.AuthWait, ReasonUnauthorized, re.Error())
		case errors.As(err, &re) && re.Status >= 400 && re.Status < 500 && re.Status != http.StatusRequestTimeout:
			return p.rejectRecovered(ctx, op, "http_"+strconv.Itoa(re.Status), re.Error())
		}
		return touch(0, ReasonReconcileFailed, err.Error())
	}
	read, err := kind.Read(rctx, p.Remote, op)
	if err != nil {
		return failed(err)
	}
	if found {
		done, err := kind.Converged(op, latest.Plan, read.Links)
		if err != nil {
			return touch(0, ReasonReconcileFailed, err.Error())
		}
		if done {
			return p.applyRecovered(ctx, ll, op, read.Links)
		}
	}
	plan, err := ll.PlanLinks(ctx, p.now(), op.ID, op.Attempts, recovers+1, read.Links)
	if errors.Is(err, ErrState) {
		return false, nil // resolved or recovered elsewhere meanwhile
	}
	if err != nil {
		return touch(0, ReasonReconcileFailed, "plan not recorded: "+err.Error())
	}
	n, err := kind.Execute(rctx, p.Remote, op, read.Types, plan)
	if err != nil {
		var partial *PartialWriteError
		if errors.As(err, &partial) {
			err = partial.Err
		}
		return failed(err)
	}
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
		return touch(0, ReasonReconcileFailed, err.Error())
	}
	if !done {
		return touch(0, ReasonPartialWrite, fmt.Sprintf("recovery plan %d: the read after it does not show it done", recovers+1))
	}
	return p.applyRecovered(ctx, ll, op, cur)
}

func (p *Protocol) applyRecovered(ctx context.Context, ll LinksLedger, op Op, cur []IssueLink) (bool, error) {
	got, err := ll.ApplyLinks(ctx, p.now(), op.ID, op.YTTargetID, cur)
	if err != nil {
		return false, err
	}
	logTransition(got)
	return true, nil
}

func (p *Protocol) rejectRecovered(ctx context.Context, op Op, reason, detail string) (bool, error) {
	s := p.reject(ctx, op, reason, detail)
	if s.err != nil {
		return false, s.err
	}
	logTransition(s.op)
	return false, nil
}

// linkRecoveryLease is how long one recovery pass holds its op: its remote
// steps (DispatchTimeout), its ledger steps (SettleTimeout), and a
// SettleTimeout of margin for a commit whose reply was lost.
func (p *Protocol) linkRecoveryLease() time.Duration {
	return p.DispatchTimeout + 2*p.SettleTimeout
}
