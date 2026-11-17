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
	if err := s.ReleaseQuota(ctx, quotaTestKey, "client", winner); err != nil {
		t.Fatal(err)
	}
}
