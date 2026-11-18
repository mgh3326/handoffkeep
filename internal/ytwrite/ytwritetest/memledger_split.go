package ytwritetest

import (
	"context"
	"fmt"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The in-memory ledger's side of article_split (hk 5085): the same fence
// (CheckLinkFence), plan check (CheckSplitPlan), mark check (CheckSplitMark)
// and settlement check (CheckSplitSettlement) as the store's ledger.

var _ ytwrite.SplitLedger = (*MemLedger)(nil)

func (l *MemLedger) PlanSplit(_ context.Context, now time.Time, id int64, f ytwrite.LinkFence, plan ytwrite.SplitPlan) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return err
	}
	if err := ytwrite.CheckLinkFence(*op, f, now); err != nil {
		return err
	}
	if err := ytwrite.CheckSplitPlan(*op, l.splits[id], f, plan); err != nil {
		return err
	}
	if l.splits == nil {
		l.splits = map[int64][]ytwrite.SplitPlanRecord{}
	}
	plan.Steps = append([]int{}, plan.Steps...)
	l.splits[id] = append(l.splits[id], ytwrite.SplitPlanRecord{Attempt: f.Attempt, Recover: f.Recover, Plan: plan})
	return nil
}

func (l *MemLedger) SplitPlans(_ context.Context, id int64) ([]ytwrite.SplitPlanRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ytwrite.SplitPlanRecord(nil), l.splits[id]...), nil
}

func (l *MemLedger) MarkSplitStep(_ context.Context, now time.Time, id int64, f ytwrite.LinkFence, step int, index string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return err
	}
	if err := ytwrite.CheckLinkFence(*op, f, now); err != nil {
		return err
	}
	ps := l.splits[id]
	var latest ytwrite.SplitPlanRecord
	if len(ps) > 0 {
		latest = ps[len(ps)-1]
	}
	if err := ytwrite.CheckSplitMark(*op, latest, len(ps) > 0, f, step, index); err != nil {
		return err
	}
	ps[len(ps)-1].Marked++
	if index != "" {
		op.YTResultID = index
	}
	return nil
}

func (l *MemLedger) SettleSplit(_ context.Context, now time.Time, id int64, f ytwrite.LinkFence, s ytwrite.LinkSettlement) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	if err := ytwrite.CheckLinkFence(*op, f, now); err != nil {
		return ytwrite.Op{}, err
	}
	if err := ytwrite.CheckSplitSettlement(*op, s); err != nil {
		return ytwrite.Op{}, err
	}
	ps := l.splits[id]
	if f.Epoch == ytwrite.EpochOperator {
		if err := ytwrite.CheckSplitOperator(*op, ps, s); err != nil {
			return ytwrite.Op{}, err
		}
	}
	if len(ps) > 0 {
		latest := &ps[len(ps)-1]
		if step, outcome, ok := s.StepRecord(latest.Marked); ok && latest.Attempt == f.Attempt && latest.Recover == f.Recover && step == latest.Marked-1 {
			latest.Outcome = outcome
		}
	}
	if s.YTID != "" {
		op.YTResultID = s.YTID
	}
	switch s.To {
	case ytwrite.StateRejected:
		op.State, op.Reason, op.LastError, op.ResolvedAt = ytwrite.StateRejected, s.Reason, s.Detail, now
	default: // prepared, remote_unknown
		op.State, op.NextAttemptAt, op.Reason, op.LastError = s.To, s.Next, s.Reason, s.Detail
	}
	return *op, nil
}

func (l *MemLedger) ApplySplit(_ context.Context, now time.Time, id int64, f ytwrite.LinkFence, s ytwrite.LinkSettlement, maps []ytwrite.MapRow) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.applyLocked(now, id, s.YTID, maps, func(op ytwrite.Op) error {
		return ytwrite.CheckSplitApply(op, l.splits[id], f, s, now)
	})
}

// The CrashLedger forwards the split methods of a ledger that has them.

func (c *CrashLedger) split() (ytwrite.SplitLedger, error) {
	sl, ok := c.Ledger.(ytwrite.SplitLedger)
	if !ok {
		return nil, fmt.Errorf("%w: the wrapped ledger cannot split", ytwrite.ErrInvalid)
	}
	return sl, nil
}

func (c *CrashLedger) PlanSplit(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, plan ytwrite.SplitPlan) error {
	if err := c.enter("PlanSplit"); err != nil {
		return err
	}
	sl, err := c.split()
	if err != nil {
		return err
	}
	return c.leave("PlanSplit", sl.PlanSplit(ctx, now, id, f, plan))
}

func (c *CrashLedger) SplitPlans(ctx context.Context, id int64) ([]ytwrite.SplitPlanRecord, error) {
	if err := c.enter("SplitPlans"); err != nil {
		return nil, err
	}
	sl, err := c.split()
	if err != nil {
		return nil, err
	}
	return sl.SplitPlans(ctx, id)
}

func (c *CrashLedger) MarkSplitStep(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, step int, index string) error {
	if err := c.enter("MarkSplitStep"); err != nil {
		return err
	}
	sl, err := c.split()
	if err != nil {
		return err
	}
	return c.leave("MarkSplitStep", sl.MarkSplitStep(ctx, now, id, f, step, index))
}

func (c *CrashLedger) SettleSplit(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, s ytwrite.LinkSettlement) (ytwrite.Op, error) {
	if err := c.enter("SettleSplit"); err != nil {
		return ytwrite.Op{}, err
	}
	sl, err := c.split()
	if err != nil {
		return ytwrite.Op{}, err
	}
	op, err := sl.SettleSplit(ctx, now, id, f, s)
	if err := c.leave("SettleSplit", err); err != nil {
		return ytwrite.Op{}, err
	}
	return op, nil
}

func (c *CrashLedger) ApplySplit(ctx context.Context, now time.Time, id int64, f ytwrite.LinkFence, s ytwrite.LinkSettlement, maps []ytwrite.MapRow) (ytwrite.Op, error) {
	if err := c.enter("ApplySplit"); err != nil {
		return ytwrite.Op{}, err
	}
	sl, err := c.split()
	if err != nil {
		return ytwrite.Op{}, err
	}
	op, err := sl.ApplySplit(ctx, now, id, f, s, maps)
	if err := c.leave("ApplySplit", err); err != nil {
		return ytwrite.Op{}, err
	}
	return op, nil
}
