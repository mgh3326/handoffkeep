package store

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrQuotaLease = errors.New("quota_lease_conflict")
var quotaKeyRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}:[0-9a-f]{16}$`)

// A bare pool name grants every account key with that pool prefix. A token
// carrying a 16-hex run reads as a mangled account key (a lost colon), so
// it is refused — the bad grant line keeps its stderr diagnostic.
var quotaPoolRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var quotaHexRunRE = regexp.MustCompile(`[0-9a-f]{16}`)
var quotaHolderRE = regexp.MustCompile(`^[0-9a-f]{32}$`)
var quotaWindowRE = regexp.MustCompile(`^(credit|[1-9][0-9]{0,5}[smhdw])$`)

// Host ids are opaque placement labels (pacelever sends "host-<hash>" or an
// operator-set id); the pattern matches runtug's machine-id rule capped at 64.
var quotaHostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func ValidQuotaKey(key string) bool { return quotaKeyRE.MatchString(key) }
func ValidQuotaPool(pool string) bool {
	return quotaPoolRE.MatchString(pool) && !quotaHexRunRE.MatchString(pool)
}
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

// QuotaFreeBucket is the free-profile state of a Devin account. It carries no
// usage percentage: state is available, exhausted (with a nonzero resets_at) or
// unknown (resets_at omitted). The server stores it as given; readers apply
// expiry and freshness.
type QuotaFreeBucket struct {
	State    string     `json:"state"`
	ResetsAt *time.Time `json:"resets_at,omitempty"`
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
	// FreeBucket is optional and valid only on automatic devin observations.
	FreeBucket *QuotaFreeBucket `json:"free_bucket,omitempty"`
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

// ValidQuotaObservation validates an observation without an account key, so it
// refuses free_bucket, which is only valid for a devin account. Publishing
// paths use ValidQuotaObservationFor.
func ValidQuotaObservation(x QuotaObservation, now time.Time) bool {
	return x.FreeBucket == nil && validQuotaObservation(x, now)
}

// ValidQuotaObservationFor additionally checks free_bucket against the account
// key: it is accepted only on an automatic observation of a devin account.
func ValidQuotaObservationFor(key string, x QuotaObservation, now time.Time) bool {
	if !ValidQuotaKey(key) || !validQuotaObservation(x, now) {
		return false
	}
	return x.FreeBucket == nil || x.Kind == "auto" && strings.HasPrefix(key, "devin:")
}
func validQuotaFreeBucket(f QuotaFreeBucket) bool {
	switch f.State {
	case "available", "unknown":
		return f.ResetsAt == nil
	case "exhausted":
		return f.ResetsAt != nil && !f.ResetsAt.IsZero()
	}
	return false
}
func validQuotaObservation(x QuotaObservation, now time.Time) bool {
	if x.FreeBucket != nil && !validQuotaFreeBucket(*x.FreeBucket) {
		return false
	}
	if x.MeasuredAt.IsZero() || x.MeasuredAt.After(now.Add(time.Minute)) || len(x.Plan) > 64 || len(x.Buckets) > 32 {
		return false
	}
	if x.Kind == "auto" {
		if x.Window != "" || len(x.Buckets) == 0 && x.FreeBucket == nil || x.ExpiresAt != nil {
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
		if !quotaWindowRE.MatchString(b.Window) || math.IsNaN(b.UsedPct) || math.IsInf(b.UsedPct, 0) || b.UsedPct < 0 || b.UsedPct > 100 || (b.Scope != "account" && b.Scope != "model" && b.Scope != "group") || b.Scope == "group" && b.Model == "" || len(b.Model) > 128 {
			return false
		}
	}
	return true
}

// Presence is bounded per account: at most QuotaPresenceHosts rows, each kept
// for QuotaPresenceRetention after its last_seen.
const QuotaPresenceHosts = 64
const QuotaPresenceRetention = 30 * 24 * time.Hour

var ErrQuotaPresenceFull = errors.New("quota_presence_full")

// ValidQuotaPresence uses the observation skew rule: last_seen may lead the
// server clock by at most one minute. A last_seen already past retention is
// refused rather than stored for the next prune.
func ValidQuotaPresence(x QuotaPresence, now time.Time) bool {
	return ValidQuotaHost(x.HostID) && ValidQuotaKey(x.AccountKey) && x.LastSeen.After(now.Add(-QuotaPresenceRetention)) && !x.LastSeen.After(now.Add(time.Minute))
}
func migrateQuota(ctx context.Context, tx pgx.Tx) error {
	if err := execAll(ctx, tx, []string{
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
	}); err != nil {
		return err
	}
	return nil
}
func (s *Store) LatestQuota(ctx context.Context, key string) (QuotaLatest, error) {
	out := QuotaLatest{Manual: []QuotaObservation{}}
	rows, err := s.db().Query(ctx, s.q(`SELECT o.id,o.value,l.confirmed_at FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=$1 ORDER BY l.kind,l.window_key`, `SELECT o.id,o.value,l.confirmed_at FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=? ORDER BY l.kind,l.window_key`), key)
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

// QuotaObservationHistory returns an account's observations in the inclusive
// measured_at range, oldest first. found distinguishes an account with no
// matching rows from a key that has never had an observation.
func (s *Store) QuotaObservationHistory(ctx context.Context, key string, since, until time.Time, limit int) ([]QuotaObservation, bool, error) {
	var found bool
	if err := s.db().QueryRow(ctx, s.q(`SELECT EXISTS(SELECT 1 FROM quota_observations WHERE account_key=$1)`, `SELECT EXISTS(SELECT 1 FROM quota_observations WHERE account_key=?)`), key).Scan(&found); err != nil {
		return nil, false, err
	}
	if !found {
		return []QuotaObservation{}, false, nil
	}
	rows, err := s.db().Query(ctx, s.q(`SELECT id,value FROM quota_observations WHERE account_key=$1 AND measured_at >= $2 AND measured_at <= $3 ORDER BY measured_at,id LIMIT $4`, `SELECT id,value FROM quota_observations WHERE account_key=? AND measured_at >= ? AND measured_at <= ? ORDER BY measured_at,id LIMIT ?`), key, since, until, limit)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	out := []QuotaObservation{}
	for rows.Next() {
		var x QuotaObservation
		var data []byte
		var id int64
		if err := rows.Scan(&id, &data); err != nil {
			return nil, true, err
		}
		if err := json.Unmarshal(data, &x); err != nil {
			return nil, true, err
		}
		// ID is held in its own column and is authoritative over JSON.
		x.ID = id
		out = append(out, x)
	}
	return out, true, rows.Err()
}

// QuotaAccountKeys returns the opaque account keys that currently have a
// latest observation. Account keys are hashes/aliases and never credentials.
func (s *Store) QuotaAccountKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db().Query(ctx, `SELECT DISTINCT account_key FROM quota_latest ORDER BY account_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}
func (s *Store) LeaseQuota(ctx context.Context, key, owner, holder string, ttl int) (QuotaLease, error) {
	var x QuotaLease
	tx, err := s.db().Begin(ctx)
	if err != nil {
		return x, err
	}
	defer tx.Rollback(ctx)
	if s.dialect == mysqlDialect {
		// The duplicate-key no-op locks the account row even when it already
		// exists. Reading and updating under that lock gives one winner.
		_, err = tx.Exec(ctx, `INSERT INTO quota_leases(account_key,owner,holder,expires_at) VALUES(?,?,?,DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? SECOND)) ON DUPLICATE KEY UPDATE account_key=quota_leases.account_key`, key, owner, holder, ttl)
		if err != nil {
			return x, err
		}
		var canAcquire bool
		err = tx.QueryRow(ctx, `SELECT holder,expires_at,(expires_at<=UTC_TIMESTAMP(6) OR (owner=? AND holder=?)) FROM quota_leases WHERE account_key=? FOR UPDATE`, owner, holder, key).Scan(&x.Holder, &x.ExpiresAt, &canAcquire)
		if err != nil {
			return x, err
		}
		if canAcquire {
			_, err = tx.Exec(ctx, `UPDATE quota_leases SET owner=?,holder=?,expires_at=DATE_ADD(UTC_TIMESTAMP(6), INTERVAL ? SECOND) WHERE account_key=?`, owner, holder, ttl, key)
			if err != nil {
				return x, err
			}
			err = tx.QueryRow(ctx, `SELECT holder,expires_at FROM quota_leases WHERE account_key=?`, key).Scan(&x.Holder, &x.ExpiresAt)
			if err != nil {
				return x, err
			}
			x.Acquired = true
		}
		return x, tx.Commit(ctx)
	}
	// The conditional UPSERT serializes contenders and uses the database clock.
	err = tx.QueryRow(ctx, `INSERT INTO quota_leases(account_key,owner,holder,expires_at) VALUES($1,$2,$3,clock_timestamp()+$4*interval '1 second') ON CONFLICT(account_key) DO UPDATE SET owner=EXCLUDED.owner,holder=EXCLUDED.holder,expires_at=clock_timestamp()+$4*interval '1 second' WHERE quota_leases.expires_at<=clock_timestamp() OR (quota_leases.owner=$2 AND quota_leases.holder=$3) RETURNING holder,expires_at`, key, owner, holder, ttl).Scan(&x.Holder, &x.ExpiresAt)
	if errors.Is(err, ErrNoRows) {
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
	if s.dialect == mysqlDialect {
		conn, err := s.db().(sqlQuerier).db.Conn(ctx)
		if err != nil {
			return 0, err
		}
		defer conn.Close()
		// The guard variable is session scoped, so clear it before returning the
		// pinned connection to the pool, even after rollback or cancellation.
		defer func() {
			if _, err := conn.ExecContext(context.Background(), `SET @hk_quota_prune = NULL`); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}()
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return 0, err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `SET @hk_quota_prune = 1`); err != nil {
			return 0, err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM quota_observations WHERE received_at < DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 90 DAY) AND id NOT IN (SELECT observation_id FROM quota_latest)`)
		if err != nil {
			return 0, err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if err = tx.Commit(); err != nil {
			return 0, err
		}
		return n, nil
	}
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT quota_prune_history()`).Scan(&n)
	return n, err
}
func (s *Store) ReleaseQuota(ctx context.Context, key, owner, holder string) error {
	_, err := s.db().Exec(ctx, s.q(`DELETE FROM quota_leases WHERE account_key=$1 AND owner=$2 AND holder=$3`, `DELETE FROM quota_leases WHERE account_key=? AND owner=? AND holder=?`), key, owner, holder)
	return err
}

