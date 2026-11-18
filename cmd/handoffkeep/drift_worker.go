package main

import (
	"context"
	"log"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// driftWorker is the daily drift and hygiene delivery (hk 5112 track B-3):
// TaskDrift (merged_days 2) and TaskHygiene (dry-run), one task_drift outbox
// row per lane with rows plus the operator-desk summary, and the
// metrics/waits/<date> document. It never changes a task.
//
// It ticks every 5 minutes and delivers once on the first tick at or after
// 09:00 KST. The metrics document is the day's marker: a day whose document
// exists is already delivered, so a redeploy neither skips nor repeats it.
type driftWorker struct {
	store    driftDeliverer
	interval time.Duration
	now      func() time.Time
	lastDay  string
}

// driftDeliverer is the store surface the daily delivery needs.
type driftDeliverer interface {
	RunDriftDelivery(ctx context.Context, now time.Time) (bool, error)
}

const driftWorkerInterval = 5 * time.Minute

func (w *driftWorker) Run(ctx context.Context) {
	if w.store == nil {
		return
	}
	interval := w.interval
	if interval <= 0 {
		interval = driftWorkerInterval
	}
	now := w.now
	if now == nil {
		now = time.Now
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if t := now().UTC(); store.DriftDue(t) && w.lastDay != store.DriftDayOf(t) {
			day := store.DriftDayOf(t)
			ran, err := w.store.RunDriftDelivery(ctx, t)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("drift.delivery_failed day=%s err=%q", day, err.Error())
			} else {
				w.lastDay = day
				if ran {
					log.Printf("drift.delivered day=%s", day)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
