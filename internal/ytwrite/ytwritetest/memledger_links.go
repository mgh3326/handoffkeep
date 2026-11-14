package ytwritetest

import (
	"context"
	"fmt"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// The in-memory ledger's side of the issue_links kind (hk 4638 S3): the same
// plan (ytwrite.PlanIssueLinks) and compare-and-set (CheckLinkPlanStep) as the
// Postgres ledger, over in-memory rows. It knows no hk edges, so it passes no
// peer edges: a relates link another task writes and nobody owns is recorded
// as unowned here and as peer there; neither changes what is sent.

type memLinks struct {
	rowID   int64
	owned   []*ytwrite.OwnedLink
	unowned []*ytwrite.UnownedLink
	plans   map[int64][]ytwrite.LinkPlanRecord
}

var _ ytwrite.LinksLedger = (*MemLedger)(nil)

func (l *MemLedger) latestPlan(id int64) (ytwrite.LinkPlanRecord, bool) {
	ps := l.links.plans[id]
	if len(ps) == 0 {
		return ytwrite.LinkPlanRecord{}, false
	}
	return ps[len(ps)-1], true
}

// rowsOf returns copies of the rows naming issue on tenant.
func (l *MemLedger) rowsOf(tenant, issue string) ([]ytwrite.OwnedLink, []ytwrite.UnownedLink) {
	var owned []ytwrite.OwnedLink
	for _, o := range l.links.owned {
		if o.Tenant == tenant && (o.From == issue || o.To == issue) {
			owned = append(owned, *o)
		}
	}
	var unowned []ytwrite.UnownedLink
	for _, u := range l.links.unowned {
		if u.Tenant == tenant && u.IssueID == issue {
			unowned = append(unowned, *u)
		}
	}
	return owned, unowned
}

func (l *MemLedger) ownedByID(id int64) *ytwrite.OwnedLink {
	for _, o := range l.links.owned {
		if o.ID == id {
			return o
		}
	}
	return nil
}

func (l *MemLedger) PlanLinks(_ context.Context, now time.Time, id int64, attempt, recover int, cur []ytwrite.IssueLink) (ytwrite.LinkPlan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	latest, found := l.latestPlan(id)
	if err := ytwrite.CheckLinkPlanStep(*op, attempt, recover, latest, found); err != nil {
		return ytwrite.LinkPlan{}, err
	}
	snap, err := ytwrite.IssueLinksPayload(*op)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	owned, unowned := l.rowsOf(op.Tenant, op.YTTargetID)
	var open []ytwrite.OwnedLink
	for _, o := range owned {
		if o.Open() {
			open = append(open, o)
		}
	}
	plan, ch, err := ytwrite.PlanIssueLinks(*op, snap, cur, open, nil)
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	// Check the one-open-row rule before writing anything.
	for _, n := range ch.Insert {
		for _, o := range l.links.owned {
			if o.Open() && o.ReleasedOp == 0 && o.Tenant == n.Tenant && o.LinkType == n.LinkType && o.From == n.From && o.To == n.To && !contains(ch.Close, o.ID) && !contains(ch.Release, o.ID) {
				return ytwrite.LinkPlan{}, fmt.Errorf("ytwritetest: link %s %s->%s already owned by row %d", n.LinkType, n.From, n.To, o.ID)
			}
		}
	}
	for _, rid := range ch.Release {
		l.ownedByID(rid).ReleasedOp = id
	}
	for _, rid := range ch.Unrelease {
		l.ownedByID(rid).ReleasedOp = 0
	}
	for _, rid := range ch.Close {
		o := l.ownedByID(rid)
		if o.ReleasedOp == 0 {
			o.ReleasedOp = id
		}
		o.ReleasedAt = now
	}
	for _, n := range ch.Insert {
		l.links.rowID++
		n.ID, n.IntendedAt = l.links.rowID, now
		l.links.owned = append(l.links.owned, &n)
	}
	for _, u := range plan.Unowned {
		l.seeUnowned(*op, u, now)
	}
	_, gone := ytwrite.ClosedByRead(*op, nil, unowned, cur)
	l.goneUnowned(gone, now)
	if l.links.plans == nil {
		l.links.plans = map[int64][]ytwrite.LinkPlanRecord{}
	}
	l.links.plans[id] = append(l.links.plans[id], ytwrite.LinkPlanRecord{Attempt: attempt, Recover: recover, Plan: plan, CreatedAt: now})
	return plan, nil
}

func (l *MemLedger) seeUnowned(op ytwrite.Op, link ytwrite.IssueLink, now time.Time) {
	side := ytwrite.UnownedSide(link.Kind)
	for _, u := range l.links.unowned {
		if u.GoneAt.IsZero() && u.Tenant == op.Tenant && u.IssueID == op.YTTargetID && u.LinkType == link.Kind && u.Side == side && u.OtherIssue == link.Target {
			u.LastSeenOp, u.LastSeenAt = max(u.LastSeenOp, op.ID), now
			return
		}
	}
	l.links.rowID++
	l.links.unowned = append(l.links.unowned, &ytwrite.UnownedLink{ID: l.links.rowID, Tenant: op.Tenant, IssueID: op.YTTargetID,
		LinkType: link.Kind, Side: side, OtherIssue: link.Target, FirstSeenOp: op.ID, LastSeenOp: op.ID, FirstSeenAt: now, LastSeenAt: now})
}

func (l *MemLedger) goneUnowned(ids []int64, now time.Time) {
	for _, u := range l.links.unowned {
		if contains(ids, u.ID) {
			u.GoneAt = now
		}
	}
}

func (l *MemLedger) LatestLinkPlan(_ context.Context, id int64) (ytwrite.LinkPlanRecord, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, found := l.latestPlan(id)
	return rec, found, nil
}

func (l *MemLedger) ApplyLinks(_ context.Context, now time.Time, id int64, ytID string, cur []ytwrite.IssueLink) (ytwrite.Op, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return ytwrite.Op{}, err
	}
	switch op.State {
	case ytwrite.StateApplied:
		if op.YTResultID == ytID {
			return *op, nil
		}
		return ytwrite.Op{}, ytwrite.ErrConflictingResult
	case ytwrite.StateDispatched, ytwrite.StateRemoteUnknown:
	default:
		return ytwrite.Op{}, fmt.Errorf("%w: apply from %s", ytwrite.ErrState, op.State)
	}
	owned, unowned := l.rowsOf(op.Tenant, op.YTTargetID)
	closeOwned, gone := ytwrite.ClosedByRead(*op, owned, unowned, cur)
	for _, rid := range closeOwned {
		l.ownedByID(rid).ReleasedAt = now
	}
	l.goneUnowned(gone, now)
	op.State, op.YTResultID, op.Reason, op.LastError, op.ResolvedAt = ytwrite.StateApplied, ytID, "", "", now
	return *op, nil
}