// PutQuota appends an observation and moves the latest pointer. When the
// value is identical to the current latest for (key, kind, window) it appends
// nothing and returns the existing row with deduped=true.
func (s *Store) PutQuota(ctx context.Context, key, owner, holder string, x QuotaObservation) (QuotaObservation, bool, error) {
	if !ValidQuotaKey(key) || !ValidQuotaHolder(holder) || !ValidQuotaObservationFor(key, x, time.Now()) {
		return x, false, errors.New("invalid quota observation")
	}
	tx, err := s.db().Begin(ctx)
	if err != nil {
		return x, false, err
	}
	defer tx.Rollback(ctx)
	var valid bool
	// Hold the lease row until the append and pointer update commit. An expired
	// probe cannot publish after another holder has taken over, even with one token.
	err = tx.QueryRow(ctx, s.q(`SELECT owner=$2 AND holder=$3 AND expires_at>clock_timestamp() FROM quota_leases WHERE account_key=$1 FOR UPDATE`, `SELECT owner=? AND holder=? AND expires_at>UTC_TIMESTAMP(6) FROM quota_leases WHERE account_key=? FOR UPDATE`), quotaLeaseArgs(s.dialect, key, owner, holder)...).Scan(&valid)
	if errors.Is(err, ErrNoRows) || err == nil && !valid {
		return x, false, ErrQuotaLease
	}
	if err != nil {
		return x, false, err
	}
	x.ID = 0
	// confirmed_at is server-owned: never store a client-supplied value.
	x.ConfirmedAt = nil
	// A free-only observation is stored and returned with "buckets":[] whether
	// the client sent [] or omitted the field.
	if x.FreeBucket != nil && x.Buckets == nil {
		x.Buckets = []QuotaBucket{}
	}
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
	err = tx.QueryRow(ctx, s.q(`SELECT o.id,o.value FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=$1 AND l.kind=$2 AND l.window_key=$3 AND (o.value - 'measured_at') = ($4::jsonb - 'measured_at')`, `SELECT o.id,o.value FROM quota_latest l JOIN quota_observations o ON o.id=l.observation_id WHERE l.account_key=? AND l.kind=? AND l.window_key=? AND JSON_REMOVE(o.value, '$.measured_at') = JSON_REMOVE(CAST(? AS JSON), '$.measured_at')`), key, x.Kind, x.Window, data).Scan(&dupID, &dupValue)
	if err == nil {
		var existing QuotaObservation
		if err = json.Unmarshal(dupValue, &existing); err != nil {
			return x, false, err
		}
		existing.ID = dupID
		if s.dialect == mysqlDialect {
			_, err = tx.Exec(ctx, `UPDATE quota_latest SET confirmed_at=GREATEST(COALESCE(confirmed_at, measured_at), measured_at, ?) WHERE account_key=? AND kind=? AND window_key=?`, x.MeasuredAt, key, x.Kind, x.Window)
			if err == nil {
				err = tx.QueryRow(ctx, `SELECT confirmed_at FROM quota_latest WHERE account_key=? AND kind=? AND window_key=?`, key, x.Kind, x.Window).Scan(&existing.ConfirmedAt)
			}
		} else {
			err = tx.QueryRow(ctx, `UPDATE quota_latest SET confirmed_at=GREATEST(confirmed_at,measured_at,$4) WHERE account_key=$1 AND kind=$2 AND window_key=$3 RETURNING confirmed_at`, key, x.Kind, x.Window, x.MeasuredAt).Scan(&existing.ConfirmedAt)
		}
		if err != nil {
			return x, false, err
		}
		return existing, true, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNoRows) {
		return x, false, err
	}
	x.ID, err = tx.InsertID(ctx, s.q(`INSERT INTO quota_observations(account_key,kind,window_key,measured_at,value) VALUES($1,$2,$3,$4,$5) RETURNING id`, `INSERT INTO quota_observations(account_key,kind,window_key,measured_at,value) VALUES(?,?,?,?,?)`), key, x.Kind, x.Window, x.MeasuredAt, data)
	if err != nil {
		return x, false, err
	}
	_, err = tx.Exec(ctx, s.q(`INSERT INTO quota_latest(account_key,kind,window_key,observation_id,measured_at,confirmed_at) VALUES($1,$2,$3,$4,$5,$5) ON CONFLICT(account_key,kind,window_key) DO UPDATE SET observation_id=EXCLUDED.observation_id,measured_at=EXCLUDED.measured_at,confirmed_at=EXCLUDED.measured_at WHERE quota_latest.measured_at<=EXCLUDED.measured_at`, `INSERT INTO quota_latest(account_key,kind,window_key,observation_id,measured_at,confirmed_at) VALUES(?,?,?,?,?,?) ON DUPLICATE KEY UPDATE observation_id=IF(measured_at<=VALUES(measured_at),VALUES(observation_id),observation_id), confirmed_at=IF(measured_at<=VALUES(measured_at),VALUES(measured_at),confirmed_at), measured_at=IF(measured_at<=VALUES(measured_at),VALUES(measured_at),measured_at)`), quotaLatestArgs(s.dialect, key, x.Kind, x.Window, x.ID, x.MeasuredAt)...)
	if err != nil {
		return x, false, err
	}
	return x, false, tx.Commit(ctx)
}

