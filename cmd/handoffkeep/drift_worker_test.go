package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// fakeDriftStore replays the metrics-doc marker: the first delivery of a day
// runs, later ones report already delivered.
type fakeDriftStore struct {
	mu        sync.Mutex
	calls     int
	delivered map[string]bool
}

func (f *fakeDriftStore) RunDriftDelivery(_ context.Context, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	day := store.DriftDayOf(now.UTC())
	if f.delivered == nil {
		f.delivered = map[string]bool{}
	}
	if f.delivered[day] {
		return false, nil
	}
	f.delivered[day] = true
	return true, nil
}

func (f *fakeDriftStore) trueCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ran := range f.delivered {
		if ran {
			n++
		}
	}
	return n
}

// kstTime is 2026-11-18 at hh:mm KST.
func kstTime(hh, mm int) time.Time {
	return time.Date(2026, 11, 18, hh, mm, 0, 0, time.FixedZone("KST", 9*3600))
}

// TestDriftWorkerDeliversOnceAcrossRedeploys runs two worker instances — a
// redeploy — against one marker: only the first tick at or after 09:00 KST
// delivers, and nothing runs before 09:00.
func TestDriftWorkerDeliversOnceAcrossRedeploys(t *testing.T) {
	fake := &fakeDriftStore{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := &driftWorker{store: fake, interval: 10 * time.Millisecond, now: func() time.Time { return kstTime(10, 0) }}
			w.Run(ctx)
		}()
	}
	wg.Wait()
	if got := fake.trueCalls(); got != 1 {
		t.Fatalf("deliveries = %d, want exactly 1 across the redeploy", got)
	}

	early := &fakeDriftStore{}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := &driftWorker{store: early, interval: 10 * time.Millisecond, now: func() time.Time { return kstTime(8, 30) }}
		w.Run(ctx2)
	}()
	<-done
	early.mu.Lock()
	defer early.mu.Unlock()
	if early.calls != 0 {
		t.Fatalf("calls before 09:00 KST = %d, want 0", early.calls)
	}
}
