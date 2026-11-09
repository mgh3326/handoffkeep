package main

import (
	"context"
	"log"
	"time"

	"github.com/mgh3326/handoffkeep/internal/attachments"
)

// attachmentStagingSweepWorker deletes direct-upload staging objects that were
// never completed. An hourly pass is enough: a staging object is only swept
// after attachments.StagingSweepAge, which is well past any upload's expiry.
type attachmentStagingSweepWorker struct{ am *attachments.Manager }

func (w attachmentStagingSweepWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		deleted, err := w.am.SweepStaging(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("attachments.staging_sweep_failed err=%q", err)
		} else if deleted > 0 {
			log.Printf("attachments.staging_swept objects=%d", deleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
