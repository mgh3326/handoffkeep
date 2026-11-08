package main

import (
	"context"
	"log"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

// singletonWorker keeps a standby ready to take over when the active server
// drains. The session lock is held on its own connection throughout Run.
type workerLocker interface {
	TryWorkerLock(context.Context, store.WorkerLock) (func(), bool, error)
}

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
		release, held, err := w.store.TryWorkerLock(ctx, w.id)
		if err != nil && ctx.Err() == nil {
			log.Printf("worker.lock_failed lock_id=%d err=%q", w.id, err)
		}
		if held {
			defer release()
			w.worker.Run(ctx)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
