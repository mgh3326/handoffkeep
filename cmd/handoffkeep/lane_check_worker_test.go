package main

import (
	"context"
	"testing"
	"time"
)

type laneCheckRecorder struct {
	ran chan time.Time
}

func (r laneCheckRecorder) RunLaneCheck(_ context.Context, now time.Time) (int, error) {
	r.ran <- now
	return 1, nil
}

// TestLaneCheckWorkerRunsAtStart pins the worker's first run: a redeploy must
// not delay the check by a whole 15-minute tick.
func TestLaneCheckWorkerRunsAtStart(t *testing.T) {
	ran := make(chan time.Time, 1)
	w := &laneCheckWorker{store: laneCheckRecorder{ran: ran}, interval: time.Hour, now: func() time.Time {
		return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	select {
	case at := <-ran:
		if !at.Equal(time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("run at %s", at)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not run at start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not stop on cancel")
	}
}
