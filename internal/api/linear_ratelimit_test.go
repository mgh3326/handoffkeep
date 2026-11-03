package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func TestLinearRateGateStopsRequestsUntilReset(t *testing.T) {
	now := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)
	reset := now.Add(20 * time.Minute)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("X-RateLimit-Requests-Reset", strconv.FormatInt(reset.UnixMilli(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	gate := &LinearRateGate{now: func() time.Time { return now }}
	a := LinearAPI{Key: "k", URL: srv.URL, HTTP: srv.Client(), RateGate: gate}

	if err := a.graphql(context.Background(), time.Second, "{viewer{id}}", nil, nil); err == nil || errors.Is(err, ErrLinearRateLimited) {
		t.Fatalf("first call err = %v, want the 429 rejection", err)
	}
	for range 5 {
		if err := a.graphql(context.Background(), time.Second, "{viewer{id}}", nil, nil); !errors.Is(err, ErrLinearRateLimited) {
			t.Fatalf("paused call err = %v, want ErrLinearRateLimited", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Linear received %d requests while paused, want 1", got)
	}
	now = reset.Add(time.Second)
	if gate.Paused() {
		t.Fatal("gate still paused after the reset time")
	}
}

func TestLinearRateGateBoundsPause(t *testing.T) {
	now := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		header http.Header
		want   time.Duration
	}{
		"no headers":        {http.Header{}, linearRatePauseDefault},
		"retry-after":       {http.Header{"Retry-After": {"90"}}, 90 * time.Second},
		"reset beyond max":  {http.Header{"X-Ratelimit-Requests-Reset": {strconv.FormatInt(now.Add(3*time.Hour).UnixMilli(), 10)}}, linearRatePauseMax},
		"reset in past":     {http.Header{"X-Ratelimit-Requests-Reset": {strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10)}}, time.Second},
		"both, reset later": {http.Header{"Retry-After": {"30"}, "X-Ratelimit-Requests-Reset": {strconv.FormatInt(now.Add(20*time.Minute).UnixMilli(), 10)}}, 20 * time.Minute},
		"both, retry later": {http.Header{"Retry-After": {"600"}, "X-Ratelimit-Requests-Reset": {strconv.FormatInt(now.Add(time.Minute).UnixMilli(), 10)}}, 10 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			g := &LinearRateGate{now: func() time.Time { return now }}
			if got := g.limited(tc.header).Sub(now); got != tc.want {
				t.Fatalf("pause = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLinearBoardDrainStopsWhilePausedAndResumes(t *testing.T) {
	now := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)
	gate := &LinearRateGate{now: func() time.Time { return now }}
	gate.limited(http.Header{})
	boards := &countingBoardStore{}
	w := LinearBoardWorker{Store: boards, RateGate: gate}
	w.Drain(context.Background())
	if boards.calls != 0 {
		t.Fatalf("drain processed %d outbox rows while paused, want 0", boards.calls)
	}
	// Run calls Drain on every tick, so the next tick after the reset resumes.
	now = now.Add(linearRatePauseDefault + time.Second)
	w.Drain(context.Background())
	if boards.calls != 1 {
		t.Fatalf("drain processed %d outbox rows after the reset, want 1", boards.calls)
	}
}

type countingBoardStore struct {
	LinearBoardStore
	calls int
}

func (s *countingBoardStore) ProcessNextLinearBoardOutbox(context.Context, func(context.Context, store.LinearOutbox) (store.LinearOutboxResult, error)) (bool, error) {
	s.calls++
	return false, nil
}

func TestLinearDecisionOpsDrainKeepsAttemptsWhilePaused(t *testing.T) {
	now := time.Date(2026, 11, 3, 4, 0, 0, 0, time.UTC)
	gate := &LinearRateGate{now: func() time.Time { return now }}
	gate.limited(http.Header{})
	ops := &countingDecisionOps{}
	w := LinearDecisionOpsWorker{Store: ops, RateGate: gate}
	w.Drain(context.Background())
	if ops.calls != 0 {
		t.Fatalf("drain claimed %d decision ops while paused, want 0", ops.calls)
	}
	now = now.Add(linearRatePauseDefault + time.Second)
	w.Drain(context.Background())
	if ops.calls != 1 {
		t.Fatalf("drain claimed %d decision ops after the reset, want 1", ops.calls)
	}
}

type countingDecisionOps struct{ calls int }

func (s *countingDecisionOps) ProcessNextLinearDecisionOp(context.Context, func(context.Context, store.LinearDecisionOp) error) (bool, error, error) {
	s.calls++
	return false, nil, nil
}
