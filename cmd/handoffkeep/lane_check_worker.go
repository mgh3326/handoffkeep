package main

import (
	"context"
	"log"
	"time"
)

// laneCheckWorker is the per-lane wake check (operator decision 418, hk 5201;
// design design/2026-11-18/hk5112-track-b §10). It finds the design lanes whose
// backlog needs a look and queues one lane_check notice per lane through the
// outbox, which the forwarder delivers as hub type info. It starts nothing and
// changes no task: the design session judges what to do on the wake.
//
// The notice's event id carries the findings hash and a 6-hour bucket, so an
// unchanged state is not repeated inside a bucket and a lane's recorded look
// stops it until the state changes.
type laneCheckWorker struct {
	store    laneCheckRunner
	interval time.Duration
	now      func() time.Time
}

// laneCheckRunner is the store surface the check needs.
type laneCheckRunner interface {
	RunLaneCheck(ctx context.Context, now time.Time) (int, error)
}

const laneCheckWorkerInterval = 15 * time.Minute

func (w *laneCheckWorker) Run(ctx context.Context) {
	if w.store == nil {
		return
	}
	interval := w.interval
	if interval <= 0 {
		interval = laneCheckWorkerInterval
	}
	now := w.now
	if now == nil {
		now = time.Now
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		// A run at start keeps a redeploy from delaying the first check by a
		// whole tick; the deterministic event ids make a repeated run cheap.
		if wrote, err := w.store.RunLaneCheck(ctx, now().UTC()); err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("lane_check.failed err=%q", err.Error())
		} else if wrote > 0 {
			log.Printf("lane_check.notices_written count=%d", wrote)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
