package main

import (
	"context"
	"log"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// tableStatsWorker takes the daily MySQL table snapshot (hk 6231). It runs
// under singletonWorker, so one serve process at a time takes it. The store
// decides whether a snapshot is due from the document's history, so a restart
// neither repeats nor skips a day.
type tableStatsWorker struct {
	store    tableStatsRunner
	interval time.Duration
	now      func() time.Time
	// retryAt holds the next attempt after a failure, so a broken read is
	// logged hourly rather than every tick.
	retryAt time.Time
}

// tableStatsRunner is the store surface the snapshot needs.
type tableStatsRunner interface {
	RunTableStats(ctx context.Context, now time.Time) (store.TableStatsRun, error)
}

const (
	// tableStatsWorkerStartDelay lets the server settle before the first check,
	// so a redeploy that finds the newest snapshot a day old runs it promptly.
	tableStatsWorkerStartDelay = time.Minute
	tableStatsWorkerInterval   = 5 * time.Minute
	tableStatsWorkerRetry      = time.Hour
)

func (w *tableStatsWorker) Run(ctx context.Context) {
	if w.store == nil {
		return
	}
	interval := w.interval
	if interval <= 0 {
		interval = tableStatsWorkerInterval
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(tableStatsWorkerStartDelay):
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		w.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick runs one due check. It logs one table_stats.snapshot line when a
// snapshot was taken, and table_stats.snapshot_failed on an error.
func (w *tableStatsWorker) tick(ctx context.Context) {
	now := time.Now
	if w.now != nil {
		now = w.now
	}
	t := now().UTC()
	if t.Before(w.retryAt) {
		return
	}
	run, err := w.store.RunTableStats(ctx, t)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.retryAt = t.Add(tableStatsWorkerRetry)
		log.Printf("table_stats.snapshot_failed err=%q", err.Error())
		return
	}
	if run.Ran {
		log.Printf("table_stats.snapshot tables=%d ms=%d", run.Tables, run.Elapsed.Milliseconds())
	}
}
