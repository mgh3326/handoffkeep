package main

import (
	"context"
	"log"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

type workerLocker interface {
	TryWorkerLock(context.Context, store.WorkerLock) (*store.WorkerLease, bool, error)
}

// singletonWorker keeps a standby ready to take over when the active server
// drains. The session lock is held on its own connection throughout Run.
type singletonWorker struct {
	store    workerLocker
	id       store.WorkerLock
	worker   backgroundWorker
	interval time.Duration
}

func (w singletonWorker) Run(ctx context.Context) {
	interval := w.interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		lease, held, err := w.store.TryWorkerLock(ctx, w.id)
		if err != nil && ctx.Err() == nil {
			log.Printf("worker.lock_failed lock_id=%d err=%q", w.id, err)
		}
		if held {
			workerCtx, cancel := context.WithCancel(ctx)
			watched := make(chan error, 1)
			go func() { err := lease.Watch(workerCtx); cancel(); watched <- err }()
			w.worker.Run(workerCtx)
			cancel()
			watchErr := <-watched
			lease.Release()
			if ctx.Err() != nil {
				return
			}
			if watchErr == context.Canceled {
				return
			}
			log.Printf("worker.lock_lost lock_id=%d err=%q", w.id, watchErr)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