func (l *MemLedger) LeaseLinkRecovery(_ context.Context, now time.Time, id int64, reconciles int, until time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return false, err
	}
	if op.State != ytwrite.StateRemoteUnknown || op.NextAttemptAt.After(now) || op.Reconciles != reconciles {
		return false, nil
	}
	op.NextAttemptAt = until
	return true, nil
}

func (l *MemLedger) ReleaseUnsentLinks(_ context.Context, now time.Time, id int64, links []ytwrite.IssueLink) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	op, err := l.get(id)
	if err != nil {
		return err
	}
	writer, err := ytwrite.LinkWriter(*op)
	if err != nil {
		return err
	}
	for _, link := range links {
		for _, o := range l.links.owned {
			if o.Open() && o.ReleasedOp == 0 && o.Tenant == op.Tenant && o.From == op.YTTargetID && o.WriterTask == writer &&
				o.LinkType == link.Kind && o.To == link.Target {
				o.ReleasedOp, o.ReleasedAt = id, now
			}
		}
	}
	return nil
}

func (l *MemLedger) LinkRecords(_ context.Context, tenant, issueID string) (ytwrite.IssueLinkRecords, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	owned, unowned := l.rowsOf(tenant, issueID)
	return ytwrite.IssueLinkRecords{Owned: owned, Unowned: unowned}, nil
}

