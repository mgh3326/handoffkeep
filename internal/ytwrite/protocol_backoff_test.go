package ytwrite

import (
	"context"
	"errors"
	"testing"
	"time"
)

type captureDeferLedger struct {
	Ledger
	next time.Time
}

func (l *captureDeferLedger) Defer(_ context.Context, _ time.Time, _ int64, next time.Time, _, _ string) (Op, error) {
	l.next = next
	return Op{NextAttemptAt: next}, nil
}

func TestReconcileBackoffAfterFiftySearches(t *testing.T) {
	p := &Protocol{Config: Config{ReconcileBase: 5 * time.Second, ReconcileMax: 5 * time.Minute}}
	for _, tc := range []struct {
		searches int
		want     time.Duration
	}{{0, 5 * time.Second}, {49, 5 * time.Minute}, {50, time.Hour}, {73, time.Hour}, {74, 24 * time.Hour}} {
		if got := p.reconcileDelay(tc.searches); got != tc.want {
			t.Errorf("after %d searches delay=%s want %s", tc.searches, got, tc.want)
		}
	}
}

func TestNotSentKeepsCappedDispatchBackoffAfterManyAttempts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	ledger := &captureDeferLedger{}
	op := Op{ID: 1, Attempts: 80} // dispatch failures do not count as evidence searches
	p := &Protocol{Config: Config{
		Ledger: ledger, Now: func() time.Time { return now },
		ReconcileBase: 5 * time.Second, ReconcileMax: 5 * time.Minute,
	}}

	got := p.notSent(ctx, p, op, &NotSentError{Err: errors.New("connection failed")}).op
	if delay := got.NextAttemptAt.Sub(now); delay != 5*time.Minute {
		t.Fatalf("notSent delay after %d attempts=%s, want capped dispatch backoff 5m", op.Attempts, delay)
	}
}
