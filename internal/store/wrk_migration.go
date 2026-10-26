package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// The owner runs the additive migration. D1 separately grants SELECT/INSERT
// to its distinct runtime role; enabled startup verifies the effective grants.
// No UPDATE/DELETE grant on journal or content is issued here.
func migrateWrk(ctx context.Context, tx pgx.Tx) error {
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_version WHERE version=20)`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS wrk_job_locks(job_id TEXT PRIMARY KEY)`,
		`CREATE TABLE IF NOT EXISTS wrk_records(job_id TEXT NOT NULL REFERENCES wrk_job_locks(job_id),seq BIGINT NOT NULL CHECK(seq BETWEEN 1 AND 9007199254740991),record_id UUID NOT NULL UNIQUE,store_generation UUID NOT NULL,kind TEXT NOT NULL CHECK(kind IN ('claim','activate','round_open','verdict','void','joined','completed','grant','grant_cancel','end','import','import_begin','import_seal')),claim_seq BIGINT NOT NULL,actor_client_id TEXT NOT NULL,idempotency_key UUID NOT NULL,request_sha256 CHAR(64) NOT NULL,payload JSONB NOT NULL,imported BOOLEAN NOT NULL,source JSONB,received_at TIMESTAMPTZ NOT NULL,previous_hash CHAR(64) NOT NULL,record_hash CHAR(64) NOT NULL,PRIMARY KEY(job_id,seq),UNIQUE(actor_client_id,job_id,idempotency_key))`,
		`CREATE TABLE IF NOT EXISTS wrk_contents(job_id TEXT NOT NULL,seq BIGINT NOT NULL,content BYTEA NOT NULL,content_sha256 CHAR(64) NOT NULL,size_bytes INTEGER NOT NULL CHECK(size_bytes BETWEEN 1 AND 262144),PRIMARY KEY(job_id,seq),FOREIGN KEY(job_id,seq) REFERENCES wrk_records(job_id,seq),CHECK(octet_length(content)=size_bytes))`,
		`CREATE TABLE IF NOT EXISTS wrk_attempts(attempt_id UUID PRIMARY KEY,actor_client_id TEXT,job_id TEXT,kind TEXT,stable_error_code TEXT NOT NULL,received_at TIMESTAMPTZ NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS wrk_records_kind_seq ON wrk_records(job_id,kind,seq)`,
		`CREATE INDEX IF NOT EXISTS wrk_records_actor_time ON wrk_records(actor_client_id,received_at,record_id)`,
		`CREATE INDEX IF NOT EXISTS wrk_records_claim_task ON wrk_records((payload->>'task_id')) WHERE kind='claim'`,
		`CREATE INDEX IF NOT EXISTS wrk_records_round_lookup ON wrk_records(job_id,(payload->>'round_no')) WHERE kind IN ('round_open','verdict','void')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_round_open_unique ON wrk_records(job_id,(payload->>'round_no')) WHERE kind='round_open'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_verdict_round_unique ON wrk_records(job_id,(payload->>'round_no')) WHERE kind='verdict'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_void_round_unique ON wrk_records(job_id,(payload->>'round_no')) WHERE kind='void'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_verdict_path_unique ON wrk_records(job_id,(payload->>'path')) WHERE kind='verdict'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_verdict_content_unique ON wrk_records(job_id,(payload->>'content_sha256')) WHERE kind='verdict'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_void_cancel_grant_unique ON wrk_records(job_id,(payload->>'grant_seq')) WHERE kind IN ('void','grant_cancel')`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_extension_unique ON wrk_records(job_id,(payload->>'extension_grant_seq')) WHERE kind='round_open' AND payload->>'extension_grant_seq' IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_repeat_unique ON wrk_records(job_id,(payload->>'repeat_grant_seq')) WHERE kind='round_open' AND payload->>'repeat_grant_seq' IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_waiver_unique ON wrk_records(job_id,(payload->>'waiver_grant_seq')) WHERE kind='joined' AND payload->>'waiver_grant_seq' IS NOT NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS wrk_records_successor_unique ON wrk_records((payload->>'predecessor_job_id')) WHERE kind='claim' AND payload->>'supersedes' IS NULL AND payload->>'predecessor_job_id' IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS wrk_attempts_job_time ON wrk_attempts(job_id,received_at,attempt_id)`,
		`REVOKE ALL ON wrk_records,wrk_contents,wrk_attempts,wrk_job_locks FROM PUBLIC`,
		`INSERT INTO schema_version(version) VALUES(20)`,
	}
	for _, q := range statements {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}
	return nil
}

// MigrateWrk is an explicit owner-only provisioning step. It is never called
// by the legacy-only default startup path.
func (s *Store) MigrateWrk(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(824180045)`); err != nil {
		return err
	}
	if err = migrateWrk(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
