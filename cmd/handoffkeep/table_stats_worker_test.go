package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type fakeTableStats struct {
	calls []time.Time
	fail  bool
}

func (f *fakeTableStats) RunTableStats(_ context.Context, now time.Time) (store.TableStatsRun, error) {
	f.calls = append(f.calls, now)
	if f.fail {
		return store.TableStatsRun{}, errors.New("read failed")
	}
	return store.TableStatsRun{Ran: true, Tables: 2}, nil
}

// A failed snapshot waits an hour before the next attempt, so a broken read
// is retried without being hammered every tick.
func TestTableStatsWorkerBacksOffAfterAFailure(t *testing.T) {
	fake := &fakeTableStats{fail: true}
	start := time.Date(2026, 11, 24, 3, 0, 0, 0, time.UTC)
	w := &tableStatsWorker{store: fake, now: func() time.Time { return start }}

	w.tick(context.Background())
	w.now = func() time.Time { return start.Add(10 * time.Minute) }
	w.tick(context.Background())
	if len(fake.calls) != 1 {
		t.Fatalf("calls during the back-off = %d, want 1", len(fake.calls))
	}

	fake.fail = false
	w.now = func() time.Time { return start.Add(tableStatsWorkerRetry + time.Second) }
	w.tick(context.Background())
	if len(fake.calls) != 2 || !fake.calls[1].Equal(start.Add(tableStatsWorkerRetry+time.Second)) {
		t.Fatalf("retry after the back-off did not run at its time: %v", fake.calls)
	}
}
