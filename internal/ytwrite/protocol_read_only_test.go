package ytwrite

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// readOnlyLedger records the one Defer or Reject a settlement makes.
type readOnlyLedger struct {
	Ledger
	deferred *Op
	rejected *Op
}

func (l *readOnlyLedger) Defer(_ context.Context, _ time.Time, _ int64, next time.Time, reason, detail string) (Op, error) {
	op := Op{NextAttemptAt: next, Reason: reason, LastError: detail}
	l.deferred = &op
	return op, nil
}

func (l *readOnlyLedger) Reject(_ context.Context, _ time.Time, _ int64, reason, detail string) (Op, error) {
	op := Op{State: StateRejected, Reason: reason, LastError: detail}
	l.rejected = &op
	return op, nil
}

// youtrackReadOnly is the 418 body YouTrack sends during its maintenance
// window (2026-11-19): status 418, error invalid_state.
var youtrackReadOnly = &RemoteError{Status: http.StatusTeapot, Code: "invalid_state", Message: "The application is currently in read-only mode. Please try again later."}

// youtrackOtherTeapot is a 418 with another code: an ordinary refusal.
var youtrackOtherTeapot = &RemoteError{Status: http.StatusTeapot, Code: "some_other_error", Message: "refused"}

func readOnlyProtocol(l Ledger, now time.Time) *Protocol {
	return &Protocol{Config: Config{
		Ledger: l, Now: func() time.Time { return now },
		ReconcileBase: 5 * time.Second, ReconcileMax: 5 * time.Minute,
		RateLimitWait: 5 * time.Second, ReadOnlyWait: 5 * time.Minute, MaxRetryAfter: RetryAfterCap,
	}}
}

// The write path: the read-only answer defers the op like a 429 without
// Retry-After, under its own reason, and does not reject it.
func TestWriteReadOnlyDefersLikeARateLimit(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)
	l := &readOnlyLedger{}
	p := readOnlyProtocol(l, now)

	got := p.writeFailed(ctx, p, Op{ID: 1}, youtrackReadOnly).op
	if l.rejected != nil {
		t.Fatalf("read-only answer rejected the op: %+v", l.rejected)
	}
	if got.Reason != ReasonReadOnly {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonReadOnly)
	}
	if delay := got.NextAttemptAt.Sub(now); delay != 5*time.Minute {
		t.Fatalf("delay = %s, want ReadOnlyWait 5m", delay)
	}
}

// A Retry-After the server sent wins over ReadOnlyWait, as it does for a 429.
func TestWriteReadOnlyHonoursRetryAfter(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)
	l := &readOnlyLedger{}
	p := readOnlyProtocol(l, now)
	re := *youtrackReadOnly
	re.RetryAfter = 90 * time.Second

	got := p.writeFailed(ctx, p, Op{ID: 1}, &re).op
	if delay := got.NextAttemptAt.Sub(now); delay != 90*time.Second {
		t.Fatalf("delay = %s, want the server's 90s", delay)
	}
}

// ReadOnlyWait is capped by MaxRetryAfter, as a 429's wait is.
func TestWriteReadOnlyWaitIsCapped(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)
	l := &readOnlyLedger{}
	p := readOnlyProtocol(l, now)
	p.ReadOnlyWait = 2 * time.Hour
	p.MaxRetryAfter = time.Hour

	got := p.writeFailed(ctx, p, Op{ID: 1}, youtrackReadOnly).op
	if delay := got.NextAttemptAt.Sub(now); delay != time.Hour {
		t.Fatalf("delay = %s, want the MaxRetryAfter cap 1h", delay)
	}
}

// The read path: a failed read answered with the same 418 waits, and the
// op is not rejected.
func TestReadReadOnlyDefersNotSent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)
	l := &readOnlyLedger{}
	p := readOnlyProtocol(l, now)

	got := p.notSent(ctx, p, Op{ID: 1, Attempts: 3}, &NotSentError{Err: youtrackReadOnly}).op
	if l.rejected != nil {
		t.Fatalf("read-only answer rejected the op: %+v", l.rejected)
	}
	if got.Reason != ReasonReadOnly {
		t.Fatalf("reason = %q, want %q", got.Reason, ReasonReadOnly)
	}
	if delay := got.NextAttemptAt.Sub(now); delay != 5*time.Minute {
		t.Fatalf("delay = %s, want ReadOnlyWait 5m", delay)
	}
}

// Another 418 body is a final refusal on both paths, as before.
func TestOtherTeapotStaysRejected(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 19, 17, 0, 0, 0, time.UTC)

	l := &readOnlyLedger{}
	p := readOnlyProtocol(l, now)
	p.writeFailed(ctx, p, Op{ID: 1}, youtrackOtherTeapot)
	if l.rejected == nil || l.rejected.Reason != "http_418" || l.deferred != nil {
		t.Fatalf("write path: rejected=%+v deferred=%+v, want rejected http_418", l.rejected, l.deferred)
	}

	l = &readOnlyLedger{}
	p = readOnlyProtocol(l, now)
	p.notSent(ctx, p, Op{ID: 1}, &NotSentError{Err: youtrackOtherTeapot})
	if l.rejected == nil || l.rejected.Reason != "http_418" || l.deferred != nil {
		t.Fatalf("read path: rejected=%+v deferred=%+v, want rejected http_418", l.rejected, l.deferred)
	}
}

// A 418 without a code is not the maintenance answer either.
func TestBareTeapotIsNotReadOnly(t *testing.T) {
	re := &RemoteError{Status: http.StatusTeapot, Message: "teapot"}
	if readOnlyRefusal(re) {
		t.Fatal("a 418 with no error code matched the read-only refusal")
	}
	if readOnlyRefusal(&RemoteError{Status: http.StatusForbidden, Code: readOnlyCode}) {
		t.Fatal("invalid_state on a non-418 status matched the read-only refusal")
	}
	var e error = youtrackReadOnly
	var re2 *RemoteError
	if !errors.As(e, &re2) || !readOnlyRefusal(re2) {
		t.Fatal("the maintenance answer does not match through errors.As")
	}
}