// PutQuotaPresence upserts one (host, account) row and returns the stored
// value. last_seen is clamped to the database clock so a slightly fast host
// cannot pin a future value; an older last_seen leaves the row unchanged.
// Each write first prunes the account's rows past retention. A host new to an
// account that already holds QuotaPresenceHosts rows gets
// ErrQuotaPresenceFull: hosts already present keep updating, so a flood of
// invented host ids cannot evict them.
func (s *Store) PutQuotaPresence(ctx context.Context, x QuotaPresence) (QuotaPresence, error) {
	if !ValidQuotaPresence(x, time.Now()) {
		return x, errors.New("invalid quota presence")
	}
	tx, release, err := s.beginQuotaPresence(ctx, x.AccountKey)
	if err != nil {
		return x, err
	}
	defer release()
	defer tx.Rollback(ctx)
	// The per-account transaction lock makes the count and insert atomic
	// against concurrent new hosts for the same account.
	if _, err = tx.Exec(ctx, s.q(`DELETE FROM quota_presence WHERE account_key=$1 AND last_seen < clock_timestamp()-$2*interval '1 second'`, `DELETE FROM quota_presence WHERE account_key=? AND last_seen < DATE_SUB(UTC_TIMESTAMP(6), INTERVAL ? SECOND)`), x.AccountKey, int64(QuotaPresenceRetention/time.Second)); err != nil {
		return x, err
	}
	var exists bool
	var hosts int
	if err = tx.QueryRow(ctx, s.q(`SELECT EXISTS(SELECT 1 FROM quota_presence WHERE host_id=$1 AND account_key=$2), (SELECT count(*) FROM quota_presence WHERE account_key=$2)`, `SELECT EXISTS(SELECT 1 FROM quota_presence WHERE host_id=? AND account_key=?), (SELECT count(*) FROM quota_presence WHERE account_key=?)`), quotaPresenceCountArgs(s.dialect, x.HostID, x.AccountKey)...).Scan(&exists, &hosts); err != nil {
		return x, err
	}
	if !exists && hosts >= QuotaPresenceHosts {
		return x, ErrQuotaPresenceFull
	}
	if s.dialect == mysqlDialect {
		_, err = tx.Exec(ctx, `INSERT INTO quota_presence(host_id,account_key,last_seen) VALUES(?,?,LEAST(?,UTC_TIMESTAMP(6))) ON DUPLICATE KEY UPDATE last_seen=GREATEST(last_seen,VALUES(last_seen))`, x.HostID, x.AccountKey, x.LastSeen)
		if err == nil {
			err = tx.QueryRow(ctx, `SELECT last_seen FROM quota_presence WHERE host_id=? AND account_key=?`, x.HostID, x.AccountKey).Scan(&x.LastSeen)
		}
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO quota_presence(host_id,account_key,last_seen) VALUES($1,$2,LEAST($3::timestamptz,clock_timestamp())) ON CONFLICT(host_id,account_key) DO UPDATE SET last_seen=EXCLUDED.last_seen WHERE quota_presence.last_seen<EXCLUDED.last_seen RETURNING last_seen`, x.HostID, x.AccountKey, x.LastSeen).Scan(&x.LastSeen)
		if errors.Is(err, ErrNoRows) {
			// The stored row is newer; the account lock keeps it in place.
			err = tx.QueryRow(ctx, `SELECT last_seen FROM quota_presence WHERE host_id=$1 AND account_key=$2`, x.HostID, x.AccountKey).Scan(&x.LastSeen)
		}
	}
	if err != nil {
		return x, err
	}
	return x, tx.Commit(ctx)
}

// QuotaPresence lists presence rows within retention, newest first, optionally
// filtered by host and account. It returns at most limit rows and reports
// whether more exist. Rows of an account nobody writes any more are hidden
// once past retention, even before a write prunes them.
func (s *Store) QuotaPresence(ctx context.Context, host, key string, limit int) ([]QuotaPresence, bool, error) {
	out := []QuotaPresence{}
	rows, err := s.db().Query(ctx, s.q(`SELECT host_id,account_key,last_seen FROM quota_presence WHERE ($1='' OR host_id=$1) AND ($2='' OR account_key=$2) AND last_seen >= clock_timestamp()-$4*interval '1 second' ORDER BY last_seen DESC,host_id,account_key LIMIT $3`, `SELECT host_id,account_key,last_seen FROM quota_presence WHERE (?='' OR host_id=?) AND (?='' OR account_key=?) AND last_seen >= DATE_SUB(UTC_TIMESTAMP(6), INTERVAL ? SECOND) ORDER BY last_seen DESC,host_id,account_key LIMIT ?`), quotaPresenceListArgs(s.dialect, host, key, limit+1)...)
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

func quotaLeaseArgs(d storeDialect, key, owner, holder string) []any {
	if d == mysqlDialect {
		return []any{owner, holder, key}
	}
	return []any{key, owner, holder}
}

func quotaLatestArgs(d storeDialect, key, kind, window string, id int64, measured time.Time) []any {
	args := []any{key, kind, window, id, measured}
	if d == mysqlDialect {
		args = append(args, measured)
	}
	return args
}

func (s *Store) beginQuotaPresence(ctx context.Context, key string) (transaction, func(), error) {
	if s.dialect == mysqlDialect {
		return s.beginNamedTransaction(ctx, "quota-presence:"+key)
	}
	tx, err := s.db().Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('quota_presence:'||$1, 0))`, key); err != nil {
		_ = tx.Rollback(ctx)
		return nil, nil, err
	}
	return tx, func() {}, nil
}
func quotaPresenceCountArgs(d storeDialect, host, key string) []any {
	if d == mysqlDialect {
		return []any{host, key, key}
	}
	return []any{host, key}
}
func quotaPresenceListArgs(d storeDialect, host, key string, limit int) []any {
	retention := int64(QuotaPresenceRetention / time.Second)
	if d == mysqlDialect {
		return []any{host, host, key, key, retention, limit}
	}
	return []any{host, key, limit, retention}
}
