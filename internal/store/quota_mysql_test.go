//go:build mysql

package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/testdb"
)

func TestMySQLQuota(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := t.Context()
	var wg sync.WaitGroup
	winners := make(chan string, 2)
	for _, holder := range []string{holderA, holderB} {
		wg.Add(1)
		go func(holder string) {
			defer wg.Done()
			lease, err := s.LeaseQuota(ctx, quotaTestKey, "client", holder, 120)
			if err != nil {
				t.Error(err)
			} else if lease.Acquired {
				winners <- holder
			}
		}(holder)
	}
	wg.Wait()
	close(winners)
	var winner string
	n := 0
	for holder := range winners {
		winner = holder
		n++
	}
	if n != 1 {
		t.Fatalf("lease winners = %d, want 1", n)
	}
	loser := holderA
	if winner == holderA {
		loser = holderB
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	obs := QuotaObservation{Kind: "auto", MeasuredAt: now, Buckets: []QuotaBucket{{Window: "5h", UsedPct: 25, Scope: "account"}}}
	if _, _, err := s.PutQuota(ctx, quotaTestKey, "client", loser, obs); !errors.Is(err, ErrQuotaLease) {
		t.Fatalf("losing lease: %v", err)
	}
	first, deduped, err := s.PutQuota(ctx, quotaTestKey, "client", winner, obs)
	if err != nil || deduped {
		t.Fatalf("first put: %+v deduped=%v err=%v", first, deduped, err)
	}
	obs.MeasuredAt = obs.MeasuredAt.Add(time.Second)
	again, deduped, err := s.PutQuota(ctx, quotaTestKey, "client", winner, obs)
	if err != nil || !deduped || again.ID != first.ID || again.ConfirmedAt == nil || !again.ConfirmedAt.Equal(obs.MeasuredAt) {
		t.Fatalf("dedupe: %+v deduped=%v err=%v", again, deduped, err)
	}
	latest, err := s.LatestQuota(ctx, quotaTestKey)
	if err != nil || latest.Observation == nil || latest.Observation.ID != first.ID {
		t.Fatalf("latest: %+v %v", latest, err)
	}
	history, found, err := s.QuotaObservationHistory(ctx, quotaTestKey, now.Add(-time.Second), now.Add(time.Minute), 10)
	if err != nil || !found || len(history) != 1 {
		t.Fatalf("history: %+v found=%v err=%v", history, found, err)
	}
	presence := QuotaPresence{HostID: "host-a", AccountKey: quotaTestKey, LastSeen: now}
	if _, err := s.PutQuotaPresence(ctx, presence); err != nil {
		t.Fatal(err)
	}
	older := presence
	older.LastSeen = now.Add(-time.Minute)
	got, err := s.PutQuotaPresence(ctx, older)
	if err != nil || !got.LastSeen.Equal(now) {
		t.Fatalf("presence moved backwards: %+v %v", got, err)
	}
	listed, truncated, err := s.QuotaPresence(ctx, "", quotaTestKey, 10)
	if err != nil || truncated || len(listed) != 1 {
		t.Fatalf("presence list: %+v truncated=%v err=%v", listed, truncated, err)
	}
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE quota_observations SET window_key=window_key`); err == nil {
		t.Fatal("history UPDATE was accepted")
	}
	if _, err := s.mysqlPool.ExecContext(ctx, `DELETE FROM quota_observations WHERE id=?`, first.ID); err == nil {
		t.Fatal("history DELETE was accepted")
	}
	if _, err := s.PruneQuotaHistory(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseQuota(ctx, quotaTestKey, "client", loser); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.LeaseQuota(ctx, quotaTestKey, "client", loser, 120)
	if err != nil || blocked.Acquired || blocked.Holder != winner {
		t.Fatalf("wrong release: %+v %v", blocked, err)
	}
	if _, err := s.mysqlPool.ExecContext(ctx, `UPDATE quota_leases SET expires_at=DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 1 SECOND) WHERE account_key=?`, quotaTestKey); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutQuota(ctx, quotaTestKey, "client", winner, obs); !errors.Is(err, ErrQuotaLease) {
		t.Fatalf("expired publisher: %v", err)
	}
	takeover, err := s.LeaseQuota(ctx, quotaTestKey, "client", loser, 120)
	if err != nil || !takeover.Acquired {
		t.Fatalf("takeover: %+v %v", takeover, err)
	}
	if _, _, err := s.PutQuota(ctx, quotaTestKey, "client", winner, obs); !errors.Is(err, ErrQuotaLease) {
		t.Fatalf("old holder published: %v", err)
	}
	if err := s.ReleaseQuota(ctx, quotaTestKey, "client", loser); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLQuotaSharedBehaviors(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *Store)
	}{
		{"dedupe", testQuotaDedupe},
		{"history", testQuotaObservationHistory},
		{"presence-bounds", testQuotaPresenceBounds},
		{"presence", func(t *testing.T, s *Store) { _ = testQuotaPresence(t, s) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(t.Context(), testdb.MySQLDatabase(t))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			tc.run(t, s)
		})
	}
}

func TestMySQLQuotaRetention(t *testing.T) {
	s, err := Open(t.Context(), testdb.MySQLDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.mysqlPool.SetMaxOpenConns(1)
	ctx := t.Context()
	insert := func(old bool) int64 {
		t.Helper()
		received := time.Now().UTC()
		if old {
			received = received.Add(-100 * 24 * time.Hour)
		}
		result, err := s.mysqlPool.ExecContext(ctx, `INSERT INTO quota_observations(account_key,kind,window_key,measured_at,value,received_at) VALUES(?,'auto','',?,'{}',?)`, quotaTestKey, time.Now().UTC(), received)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	referenced, oldFree, fresh := insert(true), insert(true), insert(false)
	if _, err := s.mysqlPool.ExecContext(ctx, `INSERT INTO quota_latest(account_key,kind,window_key,observation_id,measured_at) VALUES(?,'auto','',?,?)`, quotaTestKey, referenced, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneQuotaHistory(ctx); err != nil || n != 1 {
		t.Fatalf("prune=%d err=%v", n, err)
	}
	for id, want := range map[int64]bool{referenced: true, oldFree: false, fresh: true} {
		var exists bool
		if err := s.mysqlPool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM quota_observations WHERE id=?)`, id).Scan(&exists); err != nil || exists != want {
			t.Fatalf("id=%d exists=%v want=%v err=%v", id, exists, want, err)
		}
	}
	if n, err := s.PruneQuotaHistory(ctx); err != nil || n != 0 {
		t.Fatalf("second prune=%d err=%v", n, err)
	}
	if _, err := s.mysqlPool.ExecContext(ctx, `DELETE FROM quota_observations WHERE id=?`, fresh); err == nil {
		t.Fatal("prune session flag leaked into the pool")
	}
}
