package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrQuotaLease = errors.New("quota_lease_conflict")
var quotaKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}:[0-9a-f]{16}$`)
var quotaHolderRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
var quotaWindowRE = regexp.MustCompile(`^(credit|[1-9][0-9]{0,5}[smhdw])$`)

// Host ids are opaque placement labels (pacelever sends "host-<hash>" or an
// operator-set id); the pattern matches runtug's machine-id rule capped at 64.
var quotaHostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func ValidQuotaKey(key string) bool       { return quotaKeyRE.MatchString(key) }
func ValidQuotaHolder(holder string) bool { return quotaHolderRE.MatchString(holder) }
func ValidQuotaHost(host string) bool     { return quotaHostRE.MatchString(host) }

// Only measurement fields cross this API; no credential or host identity fields.
type QuotaBucket struct {
	Window   string     `json:"window"`
	UsedPct  float64    `json:"used_pct"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	Scope    string     `json:"scope"`
	Model    string     `json:"model,omitempty"`
}
type QuotaObservation struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Window     string    `json:"window,omitempty"`
	MeasuredAt time.Time `json:"measured_at"`
	// ConfirmedAt is server-side: the newest measured_at at which the latest
	// value was republished. Never decoded from request bodies.
	ConfirmedAt *time.Time    `json:"confirmed_at,omitempty"`
	ExpiresAt   *time.Time    `json:"expires_at,omitempty"`
	Plan        string        `json:"plan,omitempty"`
	Buckets     []QuotaBucket `json:"buckets"`
}
type QuotaLatest struct {
	Observation *QuotaObservation  `json:"observation"`
	Manual      []QuotaObservation `json:"manual"`
}

// QuotaPresence records which host last measured an account. It is kept apart
// from observations, which carry no host identity.
type QuotaPresence struct {
	HostID     string    `json:"host_id"`
	AccountKey string    `json:"account_key"`
	LastSeen   time.Time `json:"last_seen"`
}
type QuotaLease struct {
	Holder    string    `json:"holder"`
	ExpiresAt time.Time `json:"expires_at"`
	Acquired  bool      `json:"acquired"`
}

func ValidQuotaObservation(x QuotaObservation, now time.Time) bool {
	if x.MeasuredAt.IsZero() || x.MeasuredAt.After(now.Add(time.Minute)) || len(x.Plan) > 64 || len(x.Buckets) > 32 {
		return false
	}
	if x.Kind == "auto" {
		if x.Window != "" || len(x.Buckets) == 0 || x.ExpiresAt != nil {
			return false
		}
	} else if x.Kind == "manual" {
		if !quotaWindowRE.MatchString(x.Window) || x.ExpiresAt == nil || len(x.Buckets) > 1 {
			return false
		}
		if len(x.Buckets) == 1 && (x.Buckets[0].Window != x.Window || x.Buckets[0].Scope != "account" || !x.ExpiresAt.After(x.MeasuredAt)) {
			return false
		}
	} else {
		return false
	}
	for _, b := range x.Buckets {
		if !quotaWindowRE.MatchString(b.Window) || math.IsNaN(b.UsedPct) || math.IsInf(b.UsedPct, 0) || b.UsedPct < 0 || b.UsedPct > 100 || (b.Scope != "account" && b.Scope != "model") || len(b.Model) > 128 {
			return false
		}
	}
	return true
}

