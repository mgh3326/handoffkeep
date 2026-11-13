package store

import (
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/ytwrite"
)

// TestYTWriteOutboxStatusCountsOpenOps drives ops into each open state on an
// injected clock and reads the outbox gauges at t0. Applied and rejected ops
// are not open and are not counted; the oldest age per state is measured from
// created_at, or from dispatched_at for a dispatched op; retrying counts ops
// sent more than once, reconciling counts ops checked again.
func TestYTWriteOutboxStatusCountsOpenOps(t *testing.T) {
	s := schemaStore(t, "ytgauge")
	ctx := t.Context()
	l := s.YTWriteLedger()
	t0 := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	// Each op has its own entity so the lane ordering never blocks a claim.
	prepare := func(key string, createdAt time.Time) ytwrite.Op {
		t.Helper()
		op, _, err := l.Prepare(ctx, createdAt, ytwrite.NewOp{Principal: "p", Kind: ytwrite.KindCommentAppend, Key: key, PayloadSHA: key,
			Payload: []byte(`{"body":"x"}`), EntityKind: "task", EntityID: key, YTTargetID: "2-" + key, Actor: "a"})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	claim := func(op ytwrite.Op, at time.Time) {
		t.Helper()
		if _, outcome, err := l.Claim(ctx, at, op.ID); err != nil || outcome != ytwrite.ClaimClaimed {
			t.Fatalf("claim op %d = %s, %v", op.ID, outcome, err)
		}
	}

	prepare("fresh", t0.Add(-1*time.Minute))

	// Sent, deferred back to prepared, sent again: one retry, now dispatched.
	retried := prepare("retried", t0.Add(-20*time.Minute))
	claim(retried, t0.Add(-20*time.Minute))
	if _, err := l.Defer(ctx, t0.Add(-19*time.Minute), retried.ID, t0.Add(-19*time.Minute), ytwrite.ReasonTransport, "429"); err != nil {
		t.Fatal(err)
	}
	claim(retried, t0.Add(-18*time.Minute))

	// Unknown outcome, checked once more: remote_unknown with one reconcile.
	unknown := prepare("unknown", t0.Add(-40*time.Minute))
	claim(unknown, t0.Add(-40*time.Minute))
	if _, err := l.MarkUnknown(ctx, t0.Add(-39*time.Minute), unknown.ID, t0.Add(-39*time.Minute), ytwrite.ReasonTransport, "timeout"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.TouchUnknown(ctx, t0.Add(-30*time.Minute), unknown.ID, t0.Add(-30*time.Minute), ytwrite.ReasonTransport, "still unknown"); err != nil {
		t.Fatal(err)
	}

	// Resolved ops must not be counted.
	rejected := prepare("rejected", t0.Add(-50*time.Minute))
	if _, err := l.Reject(ctx, t0, rejected.ID, ytwrite.ReasonOperatorNotDone, "checked"); err != nil {
		t.Fatal(err)
	}

	got, err := s.YTWriteOutboxStatus(ctx, t0)
	if err != nil {
		t.Fatal(err)
	}
	wantOpen := map[ytwrite.State]int64{ytwrite.StatePrepared: 1, ytwrite.StateDispatched: 1, ytwrite.StateRemoteUnknown: 1}
	wantAge := map[ytwrite.State]time.Duration{
		ytwrite.StatePrepared:      time.Minute,
		ytwrite.StateDispatched:    18 * time.Minute,
		ytwrite.StateRemoteUnknown: 40 * time.Minute,
	}
	for _, st := range YTWriteOutboxStates {
		if got.Open[st] != wantOpen[st] {
			t.Errorf("open[%s] = %d, want %d", st, got.Open[st], wantOpen[st])
		}
		if got.OldestAge[st] != wantAge[st] {
			t.Errorf("oldest age[%s] = %s, want %s", st, got.OldestAge[st], wantAge[st])
		}
	}
	if got.Retrying != 1 || got.Reconciling != 1 {
		t.Errorf("retrying=%d reconciling=%d, want 1 and 1", got.Retrying, got.Reconciling)
	}
}
