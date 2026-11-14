package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

// recordingPasses stands in for *store.Store so the scheduled pass can be
// tested without a database.
type recordingPasses struct {
	calls []string
	ytErr error
}

func (r *recordingPasses) EnqueueYTWriteOpAlerts(ctx context.Context, now time.Time) (int, error) {
	r.calls = append(r.calls, "yt_write_op")
	return 0, r.ytErr
}

func (r *recordingPasses) EnqueueDeployLagNotices(ctx context.Context, now time.Time) (int, error) {
	r.calls = append(r.calls, "deploy_lag")
	return 0, nil
}

// The scheduled alert tick must reach the deploy-lag notifier even when the
// YouTrack pass before it fails.
func TestRunOpsAlertPassesReachesDeployLag(t *testing.T) {
	p := &recordingPasses{ytErr: errors.New("boom")}
	runOpsAlertPasses(t.Context(), p, time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC))
	if len(p.calls) != 2 || p.calls[0] != "yt_write_op" || p.calls[1] != "deploy_lag" {
		t.Fatalf("passes ran %v; want [yt_write_op deploy_lag]", p.calls)
	}
}
