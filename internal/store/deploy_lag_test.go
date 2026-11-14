package store

import (
	"fmt"
	"testing"
	"time"
)

func TestDeployLagCalculation(t *testing.T) {
	// Acceptance criterion: lag computation (0, 2, 3 behind)
	// Threshold: more than 2 -> lagging (0, 2 not lagging; 3 lagging)
	cases := []struct {
		lag     int
		lagging bool
	}{
		{lag: 0, lagging: false},
		{lag: 1, lagging: false},
		{lag: 2, lagging: false},
		{lag: 3, lagging: true},
		{lag: 4, lagging: true},
		{lag: 10, lagging: true},
	}

	for _, tc := range cases {
		t.Run(fmt.Sprintf("lag_%d", tc.lag), func(t *testing.T) {
			if got := IsDeployLagging(tc.lag); got != tc.lagging {
				t.Fatalf("IsDeployLagging(%d) = %v; want %v", tc.lag, got, tc.lagging)
			}
		})
	}
}

func TestDeployLagNoticeEventIDAndText(t *testing.T) {
	at := time.Date(2026, 11, 13, 15, 30, 0, 0, time.UTC)
	eventID := DeployLagNoticeEventID("handoffkeep", at)
	wantID := "deploy-lag-handoffkeep-2026-11-13"
	if eventID != wantID {
		t.Fatalf("DeployLagNoticeEventID = %q; want %q", eventID, wantID)
	}

	text := DeployLagNoticeText("handoffkeep", 5)
	if text != "배포 지연 경고: handoffkeep 서비스가 main보다 5개 변경사항 뒤처져 있습니다." {
		t.Fatalf("DeployLagNoticeText = %q", text)
	}
}

func TestDeployLagEnqueueAndNoticeDB(t *testing.T) {
	s := schemaStore(t, "deploy_lag")
	ctx := t.Context()
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

	// Configure deploy service
	_, err := s.PutDeployService(ctx, DeployService{
		Name:           "hk-test",
		Repo:           "mgh3326/hk-test",
		RecordPrefixes: []string{"deploy/hk-test/"},
		Active:         true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Deploy time: 2026-11-13 08:00:00 UTC
	depTime := now.Add(-time.Hour)
	depDoc := fmt.Sprintf(`{"schema":"deploy-record/v0","service":"hk-test","result":"success","deployed_at":"%s"}`, depTime.Format(time.RFC3339))
	if _, err := s.pool.Exec(ctx, `INSERT INTO documents(key, kind, session, job, body, sha256, created_by, created_at, updated_at) VALUES($1, 'report', '', '', $2, 'sha1', 'test', $3, $3)`,
		"deploy/hk-test/20261113T080000Z", depDoc, depTime); err != nil {
		t.Fatal(err)
	}

	// Insert changes before deploy (already deployed, should not count)
	if _, err := s.pool.Exec(ctx, `INSERT INTO changes(repo, pr_number, state, merged_at, opened_at, created_at, updated_at) VALUES($1, 1, 'merged', $2, $2, $2, $2)`, "mgh3326/hk-test", depTime.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Check 0 lag
	count, err := s.CountMergedChangesSince(ctx, "mgh3326/hk-test", depTime)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 || IsDeployLagging(count) {
		t.Fatalf("expected count 0, got %d (lagging=%v)", count, IsDeployLagging(count))
	}

	// Enqueue notices with 0 lag -> 0 enqueued
	n, err := s.EnqueueDeployLagNotices(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 enqueued, got %d", n)
	}

	// Add 2 merged changes after deploy -> lag = 2 (not lagging)
	for i := 2; i <= 3; i++ {
		if _, err := s.pool.Exec(ctx, `INSERT INTO changes(repo, pr_number, state, merged_at, opened_at, created_at, updated_at) VALUES($1, $2, 'merged', $3, $3, $3, $3)`, "mgh3326/hk-test", i, depTime.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	count, err = s.CountMergedChangesSince(ctx, "mgh3326/hk-test", depTime)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 || IsDeployLagging(count) {
		t.Fatalf("expected count 2 (lagging=false), got %d (lagging=%v)", count, IsDeployLagging(count))
	}

	n, err = s.EnqueueDeployLagNotices(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("expected 0 enqueued when lag=2, got %d", n)
	}

	// Add 1 more merged change -> lag = 3 (lagging!)
	if _, err := s.pool.Exec(ctx, `INSERT INTO changes(repo, pr_number, state, merged_at, opened_at, created_at, updated_at) VALUES($1, 4, 'merged', $2, $2, $2, $2)`, "mgh3326/hk-test", depTime.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	count, err = s.CountMergedChangesSince(ctx, "mgh3326/hk-test", depTime)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || !IsDeployLagging(count) {
		t.Fatalf("expected count 3 (lagging=true), got %d (lagging=%v)", count, IsDeployLagging(count))
	}

	// Enqueue notices: should enqueue 1 row to operator-desk
	n, err = s.EnqueueDeployLagNotices(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 enqueued, got %d", n)
	}

	// Verify notification_outbox row
	var outboxKind, targetLane, eventID, outboxText string
	if err := s.pool.QueryRow(ctx, `SELECT kind, target_lane, event_id, text FROM notification_outbox WHERE event_id=$1`, DeployLagNoticeEventID("hk-test", now)).Scan(&outboxKind, &targetLane, &eventID, &outboxText); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	if outboxKind != OutboxKindDeployLag {
		t.Fatalf("outboxKind=%q, want %q", outboxKind, OutboxKindDeployLag)
	}
	if targetLane != "operator-desk" {
		t.Fatalf("targetLane=%q, want operator-desk", targetLane)
	}
	if eventID != "deploy-lag-hk-test-2026-11-13" {
		t.Fatalf("eventID=%q", eventID)
	}

	// Calling EnqueueDeployLagNotices again on the SAME day -> 0 new enqueued (idempotent / once a day)
	n2, err := s.EnqueueDeployLagNotices(ctx, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Fatalf("expected 0 new enqueued on same day, got %d", n2)
	}

	// Next day (2026-11-14) -> should enqueue next day's notice (1 enqueued)
	nextDay := now.Add(24 * time.Hour)
	n3, err := s.EnqueueDeployLagNotices(ctx, nextDay)
	if err != nil {
		t.Fatal(err)
	}
	if n3 != 1 {
		t.Fatalf("expected 1 enqueued on next day, got %d", n3)
	}

	var nextDayCount int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notification_outbox WHERE kind=$1 AND target_lane='operator-desk'`, OutboxKindDeployLag).Scan(&nextDayCount); err != nil {
		t.Fatal(err)
	}
	if nextDayCount != 2 {
		t.Fatalf("expected 2 total notices in outbox, got %d", nextDayCount)
	}
}
