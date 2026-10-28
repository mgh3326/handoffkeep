package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

const quotaTestKey = "codex:0123456789abcdef"
const holderA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const holderB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestQuotaValidation(t *testing.T) {
	for _, key := range []string{"codex:raw-account-identity", "codex:token", "codex:0123456789abcdef/path", ""} {
		if ValidQuotaKey(key) {
			t.Fatalf("accepted %q", key)
		}
	}
	x := QuotaObservation{Kind: "auto", MeasuredAt: time.Now(), Buckets: []QuotaBucket{{Window: "5h", UsedPct: 25, Scope: "account"}}}
	if !ValidQuotaObservation(x, time.Now()) {
		t.Fatal("valid refused")
	}
	x.Buckets[0].UsedPct = 101
	if ValidQuotaObservation(x, time.Now()) {
		t.Fatal("invalid percent accepted")
	}
}
func TestQuotaPostgres(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	if err := s.migrate(ctx); err != nil {
		t.Fatal("idempotent migration", err)
	}
	latest, err := s.LatestQuota(ctx, quotaTestKey)
	if err != nil || latest.Observation != nil || len(latest.Manual) != 0 {
		t.Fatalf("empty %+v %v", latest, err)
	}
	var wg sync.WaitGroup
	winners := make(chan string, 2)
	for _, h := range []string{holderA, holderB} {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			x, e := s.LeaseQuota(ctx, quotaTestKey, "client", h, 120)
			if e != nil {
				t.Error(e)
			} else if x.Acquired {
				winners <- h
			}
		}(h)
	}
	wg.Wait()
	close(winners)
	var holder string
	n := 0
	for h := range winners {
		holder = h
		n++
	}
	if n != 1 {
		t.Fatalf("winners=%d", n)
	}
	other := holderA
	if holder == holderA {
		other = holderB
	}
	x := QuotaObservation{Kind: "auto", MeasuredAt: time.Now().UTC(), Buckets: []QuotaBucket{{Window: "5h", UsedPct: 25, Scope: "account"}}}
	if _, _, err = s.PutQuota(ctx, quotaTestKey, "client", other, x); !errors.Is(err, ErrQuotaLease) {
		t.Fatalf("nonholder: %v", err)
	}
	if _, _, err = s.PutQuota(ctx, quotaTestKey, "different-client", holder, x); !errors.Is(err, ErrQuotaLease) {
		t.Fatalf("owner check: %v", err)
	}
	first, deduped, err := s.PutQuota(ctx, quotaTestKey, "client", holder, x)
	if err != nil {
		t.Fatal(err)
	}
	if deduped {
		t.Fatal("first publish deduped")
	}
	x.MeasuredAt = x.MeasuredAt.Add(-time.Minute)
	x.Buckets[0].UsedPct = 10
	if _, _, err = s.PutQuota(ctx, quotaTestKey, "client", holder, x); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	m := QuotaObservation{Kind: "manual", Window: "7d", MeasuredAt: time.Now(), ExpiresAt: &expiry, Buckets: []QuotaBucket{{Window: "7d", UsedPct: 42, Scope: "account"}}}
	if _, _, err = s.PutQuota(ctx, quotaTestKey, "client", holder, m); err != nil {
		t.Fatal(err)
	}
	latest, err = s.LatestQuota(ctx, quotaTestKey)
	if err != nil || latest.Observation.ID != first.ID || len(latest.Manual) != 1 {
		t.Fatalf("latest %+v %v", latest, err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM quota_observations`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("history=%d %v", count, err)
	}
	var v22 bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=22)`).Scan(&v22); err != nil || !v22 {
		t.Fatalf("quota schema_version row missing: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE quota_observations SET window_key=window_key`); err == nil {
		t.Fatal("history mutable")
	}
	renewed, err := s.LeaseQuota(ctx, quotaTestKey, "client", holder, 120)
	if err != nil || !renewed.Acquired {
		t.Fatalf("renew %+v %v", renewed, err)
	}
	if err = s.ReleaseQuota(ctx, quotaTestKey, "client", other); err != nil {
		t.Fatal(err)
	}
	blocked, err := s.LeaseQuota(ctx, quotaTestKey, "client", other, 120)
	if err != nil || blocked.Acquired || blocked.Holder != holder {
		t.Fatalf("wrong release %+v %v", blocked, err)
	}
	if _, err = pool.Exec(ctx, `UPDATE quota_leases SET expires_at=clock_timestamp()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PutQuota(ctx, quotaTestKey, "client", holder, x); !errors.Is(err, ErrQuotaLease) {
		t.Fatalf("expired publication: %v", err)
	}
	takeover, err := s.LeaseQuota(ctx, quotaTestKey, "client", other, 120)
	if err != nil || !takeover.Acquired {
		t.Fatalf("takeover %+v %v", takeover, err)
	}
	if err = s.ReleaseQuota(ctx, quotaTestKey, "client", holder); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.PutQuota(ctx, quotaTestKey, "client", holder, x); !errors.Is(err, ErrQuotaLease) {
		t.Fatalf("old holder: %v", err)
	}
	if _, _, err = s.PutQuota(ctx, quotaTestKey, "client", other, x); err != nil {
		t.Fatal(err)
	}
	if err = s.ReleaseQuota(ctx, quotaTestKey, "client", other); err != nil {
		t.Fatal(err)
	}
	acquired, err := s.LeaseQuota(ctx, quotaTestKey, "client", holder, 120)
	if err != nil || !acquired.Acquired {
		t.Fatalf("released %+v %v", acquired, err)
	}
}
func TestQuotaDedupePostgres(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	const key = "claude:fedcba9876543210"
	if _, err := s.LeaseQuota(ctx, key, "client", holderA, 120); err != nil {
		t.Fatal(err)
	}
	x := QuotaObservation{Kind: "auto", MeasuredAt: time.Now().UTC(), Buckets: []QuotaBucket{{Window: "5h", UsedPct: 30, Scope: "account"}}}
	first, deduped, err := s.PutQuota(ctx, key, "client", holderA, x)
	if err != nil || deduped {
		t.Fatalf("first publish deduped=%v %v", deduped, err)
	}
	again, deduped, err := s.PutQuota(ctx, key, "client", holderA, x)
	if err != nil || !deduped || again.ID != first.ID {
		t.Fatalf("dedupe: id=%d want=%d deduped=%v err=%v", again.ID, first.ID, deduped, err)
	}
	// A newer measured_at on the same value dedupes and advances confirmed_at.
	newer := x
	newer.MeasuredAt = x.MeasuredAt.Add(time.Minute)
	confirmed, deduped, err := s.PutQuota(ctx, key, "client", holderA, newer)
	if err != nil || !deduped || confirmed.ID != first.ID {
		t.Fatalf("dedupe freshness: id=%d deduped=%v %v", confirmed.ID, deduped, err)
	}
	if confirmed.ConfirmedAt == nil || !confirmed.ConfirmedAt.Equal(newer.MeasuredAt) {
		t.Fatalf("dedupe freshness confirmed_at=%v want=%v", confirmed.ConfirmedAt, newer.MeasuredAt)
	}
	latestQ, err := s.LatestQuota(ctx, key)
	if err != nil || latestQ.Observation == nil || latestQ.Observation.ConfirmedAt == nil || !latestQ.Observation.ConfirmedAt.Equal(newer.MeasuredAt) {
		t.Fatalf("dedupe freshness latest=%+v %v", latestQ.Observation, err)
	}
	// An older measured_at on the same value dedupes without moving confirmed_at.
	older := x
	older.MeasuredAt = x.MeasuredAt.Add(-time.Minute)
	if got, deduped, err := s.PutQuota(ctx, key, "client", holderA, older); err != nil || !deduped || !got.ConfirmedAt.Equal(newer.MeasuredAt) {
		t.Fatalf("dedupe freshness older: confirmed_at=%v deduped=%v %v", got.ConfirmedAt, deduped, err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM quota_observations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dedupe history=%d %v", count, err)
	}
	changed := x
	changed.Buckets[0].UsedPct = 31
	if next, deduped, err := s.PutQuota(ctx, key, "client", holderA, changed); err != nil || deduped || next.ID == first.ID {
		t.Fatalf("dedupe change: %+v deduped=%v %v", next, deduped, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM quota_observations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("dedupe history=%d %v", count, err)
	}
	// Concurrent identical publishes under one holder must not double-append.
	var wg sync.WaitGroup
	results := make(chan QuotaObservation, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, e := s.PutQuota(ctx, key, "client", holderA, changed)
			if e != nil {
				t.Error(e)
				return
			}
			results <- got
		}()
	}
	wg.Wait()
	close(results)
	var ids []int64
	for got := range results {
		ids = append(ids, got.ID)
	}
	if len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("dedupe concurrent ids=%v", ids)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM quota_observations`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("dedupe concurrent history=%d %v", count, err)
	}
}
