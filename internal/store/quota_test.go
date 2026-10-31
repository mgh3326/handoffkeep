package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// Antigravity meters by provider group (gemini-5h, 3p-weekly, ...);
	// pacelever publishes those buckets with scope "group".
	x.Buckets = append(x.Buckets, QuotaBucket{Window: "7d", UsedPct: 2, Scope: "group", Model: "3p-weekly"})
	if !ValidQuotaObservation(x, time.Now()) {
		t.Fatal("group-scoped bucket refused")
	}
	x.Buckets[1].Model = ""
	if ValidQuotaObservation(x, time.Now()) {
		t.Fatal("group bucket without a group name accepted")
	}
	x.Buckets[1].Model = "3p-weekly"
	x.Buckets[1].Scope = "team"
	if ValidQuotaObservation(x, time.Now()) {
		t.Fatal("unknown scope accepted")
	}
	x.Buckets = x.Buckets[:1]
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
	x := QuotaObservation{Kind: "auto", MeasuredAt: time.Now().UTC().Truncate(time.Microsecond), Buckets: []QuotaBucket{{Window: "5h", UsedPct: 25, Scope: "account"}}}
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
	var v23 bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=23)`).Scan(&v23); err != nil || !v23 {
		t.Fatalf("quota schema_version row missing: %v", err)
	}
	colExists := func(table, col string) bool {
		var ok bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=$1::regclass AND attname=$2 AND NOT attisdropped)`, table, col).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !colExists("tasks", "expected") {
		t.Fatal("v22-gated tasks.expected missing after migrate")
	}
	if !colExists("quota_latest", "confirmed_at") {
		t.Fatal("quota_latest.confirmed_at missing after migrate")
	}
	// Replay the pre-90 state: #92's v22 row and the PR 89 quota tables exist,
	// without v23 or confirmed_at — migrate must restore both idempotently.
	for _, q := range []string{
		`DELETE FROM schema_version WHERE version IN (22,23)`,
		`ALTER TABLE tasks DROP COLUMN expected`,
		`ALTER TABLE quota_latest DROP COLUMN confirmed_at`,
	} {
		if _, err = pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal("migrate on v22+PR89 state", err)
	}
	if !colExists("tasks", "expected") || !colExists("quota_latest", "confirmed_at") {
		t.Fatal("re-migrate did not restore expected or confirmed_at")
	}
	var rows int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version IN (22,23)`).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("version rows after re-migrate=%d %v", rows, err)
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
	// Base 10 min in the past so later +1/+2 min observations stay within the
	// 1-minute future-skew limit enforced by ValidQuotaObservation.
	x := QuotaObservation{Kind: "auto", MeasuredAt: time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond), Buckets: []QuotaBucket{{Window: "5h", UsedPct: 30, Scope: "account"}}}
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
	// A legacy pointer (confirmed_at NULL) never confirms below its own
	// measured_at, even when the identical republish is older.
	if _, err = pool.Exec(ctx, `UPDATE quota_latest SET confirmed_at=NULL WHERE account_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if got, deduped, err := s.PutQuota(ctx, key, "client", holderA, older); err != nil || !deduped || got.ConfirmedAt == nil || !got.ConfirmedAt.Equal(x.MeasuredAt) {
		t.Fatalf("dedupe legacy floor: confirmed_at=%v want=%v deduped=%v %v", got.ConfirmedAt, x.MeasuredAt, deduped, err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM quota_observations`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dedupe history=%d %v", count, err)
	}
	changed := x
	changed.Buckets = append([]QuotaBucket(nil), x.Buckets...) // deep copy: x shares the slice
	changed.Buckets[0].UsedPct = 31
	changed.MeasuredAt = newer.MeasuredAt.Add(time.Minute)
	// A client-supplied confirmed_at is never stored nor trusted.
	forgedAt := x.MeasuredAt.Add(-time.Hour)
	changed.ConfirmedAt = &forgedAt
	next, deduped, err := s.PutQuota(ctx, key, "client", holderA, changed)
	if err != nil || deduped || next.ID == first.ID {
		t.Fatalf("dedupe change: %+v deduped=%v %v", next, deduped, err)
	}
	var storedCA *string
	if err = pool.QueryRow(ctx, `SELECT value->>'confirmed_at' FROM quota_observations WHERE id=$1`, next.ID).Scan(&storedCA); err != nil || storedCA != nil {
		t.Fatalf("confirmed_at stored: %v %v", storedCA, err)
	}
	latestQ, err = s.LatestQuota(ctx, key)
	if err != nil || latestQ.Observation.ConfirmedAt == nil || !latestQ.Observation.ConfirmedAt.Equal(changed.MeasuredAt) {
		t.Fatalf("confirmed_at not server-owned: %+v %v", latestQ.Observation, err)
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
func TestQuotaRetentionPostgres(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	const key = "claude:fedcba9876543210"
	insert := func(measured time.Time, old bool) int64 {
		var id int64
		received := `clock_timestamp()`
		if old {
			received = `clock_timestamp()-interval '100 days'`
		}
		err := pool.QueryRow(ctx, `INSERT INTO quota_observations(account_key,kind,window_key,measured_at,value,received_at) VALUES($1,'auto','',$2,'{}'::jsonb,`+received+`) RETURNING id`, key, measured).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldReferenced := insert(time.Now(), true)
	oldFree := insert(time.Now(), true)
	freshFree := insert(time.Now(), false)
	if _, err := pool.Exec(ctx, `INSERT INTO quota_latest(account_key,kind,window_key,observation_id,measured_at) VALUES($1,'auto','',$2,$3)`, key, oldReferenced, time.Now()); err != nil {
		t.Fatal(err)
	}
	// No delete outside the retention function: an arbitrary DELETE, every
	// UPDATE and TRUNCATE all stay blocked.
	if _, err := pool.Exec(ctx, `DELETE FROM quota_observations WHERE id=$1`, oldFree); err == nil {
		t.Fatal("retention arbitrary delete allowed")
	}
	if _, err := pool.Exec(ctx, `UPDATE quota_observations SET value='{}'`); err == nil {
		t.Fatal("retention update allowed")
	}
	if _, err := pool.Exec(ctx, `TRUNCATE quota_observations`); err == nil {
		t.Fatal("retention truncate allowed")
	}
	n, err := s.PruneQuotaHistory(ctx)
	if err != nil {
		t.Fatalf("retention prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("retention pruned=%d want=1", n)
	}
	for id, want := range map[int64]bool{oldReferenced: true, oldFree: false, freshFree: true} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM quota_observations WHERE id=$1)`, id).Scan(&exists); err != nil || exists != want {
			t.Fatalf("retention referenced guard id=%d exists=%v want=%v %v", id, exists, want, err)
		}
	}
	// A second prune deletes nothing new and stays idempotent.
	if n, err = s.PruneQuotaHistory(ctx); err != nil || n != 0 {
		t.Fatalf("retention second prune=%d %v", n, err)
	}
	// The prune flag must not outlive the function inside a larger
	// transaction: a later arbitrary DELETE in the same transaction stays blocked.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT quota_prune_history()`); err != nil {
		t.Fatalf("retention prune in tx: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM quota_observations WHERE id=$1`, freshFree); err == nil {
		t.Fatal("retention prune flag leaked into the enclosing transaction")
	}
}
func TestQuotaPresenceValidation(t *testing.T) {
	now := time.Now()
	ok := QuotaPresence{HostID: "host-0123456789abcdef", AccountKey: quotaTestKey, LastSeen: now}
	if !ValidQuotaPresence(ok, now) {
		t.Fatal("valid presence refused")
	}
	long := ok
	long.HostID = strings.Repeat("a", 64)
	if !ValidQuotaPresence(long, now) {
		t.Fatal("64-char host refused")
	}
	for name, mutate := range map[string]func(*QuotaPresence){
		"empty host":     func(x *QuotaPresence) { x.HostID = "" },
		"long host":      func(x *QuotaPresence) { x.HostID = strings.Repeat("a", 65) },
		"host slash":     func(x *QuotaPresence) { x.HostID = "host/x" },
		"host space":     func(x *QuotaPresence) { x.HostID = "host x" },
		"leading dot":    func(x *QuotaPresence) { x.HostID = ".host" },
		"raw identity":   func(x *QuotaPresence) { x.AccountKey = "codex:raw-account-identity" },
		"empty key":      func(x *QuotaPresence) { x.AccountKey = "" },
		"zero last_seen": func(x *QuotaPresence) { x.LastSeen = time.Time{} },
		"future skew":    func(x *QuotaPresence) { x.LastSeen = now.Add(2 * time.Minute) },
		"past retention": func(x *QuotaPresence) { x.LastSeen = now.Add(-QuotaPresenceRetention - time.Minute) },
	} {
		x := ok
		mutate(&x)
		if ValidQuotaPresence(x, now) {
			t.Fatalf("%s accepted", name)
		}
	}
}
func TestQuotaPresencePostgres(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	if err := s.migrate(ctx); err != nil {
		t.Fatal("idempotent migration", err)
	}
	empty, truncated, err := s.QuotaPresence(ctx, "", "", 10)
	if err != nil || empty == nil || len(empty) != 0 || truncated {
		t.Fatalf("empty presence=%v truncated=%v %v", empty, truncated, err)
	}
	base := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Microsecond)
	a := QuotaPresence{HostID: "host-a", AccountKey: quotaTestKey, LastSeen: base}
	got, err := s.PutQuotaPresence(ctx, a)
	if err != nil || !got.LastSeen.Equal(base) {
		t.Fatalf("first presence %+v %v", got, err)
	}
	// An older last_seen never moves the row backward; the stored value returns.
	older := a
	older.LastSeen = base.Add(-time.Minute)
	if got, err = s.PutQuotaPresence(ctx, older); err != nil || !got.LastSeen.Equal(base) {
		t.Fatalf("older presence %+v %v", got, err)
	}
	newer := a
	newer.LastSeen = base.Add(time.Minute)
	if got, err = s.PutQuotaPresence(ctx, newer); err != nil || !got.LastSeen.Equal(newer.LastSeen) {
		t.Fatalf("newer presence %+v %v", got, err)
	}
	// Within the one-minute tolerance a fast host is clamped to the database
	// clock, so the stored row never sits in the future.
	fast := QuotaPresence{HostID: "host-b", AccountKey: quotaTestKey, LastSeen: time.Now().Add(30 * time.Second)}
	if got, err = s.PutQuotaPresence(ctx, fast); err != nil || !got.LastSeen.Before(fast.LastSeen) || got.LastSeen.After(time.Now().Add(time.Second)) {
		t.Fatalf("future clamp %+v %v", got, err)
	}
	if _, err = s.PutQuotaPresence(ctx, QuotaPresence{HostID: "host-b", AccountKey: quotaTestKey, LastSeen: time.Now().Add(5 * time.Minute)}); err == nil {
		t.Fatal("future presence beyond skew accepted")
	}
	if _, err = s.PutQuotaPresence(ctx, QuotaPresence{HostID: "host/b", AccountKey: quotaTestKey, LastSeen: base}); err == nil {
		t.Fatal("invalid host stored")
	}
	const otherKey = "claude:fedcba9876543210"
	if _, err = s.PutQuotaPresence(ctx, QuotaPresence{HostID: "host-a", AccountKey: otherKey, LastSeen: base}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM quota_presence`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("presence rows=%d %v", count, err)
	}
	all, truncated, err := s.QuotaPresence(ctx, "", "", 10)
	if err != nil || len(all) != 3 || truncated || all[0].HostID != "host-b" {
		t.Fatalf("all presence=%+v truncated=%v %v", all, truncated, err)
	}
	byHost, _, err := s.QuotaPresence(ctx, "host-a", "", 10)
	if err != nil || len(byHost) != 2 || byHost[0].AccountKey != quotaTestKey {
		t.Fatalf("host filter=%+v %v", byHost, err)
	}
	byKey, _, err := s.QuotaPresence(ctx, "", otherKey, 10)
	if err != nil || len(byKey) != 1 || byKey[0].HostID != "host-a" {
		t.Fatalf("account filter=%+v %v", byKey, err)
	}
	both, _, err := s.QuotaPresence(ctx, "host-b", otherKey, 10)
	if err != nil || len(both) != 0 {
		t.Fatalf("combined filter=%+v %v", both, err)
	}
	capped, truncated, err := s.QuotaPresence(ctx, "", "", 2)
	if err != nil || len(capped) != 2 || !truncated {
		t.Fatalf("limit presence=%+v truncated=%v %v", capped, truncated, err)
	}
	// Concurrent upserts for one row keep the newest value, whatever the order.
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			x := a
			x.LastSeen = newer.LastSeen.Add(time.Duration(i) * time.Second)
			if _, e := s.PutQuotaPresence(ctx, x); e != nil {
				t.Error(e)
			}
		}(i)
	}
	wg.Wait()
	byKey, _, err = s.QuotaPresence(ctx, "host-a", quotaTestKey, 10)
	if err != nil || len(byKey) != 1 || !byKey[0].LastSeen.Equal(newer.LastSeen.Add(8*time.Second)) {
		t.Fatalf("concurrent newest=%+v %v", byKey, err)
	}
	var v24 bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=24)`).Scan(&v24); err != nil || !v24 {
		t.Fatalf("presence schema_version row missing: %v", err)
	}
	// Replay a v23 database: no presence table, no v24 row. Restart restores
	// both, and a second restart over existing rows keeps them.
	for _, q := range []string{`DROP TABLE quota_presence`, `DELETE FROM schema_version WHERE version=24`} {
		if _, err = pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal("migrate on v23 state", err)
	}
	if _, err = s.PutQuotaPresence(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err = s.migrate(ctx); err != nil {
		t.Fatal("re-migrate with presence rows", err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM quota_presence`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("presence rows after re-migrate=%d %v", count, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM schema_version WHERE version=24`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("v24 rows after re-migrate=%d %v", count, err)
	}
}

// A granted client inventing host ids cannot grow an account past the cap or
// evict hosts already present; rows past retention are hidden and pruned.
func TestQuotaPresenceBoundsPostgres(t *testing.T) {
	s, pool := searchTestStore(t)
	ctx := context.Background()
	seen := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	for i := 0; i < QuotaPresenceHosts; i++ {
		if _, err := s.PutQuotaPresence(ctx, QuotaPresence{HostID: fmt.Sprintf("host-%02d", i), AccountKey: quotaTestKey, LastSeen: seen}); err != nil {
			t.Fatal(i, err)
		}
	}
	flood := QuotaPresence{HostID: "host-flood", AccountKey: quotaTestKey, LastSeen: seen.Add(time.Minute)}
	if _, err := s.PutQuotaPresence(ctx, flood); !errors.Is(err, ErrQuotaPresenceFull) {
		t.Fatalf("cap: %v", err)
	}
	// A host already present still updates at the cap.
	present := QuotaPresence{HostID: "host-00", AccountKey: quotaTestKey, LastSeen: seen.Add(time.Minute)}
	if got, err := s.PutQuotaPresence(ctx, present); err != nil || !got.LastSeen.Equal(present.LastSeen) {
		t.Fatalf("present host at cap %+v %v", got, err)
	}
	// The cap is per account: another account is unaffected.
	if _, err := s.PutQuotaPresence(ctx, QuotaPresence{HostID: "host-flood", AccountKey: "claude:fedcba9876543210", LastSeen: seen}); err != nil {
		t.Fatal("other account", err)
	}
	// Concurrent new hosts at the cap all refuse; none slips past the count.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, e := s.PutQuotaPresence(ctx, QuotaPresence{HostID: fmt.Sprintf("host-race-%d", i), AccountKey: quotaTestKey, LastSeen: seen}); !errors.Is(e, ErrQuotaPresenceFull) {
				t.Errorf("race %d: %v", i, e)
			}
		}(i)
	}
	wg.Wait()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM quota_presence WHERE account_key=$1`, quotaTestKey).Scan(&count); err != nil || count != QuotaPresenceHosts {
		t.Fatalf("rows at cap=%d %v", count, err)
	}
	// Age one row past retention: listings hide it at once, and the next
	// write for the account prunes it, freeing the slot for a new host.
	if _, err := pool.Exec(ctx, `UPDATE quota_presence SET last_seen=clock_timestamp()-interval '31 days' WHERE host_id='host-63' AND account_key=$1`, quotaTestKey); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.QuotaPresence(ctx, "host-63", "", 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("stale row listed %+v %v", rows, err)
	}
	if _, err = s.PutQuotaPresence(ctx, flood); err != nil {
		t.Fatalf("slot after prune: %v", err)
	}
	var stale bool
	if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM quota_presence WHERE host_id='host-63')`).Scan(&stale); err != nil || stale {
		t.Fatalf("stale row not pruned %v %v", stale, err)
	}
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM quota_presence WHERE account_key=$1`, quotaTestKey).Scan(&count); err != nil || count != QuotaPresenceHosts {
		t.Fatalf("rows after prune=%d %v", count, err)
	}
}

func TestQuotaFreeBucketValidation(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	zero := time.Time{}
	const devin = "devin:0123456789abcdef"
	free := func(f *QuotaFreeBucket) QuotaObservation {
		return QuotaObservation{Kind: "auto", MeasuredAt: now, Buckets: []QuotaBucket{}, FreeBucket: f}
	}
	for _, tc := range []struct {
		name string
		key  string
		x    QuotaObservation
		want bool
	}{
		{"available", devin, free(&QuotaFreeBucket{State: "available"}), true},
		{"unknown", devin, free(&QuotaFreeBucket{State: "unknown"}), true},
		{"exhausted", devin, free(&QuotaFreeBucket{State: "exhausted", ResetsAt: &reset}), true},
		{"nil buckets", devin, QuotaObservation{Kind: "auto", MeasuredAt: now, FreeBucket: &QuotaFreeBucket{State: "available"}}, true},
		{"with percentage bucket", devin, QuotaObservation{Kind: "auto", MeasuredAt: now, Buckets: []QuotaBucket{{Window: "5h", UsedPct: 10, Scope: "account"}}, FreeBucket: &QuotaFreeBucket{State: "available"}}, true},
		{"empty state", devin, free(&QuotaFreeBucket{}), false},
		{"bad state", devin, free(&QuotaFreeBucket{State: "full"}), false},
		{"exhausted without reset", devin, free(&QuotaFreeBucket{State: "exhausted"}), false},
		{"exhausted zero reset", devin, free(&QuotaFreeBucket{State: "exhausted", ResetsAt: &zero}), false},
		{"available with reset", devin, free(&QuotaFreeBucket{State: "available", ResetsAt: &reset}), false},
		{"unknown with reset", devin, free(&QuotaFreeBucket{State: "unknown", ResetsAt: &reset}), false},
		{"non-devin account", "codex:0123456789abcdef", free(&QuotaFreeBucket{State: "available"}), false},
		{"manual", devin, QuotaObservation{Kind: "manual", Window: "7d", MeasuredAt: now, ExpiresAt: &reset, FreeBucket: &QuotaFreeBucket{State: "available"}}, false},
		{"empty buckets without free_bucket", devin, QuotaObservation{Kind: "auto", MeasuredAt: now, Buckets: []QuotaBucket{}}, false},
		{"devin percentage only", devin, QuotaObservation{Kind: "auto", MeasuredAt: now, Buckets: []QuotaBucket{{Window: "5h", UsedPct: 10, Scope: "account"}}}, true},
		{"invalid key", "devin:short", free(&QuotaFreeBucket{State: "available"}), false},
	} {
		if got := ValidQuotaObservationFor(tc.key, tc.x, now); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
	// The key-less validator keeps today's rules and refuses free_bucket.
	if ValidQuotaObservation(free(&QuotaFreeBucket{State: "available"}), now) {
		t.Error("key-less validator accepted free_bucket")
	}
	if ValidQuotaObservation(QuotaObservation{Kind: "auto", MeasuredAt: now, Buckets: []QuotaBucket{}}, now) {
		t.Error("empty auto buckets accepted without free_bucket")
	}
}
