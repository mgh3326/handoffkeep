package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mgh3326/handoffkeep/internal/wrk"
)

// O3 journal storage (hk 1873 design §2, hk 1925 S1). Four additive tables;
// records, contents and attempts are append-only for the runtime role, and
// the legacy tables are untouched.

// wrkLaneLock is the design §5 global lane-graph advisory lock.
const wrkLaneLock = 8241873001

var wrkMigration = []string{
	`CREATE TABLE IF NOT EXISTS wrk_job_locks (job_id TEXT PRIMARY KEY)`,
	`CREATE TABLE IF NOT EXISTS wrk_records (
		job_id TEXT NOT NULL REFERENCES wrk_job_locks(job_id),
		seq BIGINT NOT NULL CHECK(seq >= 1),
		record_id UUID NOT NULL UNIQUE,
		store_generation UUID NOT NULL,
		kind TEXT NOT NULL CHECK(kind IN ('claim','activate','round_open','verdict','void','joined','completed','grant','grant_cancel','end','import','import_begin','import_seal')),
		claim_seq BIGINT NOT NULL CHECK(claim_seq >= 0),
		actor_client_id TEXT NOT NULL,
		idempotency_key UUID NOT NULL,
		request_sha256 CHAR(64) NOT NULL,
		payload JSONB NOT NULL,
		imported BOOLEAN NOT NULL,
		source JSONB,
		received_at TIMESTAMPTZ NOT NULL,
		previous_hash CHAR(64) NOT NULL,
		record_hash CHAR(64) NOT NULL,
		PRIMARY KEY(job_id, seq),
		UNIQUE(actor_client_id, job_id, idempotency_key),
		CHECK((kind = 'import') = imported),
		CHECK((kind = 'import') = (source IS NOT NULL))
	)`,
	`CREATE TABLE IF NOT EXISTS wrk_contents (
		job_id TEXT NOT NULL,
		seq BIGINT NOT NULL,
		content BYTEA NOT NULL,
		content_sha256 CHAR(64) NOT NULL,
		size_bytes INTEGER NOT NULL CHECK(size_bytes BETWEEN 1 AND 262144 AND size_bytes = octet_length(content)),
		PRIMARY KEY(job_id, seq),
		FOREIGN KEY(job_id, seq) REFERENCES wrk_records(job_id, seq)
	)`,
	`CREATE TABLE IF NOT EXISTS wrk_attempts (
		attempt_id UUID PRIMARY KEY,
		actor_client_id TEXT,
		job_id TEXT,
		kind TEXT,
		stable_error_code TEXT NOT NULL,
		received_at TIMESTAMPTZ NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS wrk_attempts_job ON wrk_attempts(job_id, received_at, attempt_id)`,
	`CREATE INDEX IF NOT EXISTS wrk_records_job_kind ON wrk_records(job_id, kind, seq)`,
	`CREATE INDEX IF NOT EXISTS wrk_records_actor ON wrk_records(actor_client_id, received_at, record_id)`,
	`CREATE INDEX IF NOT EXISTS wrk_records_claim_task ON wrk_records(((payload->>'task_id')::bigint)) WHERE kind = 'claim'`,
	`CREATE INDEX IF NOT EXISTS wrk_records_round ON wrk_records(job_id, (payload->>'round_no')) WHERE kind IN ('round_open','verdict','void')`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_open_round ON wrk_records(job_id, (payload->>'round_no')) WHERE kind = 'round_open'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_verdict_round ON wrk_records(job_id, (payload->>'round_no')) WHERE kind = 'verdict'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_void_round ON wrk_records(job_id, (payload->>'round_no')) WHERE kind = 'void'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_verdict_path ON wrk_records(job_id, (payload->>'path')) WHERE kind = 'verdict'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_verdict_content ON wrk_records(job_id, (payload->>'content_sha256')) WHERE kind = 'verdict'`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_grant_use ON wrk_records(job_id, (payload->>'grant_seq')) WHERE kind IN ('void','grant_cancel')`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_extension_use ON wrk_records(job_id, (payload->>'extension_grant_seq')) WHERE kind = 'round_open' AND payload->>'extension_grant_seq' IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_repeat_use ON wrk_records(job_id, (payload->>'repeat_grant_seq')) WHERE kind = 'round_open' AND payload->>'repeat_grant_seq' IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_waiver_use ON wrk_records(job_id, (payload->>'waiver_grant_seq')) WHERE kind = 'joined' AND payload->>'waiver_grant_seq' IS NOT NULL`,
	`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_successor ON wrk_records((payload->>'predecessor_job_id')) WHERE kind = 'claim' AND payload->>'supersedes' IS NULL AND payload->>'predecessor_job_id' IS NOT NULL`,
	// Append-only even for a role that still holds a privilege: the runtime
	// role additionally has no UPDATE/DELETE grant (wrkGrantRuntime).
	`CREATE OR REPLACE FUNCTION handoffkeep_wrk_append_only() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'wrk journal is append-only'; RETURN NULL; END; $$`,
	`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'wrk_records_append_only' AND tgrelid = 'wrk_records'::regclass) THEN CREATE TRIGGER wrk_records_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON wrk_records FOR EACH STATEMENT EXECUTE FUNCTION handoffkeep_wrk_append_only(); END IF; END $$`,
	`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'wrk_contents_append_only' AND tgrelid = 'wrk_contents'::regclass) THEN CREATE TRIGGER wrk_contents_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON wrk_contents FOR EACH STATEMENT EXECUTE FUNCTION handoffkeep_wrk_append_only(); END IF; END $$`,
	`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'wrk_attempts_append_only' AND tgrelid = 'wrk_attempts'::regclass) THEN CREATE TRIGGER wrk_attempts_append_only BEFORE UPDATE OR DELETE OR TRUNCATE ON wrk_attempts FOR EACH STATEMENT EXECUTE FUNCTION handoffkeep_wrk_append_only(); END IF; END $$`,
}

// migrateWrk applies schema version 20 (hk 1925) once.
func migrateWrk(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=20)`).Scan(&applied); err != nil {
		return err
	}
	if applied {
		return nil
	}
	for _, q := range wrkMigration {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `INSERT INTO schema_version(version) VALUES (20)`)
	return err
}

