package ytwrite_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
	"github.com/mgh3326/handoffkeep/internal/ytwrite/ytwritetest"
)

// stuckApply is a ledger whose Apply waits until its context ends or the test
// releases it, like an Apply queued behind a row lock.
type stuckApply struct {
	*ytwritetest.MemLedger
	release chan struct{}
}

func (l stuckApply) Apply(ctx context.Context, now time.Time, id int64, ytID string, maps []ytwrite.MapRow) (ytwrite.Op, error) {
	select {
	case <-ctx.Done():
		return ytwrite.Op{}, ctx.Err()
	case <-l.release:
		return l.MemLedger.Apply(ctx, now, id, ytID, maps)
	}
}

// Recording a remote answer is bounded by SettleTimeout even when the caller
// can no longer cancel it. A settlement that cannot finish leaves the op
// dispatched (durable, not applied); stale recovery and the marker search
// then apply it without a second POST.
func TestSettlementIsBounded(t *testing.T) {
	yt := ytwritetest.NewFake()
	clock := ytwritetest.NewClock()
	ledger := stuckApply{MemLedger: ytwritetest.NewMemLedger(), release: make(chan struct{})}
	defer close(ledger.release)
	p, err := ytwrite.New(ytwrite.Config{Ledger: ledger, Remote: yt, Now: clock.Now, SettleTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		r   ytwrite.Result
		err error
	}
	done := make(chan answer, 1)
	go func() {
		r, err := p.AppendComment(context.Background(), commentReq("settle", "본문"))
		done <- answer{r, err}
	}()
	var got answer
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		ledger.release <- struct{}{}
		<-done
		t.Fatal("settlement waited on its ledger step without a bound")
	}
	if got.err != nil || got.r.Status != ytwrite.StatusPending || got.r.Reason != ytwrite.ReasonOutcomeUnrecorded {
		t.Fatalf("answer %+v %v; want pending, outcome_unrecorded", got.r, got.err)
	}
	op, err := ledger.ByID(context.Background(), got.r.OpID)
	if err != nil || op.State != ytwrite.StateDispatched || yt.Calls() != 1 {
		t.Fatalf("op %+v %v after %d POSTs; want dispatched after one", op, err, yt.Calls())
	}
	// A later pass with a working ledger: stale recovery, then evidence.
	clock.Advance(time.Hour)
	p2, err := ytwrite.New(ytwrite.Config{Ledger: ledger.MemLedger, Remote: yt, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p2.Drive(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	if op, err := ledger.ByID(context.Background(), got.r.OpID); err != nil || op.State != ytwrite.StateApplied || yt.Calls() != 1 {
		t.Fatalf("after recovery: op %+v %v, %d POSTs", op, err, yt.Calls())
	}
}

// The tenant is part of what an op writes: it is stored with the op and
// covered by the payload hash, so the same key on another tenant conflicts.
func TestCommentOpBindsTenant(t *testing.T) {
	a := commentReq("task-comment:9", "본문")
	a.Tenant = "a.youtrack.cloud"
	b := a
	b.Tenant = "b.youtrack.cloud"
	na, err := ytwrite.CommentOp(a)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := ytwrite.CommentOp(b)
	if err != nil {
		t.Fatal(err)
	}
	if na.Tenant != a.Tenant || nb.Tenant != b.Tenant || na.PayloadSHA == nb.PayloadSHA {
		t.Fatalf("ops %+v / %+v: the tenant is not bound", na, nb)
	}
	ledger := ytwritetest.NewMemLedger()
	op, _, err := ledger.Prepare(context.Background(), time.Now().UTC(), na)
	if err != nil || op.Tenant != a.Tenant {
		t.Fatalf("prepared %+v %v", op, err)
	}
	if _, _, err := ledger.Prepare(context.Background(), time.Now().UTC(), nb); !errors.Is(err, ytwrite.ErrKeyConflict) {
		t.Fatalf("same key on another tenant: %v, want ErrKeyConflict", err)
	}
}
