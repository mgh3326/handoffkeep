package ytwrite

import (
	"context"
	"errors"
	"testing"
	"time"
)

type captureDeferLedger struct {
	Ledger
	next   time.Time
	reason string
}

func (l *captureDeferLedger) Defer(_ context.Context, _ time.Time, _ int64, next time.Time, reason, _ string) (Op, error) {
	l.next, l.reason = next, reason
	return Op{NextAttemptAt: next, Reason: reason}, nil
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

// hk 4784: a read that keeps failing waits the reconcile backoff, not the
// dispatch cap. A cap equal to the worker's pass interval is what made 197
// article_create ops come back in every pass and fill the window.
func TestNotSentBackoffGrowsPastTheDispatchCap(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		attempts int
		want     time.Duration
	}{{1, 10 * time.Second}, {5, 2 * time.Minute + 40*time.Second}, {49, 5 * time.Minute}, {50, time.Hour}, {80, 24 * time.Hour}, {484, 24 * time.Hour}} {
		ledger := &captureDeferLedger{}
		op := Op{ID: 1, Attempts: tc.attempts}
		p := &Protocol{Config: Config{
			Ledger: ledger, Now: func() time.Time { return now },
			ReconcileBase: 5 * time.Second, ReconcileMax: 5 * time.Minute,
		}}

		got := p.notSent(ctx, p, op, &NotSentError{Err: errors.New("connection failed")}).op
		if delay := got.NextAttemptAt.Sub(now); delay != tc.want {
			t.Errorf("notSent delay after %d attempts=%s, want %s", tc.attempts, delay, tc.want)
		}
	}
}

// A 429 read still waits exactly what the server asked, not the backoff.
func TestNotSentHonoursRetryAfter(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	ledger := &captureDeferLedger{}
	p := &Protocol{Config: Config{
		Ledger: ledger, Now: func() time.Time { return now },
		ReconcileBase: 5 * time.Second, ReconcileMax: 5 * time.Minute, MaxRetryAfter: RetryAfterCap,
	}}
	op := Op{ID: 1, Attempts: 60}
	ns := &NotSentError{Err: &RemoteError{Status: 429, RetryAfter: 90 * time.Second}}
	got := p.notSent(ctx, p, op, ns).op
	if delay := got.NextAttemptAt.Sub(now); delay != 90*time.Second {
		t.Fatalf("notSent delay after a 429=%s, want the server's 90s", delay)
	}
	if got.Reason != ReasonRateLimited {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonRateLimited)
	}
}