// detectWrk reports whether the journal tables exist in the search path.
func (s *Store) detectWrk(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass('wrk_records') IS NOT NULL`).Scan(&ok)
	return ok, err
}

// HasWrkTables reports whether the O3 journal schema exists.
func (s *Store) HasWrkTables() bool { return s.wrk }

// OpenWithOwner opens the runtime pool at runtimeURL and, when ownerURL is
// set, runs every migration (legacy and the O3 journal, schema version 20)
// through that separate owner connection and grants the runtime role only
// what the journal needs: SELECT and INSERT on records, contents and
// attempts (no UPDATE, DELETE or TRUNCATE), and SELECT, INSERT and UPDATE on
// wrk_job_locks (row locks need UPDATE). The O3 schema is created only
// here. With an empty ownerURL it is Open, which runs the legacy
// migrations alone (director-1 off-mode startup ruling).
func OpenWithOwner(ctx context.Context, runtimeURL, ownerURL string) (*Store, error) {
	if ownerURL == "" {
		return Open(ctx, runtimeURL)
	}
	if ownerURL == runtimeURL {
		return nil, errors.New("HANDOFFKEEP_DB_MIGRATE_URL must name a role distinct from HANDOFFKEEP_DB_URL")
	}
	owner, err := Open(ctx, ownerURL)
	if err != nil {
		return nil, err
	}
	defer owner.Close()
	if err := owner.migrateWrkSchema(ctx); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(runtimeURL, "postgres://") && !strings.HasPrefix(runtimeURL, "postgresql://") {
		return nil, errors.New("HANDOFFKEEP_DB_URL must be a PostgreSQL URL")
	}
	p, err := pgxpool.New(ctx, runtimeURL)
	if err != nil {
		return nil, err
	}
	var role string
	if err := p.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
		p.Close()
		return nil, err
	}
	if err := owner.wrkGrantRuntime(ctx, role); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{pool: p, wrk: true}, nil
}

// migrateWrkSchema applies the journal schema under the same advisory lock
// the legacy migrations use.
func (s *Store) migrateWrkSchema(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(824180045)`); err != nil {
		return err
	}
	if err := migrateWrk(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) wrkGrantRuntime(ctx context.Context, role string) error {
	id := pgx.Identifier{role}.Sanitize()
	for _, q := range []string{
		`REVOKE ALL ON wrk_records, wrk_contents, wrk_attempts, wrk_job_locks FROM ` + id,
		`GRANT SELECT, INSERT ON wrk_records, wrk_contents, wrk_attempts TO ` + id,
		`GRANT SELECT, INSERT, UPDATE ON wrk_job_locks TO ` + id,
	} {
		if _, err := s.pool.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// CheckWrkRuntimeRole refuses a startup (any WRK_RECORDS_API mode, once the
// journal tables exist) unless the runtime role is distinct from the
// journal owner (design §2 L45): it must not be a
// superuser or BYPASSRLS role, must not own or be a member of the owner of
// any journal table, and must hold no UPDATE, DELETE or TRUNCATE on
// records, contents or attempts (director-1 esc-2 #4).
func (s *Store) CheckWrkRuntimeRole(ctx context.Context) error {
	var super, bypass bool
	if err := s.pool.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_catalog.pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
		return err
	}
	if super || bypass {
		return errors.New("wrk journal: the runtime database role is a superuser or BYPASSRLS role")
	}
	for _, table := range []string{"wrk_records", "wrk_contents", "wrk_attempts"} {
		var owns bool
		if err := s.pool.QueryRow(ctx, `SELECT pg_has_role(current_user, c.relowner, 'MEMBER') FROM pg_catalog.pg_class c WHERE c.oid = $1::regclass`, table).Scan(&owns); err != nil {
			return err
		}
		if owns {
			return fmt.Errorf("wrk journal: the runtime database role owns %s or is a member of its owner", table)
		}
	}
	for _, table := range []string{"wrk_records", "wrk_contents", "wrk_attempts"} {
		for _, priv := range []string{"UPDATE", "DELETE", "TRUNCATE"} {
			var has bool
			if err := s.pool.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1, $2)`, table, priv).Scan(&has); err != nil {
				return err
			}
			if has {
				return fmt.Errorf("wrk journal: the runtime database role holds %s on %s", priv, table)
			}
		}
	}
	return nil
}

// WrkJournal is the PostgreSQL wrk.Backend.
type WrkJournal struct {
	s *Store
}

// WrkJournal returns the store's journal backend.
func (s *Store) WrkJournal() *WrkJournal { return &WrkJournal{s: s} }

type pgWrkTx struct {
	ctx context.Context
	tx  pgx.Tx
}

const wrkRecordColumns = `job_id,seq,record_id::text,store_generation::text,claim_seq,actor_client_id,idempotency_key::text,request_sha256,imported,source,received_at,previous_hash,record_hash,kind,payload`

func scanWrkRecord(row interface{ Scan(...any) error }) (wrk.Record, error) {
	var r wrk.Record
	var source, payload []byte
	var at time.Time
	if err := row.Scan(&r.JobID, &r.Seq, &r.RecordID, &r.StoreGeneration, &r.ClaimSeq, &r.ActorClientID, &r.IdempotencyKey, &r.RequestSHA256, &r.Imported, &source, &at, &r.PreviousHash, &r.RecordHash, &r.Kind, &payload); err != nil {
		return r, err
	}
	r.ReceivedAt = wrk.FormatTime(at)
	if source != nil {
		r.Source = &wrk.Source{}
		if err := json.Unmarshal(source, r.Source); err != nil {
			return r, err
		}
	}
	v, err := wrk.DecodeStrict(payload)
	if err != nil {
		return r, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		return r, errors.New("wrk payload is not an object")
	}
	r.Payload = m
	return r, nil
}

func (t *pgWrkTx) Journal(jobID string) (*wrk.Journal, error) {
	j := &wrk.Journal{JobID: jobID, Imports: map[int64][]byte{}}
	rows, err := t.tx.Query(t.ctx, `SELECT `+wrkRecordColumns+` FROM wrk_records WHERE job_id=$1 ORDER BY seq`, jobID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		r, err := scanWrkRecord(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		j.Records = append(j.Records, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	crow, err := t.tx.Query(t.ctx, `SELECT c.seq, c.content FROM wrk_contents c JOIN wrk_records r USING (job_id, seq) WHERE c.job_id=$1 AND r.kind='import'`, jobID)
	if err != nil {
		return nil, err
	}
	defer crow.Close()
	for crow.Next() {
		var seq int64
		var b []byte
		if err := crow.Scan(&seq, &b); err != nil {
			return nil, err
		}
		j.Imports[seq] = b
	}
	return j, crow.Err()
}

func (t *pgWrkTx) CanonicalClaims() ([]wrk.Record, error) {
	rows, err := t.tx.Query(t.ctx, `SELECT DISTINCT ON (job_id) `+wrkRecordColumns+` FROM wrk_records WHERE kind='claim' ORDER BY job_id, seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []wrk.Record
	for rows.Next() {
		r, err := scanWrkRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (t *pgWrkTx) ContentExists(jobID string, seq int64) (bool, error) {
	var ok bool
	err := t.tx.QueryRow(t.ctx, `SELECT EXISTS(SELECT 1 FROM wrk_contents WHERE job_id=$1 AND seq=$2)`, jobID, seq).Scan(&ok)
	return ok, err
}

func (t *pgWrkTx) Content(jobID string, seq int64) ([]byte, bool, error) {
	var b []byte
	err := t.tx.QueryRow(t.ctx, `SELECT content FROM wrk_contents WHERE job_id=$1 AND seq=$2`, jobID, seq).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// BindTask writes the protected task binding of a canonical subject claim
// inside the journal transaction. The tasks row is locked, so a concurrent
// legacy mutation either commits first (and is then judged by its fence) or
// waits and sees the binding.
func (t *pgWrkTx) BindTask(taskID int64, jobID, release string, releaseTask int64) error {
	var x Task
	if err := scanTask(t.tx.QueryRow(t.ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, taskID), &x); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return wrk.Fail(wrk.ClaimConflict)
		}
		return err
	}
	if x.Refs.JobID != "" && x.Refs.JobID != jobID && x.Refs.JobID != release {
		return wrk.Fail(wrk.ClaimConflict)
	}
	now := time.Now().UTC()
	if releaseTask != 0 && releaseTask != taskID {
		var old Task
		if err := scanTask(t.tx.QueryRow(t.ctx, `SELECT `+taskColumns+` FROM tasks WHERE id=$1 FOR UPDATE`, releaseTask), &old); err == nil && old.Refs.JobID == release {
			if err := scanTask(t.tx.QueryRow(t.ctx, `UPDATE tasks SET refs=refs-'job_id',updated_at=$2 WHERE id=$1 RETURNING `+taskColumns, releaseTask, now), &old); err != nil {
				return err
			}
			if err := t.bindEvent(old, "o3 canonical claim correction released "+release, now); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	if x.Refs.JobID == jobID {
		return nil
	}
	if err := scanTask(t.tx.QueryRow(t.ctx, `UPDATE tasks SET refs=refs||jsonb_build_object('job_id',$2::text),updated_at=$3 WHERE id=$1 RETURNING `+taskColumns, taskID, jobID, now), &x); err != nil {
		return err
	}
	return t.bindEvent(x, "o3 canonical claim binds job "+jobID, now)
}

func (t *pgWrkTx) bindEvent(x Task, note string, at time.Time) error {
	refs, err := json.Marshal(x.Refs)
	if err != nil {
		return err
	}
	_, err = t.tx.Exec(t.ctx, `INSERT INTO task_events(task_id,"from","to","by",note,refs,at,kind) VALUES($1,$2,$2,'wrk',$3,$4::jsonb,$5,'transition')`, x.ID, x.State, note, string(refs), at)
	return err
}

// Append inserts one record and its content. A canonical completed or
// joined also inserts its relay projection in the same transaction, so the
// legacy relay row and the receipt-bearing record commit together.
func (t *pgWrkTx) Append(rec wrk.Record, content []byte) error {
	payload, err := json.Marshal(rec.Payload)
	if err != nil {
		return err
	}
	var source any
	if rec.Source != nil {
		b, err := json.Marshal(rec.Source)
		if err != nil {
			return err
		}
		source = string(b)
	}
	at, err := time.Parse(time.RFC3339Nano, rec.ReceivedAt)
	if err != nil {
		return err
	}
	if _, err := t.tx.Exec(t.ctx, `INSERT INTO wrk_job_locks(job_id) VALUES($1) ON CONFLICT DO NOTHING`, rec.JobID); err != nil {
		return err
	}
	if _, err := t.tx.Exec(t.ctx, `INSERT INTO wrk_records(job_id,seq,record_id,store_generation,claim_seq,actor_client_id,idempotency_key,request_sha256,imported,source,received_at,previous_hash,record_hash,kind,payload) VALUES($1,$2,$3::uuid,$4::uuid,$5,$6,$7::uuid,$8,$9,$10::jsonb,$11,$12,$13,$14,$15::jsonb)`,
		rec.JobID, rec.Seq, rec.RecordID, rec.StoreGeneration, rec.ClaimSeq, rec.ActorClientID, rec.IdempotencyKey, rec.RequestSHA256, rec.Imported, source, at, rec.PreviousHash, rec.RecordHash, rec.Kind, string(payload)); err != nil {
		return wrkInsertErr(err)
	}
	if content != nil {
		if _, err := t.tx.Exec(t.ctx, `INSERT INTO wrk_contents(job_id,seq,content,content_sha256,size_bytes) VALUES($1,$2,$3,$4,$5)`, rec.JobID, rec.Seq, content, wrk.ContentSHA256(content), len(content)); err != nil {
			return err
		}
	}
	if rec.Kind == "completed" || rec.Kind == "joined" {
		return t.project(rec)
	}
	return nil
}

// wrkInsertErr maps a unique-index violation to sequence_conflict: the
// rules already refused every duplicate visible under the job lock, so a
// violation means a concurrent writer won and a refresh is required.
func wrkInsertErr(err error) error {
	if strings.Contains(err.Error(), "SQLSTATE 23505") {
		return wrk.Fail(wrk.SequenceConflict)
	}
	return err
}

// project inserts the legacy relay row for a canonical completed/joined.
// Its reason names the record so legacy dedup is per record.
func (t *pgWrkTx) project(rec wrk.Record) error {
	var claimPayload []byte
	if err := t.tx.QueryRow(t.ctx, `SELECT payload FROM wrk_records WHERE job_id=$1 AND kind='claim' ORDER BY seq DESC LIMIT 1`, rec.JobID).Scan(&claimPayload); err != nil {
		return err
	}
	var claim struct {
		OwnerLane string `json:"owner_lane"`
	}
	if err := json.Unmarshal(claimPayload, &claim); err != nil {
		return err
	}
	kind := "job." + rec.Kind
	head, _ := rec.Payload["head"].(string)
	pr, _ := rec.Payload["pr"].(string)
	_, err := t.tx.Exec(t.ctx, `INSERT INTO relay_events(kind,job_id,epoch,owner_lane,head,pr,reason,event_id,received_at) VALUES($1,$2,0,$3,$4,$5,$6,$7,now())`,
		kind, rec.JobID, claim.OwnerLane, head, pr, WrkProjectionReason(rec.Seq), "wrk:"+rec.JobID+":"+fmt.Sprint(rec.Seq))
	return err
}

// WrkProjectionReason is the reason text of a canonical relay projection.
func WrkProjectionReason(seq int64) string { return fmt.Sprintf("o3 receipt seq %d", seq) }

// Update implements wrk.Backend.
func (b *WrkJournal) Update(ctx context.Context, lane bool, jobIDs []string, fn func(wrk.Tx) error) error {
	tx, err := b.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit = on`); err != nil {
		return err
	}
	if lane {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(wrkLaneLock)); err != nil {
			return err
		}
	}
	// jobIDs arrive sorted; lock rows exist only for jobs with records, so a
	// job's first claim creates its lock row inside the claim transaction.
	for _, id := range jobIDs {
		if _, err := tx.Exec(ctx, `SELECT job_id FROM wrk_job_locks WHERE job_id=$1 FOR UPDATE`, id); err != nil {
			return err
		}
	}
	if err := fn(&pgWrkTx{ctx: ctx, tx: tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// View implements wrk.Backend.
func (b *WrkJournal) View(ctx context.Context, fn func(wrk.Tx) error) error {
	tx, err := b.s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	return fn(&pgWrkTx{ctx: ctx, tx: tx})
}

// RecordAttempt implements wrk.Backend.
func (b *WrkJournal) RecordAttempt(ctx context.Context, a wrk.Attempt) error {
	_, err := b.s.pool.Exec(ctx, `INSERT INTO wrk_attempts(attempt_id,actor_client_id,job_id,kind,stable_error_code,received_at) VALUES($1::uuid,NULLIF($2,''),NULLIF($3,''),NULLIF($4,''),$5,$6)`, a.AttemptID, a.ClientID, a.JobID, a.Kind, string(a.Code), a.ReceivedAt)
	return err
}
