package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type fakeHeldRecoverer struct {
	at     []time.Time
	grace  []time.Duration
	failed bool
}

func (f *fakeHeldRecoverer) RecoverHeldYTOps(_ context.Context, now time.Time, grace time.Duration) (store.YTRecovery, error) {
	f.at = append(f.at, now)
	f.grace = append(f.grace, grace)
	if f.failed {
		return store.YTRecovery{}, errors.New("boom")
	}
	return store.YTRecovery{Rejected: 1, OpIDs: []int64{7}}, nil
}

// The worker runs the recovery pass once per ytHeldRecoveryInterval with the
// store's grace, a failed pass is not retried before the interval passes, and
// a worker with no recoverer does nothing.
func TestYTWorkerRunsHeldRecoveryOncePerInterval(t *testing.T) {
	clock := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	f := &fakeHeldRecoverer{}
	w := ytCommentsWorker{recovery: f, now: func() time.Time { return clock }}
	var last time.Time
	w.recoverHeld(t.Context(), &last)
	clock = clock.Add(ytHeldRecoveryInterval - time.Second)
	w.recoverHeld(t.Context(), &last)
	if len(f.at) != 1 {
		t.Fatalf("passes before the interval = %d, want 1", len(f.at))
	}
	clock = clock.Add(time.Second)
	f.failed = true
	w.recoverHeld(t.Context(), &last)
	w.recoverHeld(t.Context(), &last)
	if len(f.at) != 2 || !f.at[1].Equal(clock) {
		t.Fatalf("passes = %v, want one more at %v after the interval (a failure waits for the next one)", f.at, clock)
	}
	for _, g := range f.grace {
		if g != store.YTHeldRecoveryGrace {
			t.Fatalf("grace = %v, want %v", g, store.YTHeldRecoveryGrace)
		}
	}
	ytCommentsWorker{}.recoverHeld(t.Context(), &last)
}