func contains(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// The CrashLedger forwards the links methods of a ledger that has them.

func (c *CrashLedger) links() (ytwrite.LinksLedger, error) {
	ll, ok := c.Ledger.(ytwrite.LinksLedger)
	if !ok {
		return nil, fmt.Errorf("%w: the wrapped ledger cannot plan links", ytwrite.ErrInvalid)
	}
	return ll, nil
}

func (c *CrashLedger) PlanLinks(ctx context.Context, now time.Time, id int64, attempt, recover int, cur []ytwrite.IssueLink) (ytwrite.LinkPlan, error) {
	if err := c.enter("PlanLinks"); err != nil {
		return ytwrite.LinkPlan{}, err
	}
	ll, err := c.links()
	if err != nil {
		return ytwrite.LinkPlan{}, err
	}
	plan, err := ll.PlanLinks(ctx, now, id, attempt, recover, cur)
	if err := c.leave("PlanLinks", err); err != nil {
		return ytwrite.LinkPlan{}, err
	}
	return plan, nil
}

func (c *CrashLedger) LatestLinkPlan(ctx context.Context, id int64) (ytwrite.LinkPlanRecord, bool, error) {
	if err := c.enter("LatestLinkPlan"); err != nil {
		return ytwrite.LinkPlanRecord{}, false, err
	}
	ll, err := c.links()
	if err != nil {
		return ytwrite.LinkPlanRecord{}, false, err
	}
	return ll.LatestLinkPlan(ctx, id)
}

func (c *CrashLedger) ApplyLinks(ctx context.Context, now time.Time, id int64, ytID string, cur []ytwrite.IssueLink) (ytwrite.Op, error) {
	if err := c.enter("ApplyLinks"); err != nil {
		return ytwrite.Op{}, err
	}
	ll, err := c.links()
	if err != nil {
		return ytwrite.Op{}, err
	}
	op, err := ll.ApplyLinks(ctx, now, id, ytID, cur)
	if err := c.leave("ApplyLinks", err); err != nil {
		return ytwrite.Op{}, err
	}
	return op, nil
}

func (c *CrashLedger) LeaseLinkRecovery(ctx context.Context, now time.Time, id int64, reconciles int, until time.Time) (bool, error) {
	if err := c.enter("LeaseLinkRecovery"); err != nil {
		return false, err
	}
	ll, err := c.links()
	if err != nil {
		return false, err
	}
	ok, err := ll.LeaseLinkRecovery(ctx, now, id, reconciles, until)
	if err := c.leave("LeaseLinkRecovery", err); err != nil {
		return false, err
	}
	return ok, nil
}

func (c *CrashLedger) ReleaseUnsentLinks(ctx context.Context, now time.Time, id int64, links []ytwrite.IssueLink) error {
	if err := c.enter("ReleaseUnsentLinks"); err != nil {
		return err
	}
	ll, err := c.links()
	if err != nil {
		return err
	}
	return c.leave("ReleaseUnsentLinks", ll.ReleaseUnsentLinks(ctx, now, id, links))
}

func (c *CrashLedger) LinkRecords(ctx context.Context, tenant, issueID string) (ytwrite.IssueLinkRecords, error) {
	if err := c.enter("LinkRecords"); err != nil {
		return ytwrite.IssueLinkRecords{}, err
	}
	ll, err := c.links()
	if err != nil {
		return ytwrite.IssueLinkRecords{}, err
	}
	return ll.LinkRecords(ctx, tenant, issueID)
}
