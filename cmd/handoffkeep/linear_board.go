package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mgh3326/handoffkeep/internal/api"
	"github.com/mgh3326/handoffkeep/internal/store"
)

func linearBoardFromEnv(st *store.Store, linear api.LinearAPI) backgroundWorker {
	if strings.TrimSpace(os.Getenv("HANDOFFKEEP_LINEAR_WORK_BOARD")) != "on" {
		return nil
	}
	if linear.Key == "" && linear.App == nil {
		log.Printf("linear_board.disabled reason=no_credential")
		return nil
	}
	st.EnableLinearBoard()
	return api.LinearBoardWorker{Store: st, Linear: linear, TaskBaseURL: strings.TrimSpace(os.Getenv("HANDOFFKEEP_LINEAR_WORK_BOARD_BASE_URL"))}
}

type placementRetentionWorker struct{ store *store.Store }

func (w placementRetentionWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		deleted, err := w.store.PrunePlacements(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("placement.prune_failed err=%q", err)
		} else if deleted > 0 {
			log.Printf("placement.pruned rows=%d", deleted)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