// ValidQuotaPresence uses the observation skew rule: last_seen may lead the
// server clock by at most one minute.
func ValidQuotaPresence(x QuotaPresence, now time.Time) bool {
	return ValidQuotaHost(x.HostID) && ValidQuotaKey(x.AccountKey) && !x.LastSeen.IsZero() && !x.LastSeen.After(now.Add(time.Minute))
}
func migrateQuota(ctx context.Context, tx pgx.Tx) error {
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS quota_observations (id BIGSERIAL PRIMARY KEY, account_key TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN ('auto','manual')), window_key TEXT NOT NULL, measured_at TIMESTAMPTZ NOT NULL, value JSONB NOT NULL, received_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp())`,
		`CREATE TABLE IF NOT EXISTS quota_latest (account_key TEXT NOT NULL, kind TEXT NOT NULL, window_key TEXT NOT NULL, observation_id BIGINT NOT NULL REFERENCES quota_observations(id), measured_at TIMESTAMPTZ NOT NULL, PRIMARY KEY(account_key,kind,window_key))`,
		// confirmed_at is the newest measured_at that republished the latest
		// value; NULL on rows written before the column existed.
		`ALTER TABLE quota_latest ADD COLUMN IF NOT EXISTS confirmed_at TIMESTAMPTZ`,
		`CREATE TABLE IF NOT EXISTS quota_leases (account_key TEXT PRIMARY KEY, owner TEXT NOT NULL, holder TEXT NOT NULL, expires_at TIMESTAMPTZ NOT NULL)`,
		// The trigger still blocks every UPDATE, DELETE and TRUNCATE; a DELETE
		// passes only while the session flag quota_prune_history sets inside
		// its own transaction is on.
		`CREATE OR REPLACE FUNCTION quota_history_immutable() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF TG_OP = 'DELETE' AND current_setting('handoffkeep.quota_prune', true) = 'on' THEN RETURN NULL; END IF; RAISE EXCEPTION 'quota observations are append-only'; RETURN NULL; END; $$`,
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='quota_history_immutable' AND tgrelid='quota_observations'::regclass) THEN CREATE TRIGGER quota_history_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON quota_observations FOR EACH STATEMENT EXECUTE FUNCTION quota_history_immutable(); END IF; END $$`,
		// The only defined delete path: rows whose received_at is 90+ days old
		// and which no quota_latest row points to. Called manually or by a
		// later approved job; no scheduler is registered here.
		`CREATE OR REPLACE FUNCTION quota_prune_history() RETURNS bigint LANGUAGE plpgsql AS $$ DECLARE n bigint; BEGIN PERFORM set_config('handoffkeep.quota_prune', 'on', true); DELETE FROM quota_observations WHERE received_at < clock_timestamp() - interval '90 days' AND id NOT IN (SELECT observation_id FROM quota_latest); GET DIAGNOSTICS n = ROW_COUNT; PERFORM set_config('handoffkeep.quota_prune', 'off', true); RETURN n; END; $$`,
		// Version 23 records the quota migration retroactively. The CREATEs
		// above stay ungated IF NOT EXISTS so servers that already ran
		// migrateQuota only insert the marker row.
		`INSERT INTO schema_version(version) VALUES (23) ON CONFLICT DO NOTHING`,
		// Version 24 adds host presence: one row per (host, account), newest
		// last_seen wins. Additive and IF NOT EXISTS, so a restart or an
		// older binary sharing the database is unaffected.
		`CREATE TABLE IF NOT EXISTS quota_presence (host_id TEXT NOT NULL CHECK(length(host_id) BETWEEN 1 AND 64), account_key TEXT NOT NULL CHECK(length(account_key) BETWEEN 1 AND 64), last_seen TIMESTAMPTZ NOT NULL, PRIMARY KEY(host_id,account_key))`,
		`CREATE INDEX IF NOT EXISTS quota_presence_account ON quota_presence(account_key)`,
		`INSERT INTO schema_version(version) VALUES (24) ON CONFLICT DO NOTHING`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) LatestQuota(ctx context.Context, key string) (QuotaLatest, error) {
	out := QuotaLatest{Manual: []QuotaObservation{}}
	rows, err := s.pool.Query(ctx, `SELECT o.id,o.value,l.confirmed_at FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=$1 ORDER BY l.kind,l.window_key`, key)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var x QuotaObservation
		var data []byte
		var id int64
		var confirmedAt *time.Time
		if err = rows.Scan(&id, &data, &confirmedAt); err != nil {
			return out, err
		}
		if err = json.Unmarshal(data, &x); err != nil {
			return out, err
		}
		x.ID = id
		x.ConfirmedAt = confirmedAt
		if x.Kind == "auto" {
			out.Observation = &x
		} else {
			out.Manual = append(out.Manual, x)
		}
	}
	return out, rows.Err()
}
func (s *Store) LeaseQuota(ctx context.Context, key, owner, holder string, ttl int) (QuotaLease, error) {
	var x QuotaLease
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return x, err
	}
	defer tx.Rollback(ctx)
	// The conditional UPSERT serializes contenders and uses the database clock.
	err = tx.QueryRow(ctx, `INSERT INTO quota_leases(account_key,owner,holder,expires_at) VALUES($1,$2,$3,clock_timestamp()+$4*interval '1 second') ON CONFLICT(account_key) DO UPDATE SET owner=EXCLUDED.owner,holder=EXCLUDED.holder,expires_at=clock_timestamp()+$4*interval '1 second' WHERE quota_leases.expires_at<=clock_timestamp() OR (quota_leases.owner=$2 AND quota_leases.holder=$3) RETURNING holder,expires_at`, key, owner, holder, ttl).Scan(&x.Holder, &x.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT holder,expires_at FROM quota_leases WHERE account_key=$1`, key).Scan(&x.Holder, &x.ExpiresAt)
	} else if err == nil {
		x.Acquired = true
	}
	if err != nil {
		return x, err
	}
	return x, tx.Commit(ctx)
}

// PruneQuotaHistory runs the guarded 90-day retention function. It is the
// only path allowed to delete quota_observations rows; cadence is a separate
// operator decision, so nothing here schedules it.
func (s *Store) PruneQuotaHistory(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT quota_prune_history()`).Scan(&n)
	return n, err
}
func (s *Store) ReleaseQuota(ctx context.Context, key, owner, holder string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM quota_leases WHERE account_key=$1 AND owner=$2 AND holder=$3`, key, owner, holder)
	return err
}

// PutQuota appends an observation and moves the latest pointer. When the
// value is identical to the current latest for (key, kind, window) it appends
// nothing and returns the existing row with deduped=true.
func (s *Store) PutQuota(ctx context.Context, key, owner, holder string, x QuotaObservation) (QuotaObservation, bool, error) {
	if !ValidQuotaKey(key) || !ValidQuotaHolder(holder) || !ValidQuotaObservation(x, time.Now()) {
		return x, false, errors.New("invalid quota observation")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return x, false, err
	}
	defer tx.Rollback(ctx)
	var valid bool
	// Hold the lease row until the append and pointer update commit. An expired
	// probe cannot publish after another holder has taken over, even with one token.
	err = tx.QueryRow(ctx, `SELECT owner=$2 AND holder=$3 AND expires_at>clock_timestamp() FROM quota_leases WHERE account_key=$1 FOR UPDATE`, key, owner, holder).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !valid {
		return x, false, ErrQuotaLease
	}
	if err != nil {
		return x, false, err
	}
	x.ID = 0
	// confirmed_at is server-owned: never store a client-supplied value.
	x.ConfirmedAt = nil
	data, err := json.Marshal(x)
	if err != nil {
		return x, false, err
	}
	// The lease row lock above serializes publishers for this account, so a
	// concurrent identical publish observes the committed row here instead of
	// double-appending. The comparison ignores measured_at: republishing the
	// same value only advances freshness on the latest pointer. The pointer's
	// own measured_at is the floor: GREATEST ignores a NULL (legacy) confirmed_at.
	var dupID int64
	var dupValue []byte
	err = tx.QueryRow(ctx, `SELECT o.id,o.value FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=$1 AND l.kind=$2 AND l.window_key=$3 AND (o.value - 'measured_at') = ($4::jsonb - 'measured_at')`, key, x.Kind, x.Window, data).Scan(&dupID, &dupValue)
	if err == nil {
		var existing QuotaObservation
		if err = json.Unmarshal(dupValue, &existing); err != nil {
			return x, false, err
		}
		existing.ID = dupID
		if err = tx.QueryRow(ctx, `UPDATE quota_latest SET confirmed_at=GREATEST(confirmed_at,measured_at,$4) WHERE account_key=$1 AND kind=$2 AND window_key=$3 RETURNING confirmed_at`, key, x.Kind, x.Window, x.MeasuredAt).Scan(&existing.ConfirmedAt); err != nil {
			return x, false, err
		}
		return existing, true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return x, false, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO quota_observations(account_key,kind,window_key,measured_at,value) VALUES($1,$2,$3,$4,$5) RETURNING id`, key, x.Kind, x.Window, x.MeasuredAt, data).Scan(&x.ID)
	if err != nil {
		return x, false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO quota_latest(account_key,kind,window_key,observation_id,measured_at,confirmed_at) VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(account_key,kind,window_key) DO UPDATE SET observation_id=EXCLUDED.observation_id,measured_at=EXCLUDED.measured_at,confirmed_at=EXCLUDED.measured_at WHERE quota_latest.measured_at<=EXCLUDED.measured_at`, key, x.Kind, x.Window, x.ID, x.MeasuredAt)
	if err != nil {
		return x, false, err
	}
	return x, false, tx.Commit(ctx)
}

// PutQuotaPresence upserts one (host, account) row and returns the stored
// value. last_seen is clamped to the database clock so a slightly fast host
// cannot pin a future value; an older last_seen leaves the row unchanged.
func (s *Store) PutQuotaPresence(ctx context.Context, x QuotaPresence) (QuotaPresence, error) {
	if !ValidQuotaPresence(x, time.Now()) {
		return x, errors.New("invalid quota presence")
	}
	err := s.pool.QueryRow(ctx, `INSERT INTO quota_presence(host_id,account_key,last_seen) VALUES($1,$2,LEAST($3::timestamptz,clock_timestamp())) ON CONFLICT(host_id,account_key) DO UPDATE SET last_seen=EXCLUDED.last_seen WHERE quota_presence.last_seen<EXCLUDED.last_seen RETURNING last_seen`, x.HostID, x.AccountKey, x.LastSeen).Scan(&x.LastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		// The stored row is newer; rows are never deleted, so it still exists.
		err = s.pool.QueryRow(ctx, `SELECT last_seen FROM quota_presence WHERE host_id=$1 AND account_key=$2`, x.HostID, x.AccountKey).Scan(&x.LastSeen)
	}
	return x, err
}

// QuotaPresence lists presence rows, newest first, optionally filtered by host
// and account. It returns at most limit rows and reports whether more exist.
func (s *Store) QuotaPresence(ctx context.Context, host, key string, limit int) ([]QuotaPresence, bool, error) {
	out := []QuotaPresence{}
	rows, err := s.pool.Query(ctx, `SELECT host_id,account_key,last_seen FROM quota_presence WHERE ($1='' OR host_id=$1) AND ($2='' OR account_key=$2) ORDER BY last_seen DESC,host_id,account_key LIMIT $3`, host, key, limit+1)
	if err != nil {
		return out, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var x QuotaPresence
		if err = rows.Scan(&x.HostID, &x.AccountKey, &x.LastSeen); err != nil {
			return out, false, err
		}
		out = append(out, x)
	}
	if err = rows.Err(); err != nil {
		return out, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
