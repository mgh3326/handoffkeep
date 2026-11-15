package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
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

// seedDeployDocument writes one raw deploy record document.
func seedDeployDocument(t *testing.T, s *Store, key, body string, at time.Time) {
	t.Helper()
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO documents(key, kind, session, job, body, sha256, created_by, created_at, updated_at) VALUES($1, 'report', '', '', $2, $3, 'test', $4, $4)`,
		key, body, "sha-"+key, at); err != nil {
		t.Fatal(err)
	}
}

// seedDeployRecord writes a valid deploy record for service at at, keyed by
// the contract timestamp.
func seedDeployRecord(t *testing.T, s *Store, service, result string, at time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"schema":"deploy-record/v0","service":%q,"result":%q,"deployed_at":%q}`, service, result, at.UTC().Format(time.RFC3339))
	seedDeployDocument(t, s, "deploy/"+service+"/"+at.UTC().Format("20060102T150405Z"), body, at)
}

func lastSuccessMap(t *testing.T, s *Store) map[string]time.Time {
	t.Helper()
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	got, err := readProjectLastSuccessfulDeploysTx(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A run of newer failed records longer than one scan page must not hide the
// last success: the boundary is found by paging, and a service whose whole
// prefix has no success still reports "never deployed" and terminates.
func TestLastSuccessScanPagesPastNewerFailures(t *testing.T) {
	s := schemaStore(t, "deploy_lag_pages")
	ctx := t.Context()
	success := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	failuresFrom := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)

	// Counts straddle the page size: a page of exactly lastSuccessPageSize
	// failures must still continue to the next page, and more than two pages
	// must keep going.
	counts := []int{lastSuccessPageSize, lastSuccessPageSize + 1, 2*lastSuccessPageSize + 5}
	for _, failures := range counts {
		withSuccess := fmt.Sprintf("hk-deep-%d", failures)
		noSuccess := fmt.Sprintf("hk-none-%d", failures)
		for _, name := range []string{withSuccess, noSuccess} {
			if _, err := s.PutDeployService(ctx, DeployService{Name: name, Repo: "mgh3326/" + name, RecordPrefixes: []string{"deploy/" + name + "/"}, Active: true}); err != nil {
				t.Fatal(err)
			}
		}
		seedDeployRecord(t, s, withSuccess, "success", success)
		for i := 0; i < failures; i++ {
			at := failuresFrom.Add(time.Duration(i) * time.Second)
			seedDeployRecord(t, s, withSuccess, "failed", at)
			seedDeployRecord(t, s, noSuccess, "failed", at)
		}
	}

	got := lastSuccessMap(t, s)
	for _, failures := range counts {
		withSuccess := fmt.Sprintf("hk-deep-%d", failures)
		noSuccess := fmt.Sprintf("hk-none-%d", failures)
		if at, ok := got[withSuccess]; !ok || !at.Equal(success) {
			t.Fatalf("%d newer failures: last success = (%s, %v), want %s", failures, at, ok, success)
		}
		if at, ok := got[noSuccess]; ok {
			t.Fatalf("%d failures and no success: last success = %s, want none", failures, at)
		}
	}
}

// The lag measure and the operator-desk notice read that boundary, so a deep
// run of failures after the last deploy still counts the merges behind it.
func TestDeployLagCountsMergesBehindDeepFailureRun(t *testing.T) {
	s := schemaStore(t, "deploy_lag_deep")
	ctx := t.Context()
	now := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	svc, err := s.PutDeployService(ctx, DeployService{Name: "hk-deep", Repo: "mgh3326/hk-deep", RecordPrefixes: []string{"deploy/hk-deep/"}, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	success := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	seedDeployRecord(t, s, "hk-deep", "success", success)
	for i := 0; i < lastSuccessPageSize+5; i++ {
		seedDeployRecord(t, s, "hk-deep", "failed", time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Second))
	}
	for i := 1; i <= 3; i++ {
		if _, err := s.pool.Exec(ctx, `INSERT INTO changes(repo, pr_number, state, merged_at, opened_at, created_at, updated_at) VALUES($1, $2, 'merged', $3, $3, $3, $3)`, "mgh3326/hk-deep", i, success.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	st, err := s.DeployLagStatus(ctx, svc)
	if err != nil {
		t.Fatal(err)
	}
	if !st.HasDeployed || st.DeployedAt == nil || !st.DeployedAt.Equal(success) || st.LagCount != 3 || !st.Lagging {
		t.Fatalf("lag status = %+v, want deployed at %s with 3 merges behind and lagging", st, success)
	}
	n, err := s.EnqueueDeployLagNotices(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("enqueued = %d, want 1 notice for the lagging service", n)
	}
}

// A repo base name is an alias for the project, never a replacement for a
// service of that name: DeployLagStatus and the notifier read by service name.
func TestLastSuccessRepoAliasKeepsServiceBoundary(t *testing.T) {
	s := schemaStore(t, "deploy_lag_alias")
	ctx := t.Context()
	alphaAt := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	betaAt := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	for _, svc := range []DeployService{
		{Name: "alpha", Repo: "org/alpha", RecordPrefixes: []string{"deploy/alpha/"}, Active: true},
		// beta sorts after alpha and its repo base name is alpha's service name.
		{Name: "beta", Repo: "org/alpha", RecordPrefixes: []string{"deploy/beta/"}, Active: true},
		{Name: "gamma", Repo: "org/gamma-repo", RecordPrefixes: []string{"deploy/gamma/"}, Active: true},
	} {
		if _, err := s.PutDeployService(ctx, svc); err != nil {
			t.Fatal(err)
		}
	}
	seedDeployRecord(t, s, "alpha", "success", alphaAt)
	seedDeployRecord(t, s, "beta", "success", betaAt)
	seedDeployRecord(t, s, "gamma", "success", alphaAt)

	got := lastSuccessMap(t, s)
	if at := got["alpha"]; !at.Equal(alphaAt) {
		t.Fatalf("alpha boundary = %s, want its own %s (beta's repo base must not replace it)", at, alphaAt)
	}
	if at := got["beta"]; !at.Equal(betaAt) {
		t.Fatalf("beta boundary = %s, want %s", at, betaAt)
	}
	if at, ok := got["gamma-repo"]; !ok || !at.Equal(alphaAt) {
		t.Fatalf("gamma-repo alias = (%s, %v), want gamma's %s: an alias with no colliding service keeps working", at, ok, alphaAt)
	}
}

// EnqueueDeployLagNotices holds one connection for its whole pass, so it reads
// the services through that transaction. A pool of one connection would
// otherwise wait on itself until the context ends.
func TestEnqueueDeployLagNoticesNeedsOneConnection(t *testing.T) {
	_, url := testdb.Schema(t, "deploy_lag_pool")
	// Migrate with a normal pool first, so the pool of one only serves the pass.
	migrated, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	migrated.Close()
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	s, err := Open(t.Context(), url+sep+"pool_max_conns=1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.PutDeployService(t.Context(), DeployService{Name: "hk-one", Repo: "mgh3326/hk-one", RecordPrefixes: []string{"deploy/hk-one/"}, Active: true}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := s.EnqueueDeployLagNotices(ctx, time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("EnqueueDeployLagNotices on a one-connection pool: %v", err)
	}
}
