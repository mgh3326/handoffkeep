package store

// 20261118-mysql-relay-jobs expands the B2 jobs and relay_events stubs and
// creates this area's remaining tables. Guarded ALTER and index steps lock
// the existing stubs; the relay_events index builds scan the table and hold
// a metadata lock, pausing writes until each build completes. Keep that table
// small during this expand step. Generated nullable relay keys preserve the
// PostgreSQL partial unique indexes without constraining unrelated events.
func init() {
	registerMySQLMigration(mysqlMigration{id: "20261118-mysql-relay-jobs", after: []string{"20261118-mysql-tasks"}, steps: mysqlRelayJobsSteps})
}

var mysqlRelayJobsSteps = []string{
	// The sync transaction token only groups pending rows before allocation.
	// Committed rows copied between dialects already have sync_id values, so
	// the dialect-specific token is excluded from parity and may be NULL there.
	mysqlIfColumnNotNull("sync_log", "txn", `ALTER TABLE sync_log MODIFY COLUMN txn BINARY(8) NULL`),
	// Expand the jobs stub created by the tasks area. Existing rows retain the
	// exact defaults used by the PostgreSQL schema.
	mysqlIfNoColumn("jobs", "label", `ALTER TABLE jobs ADD COLUMN label LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "family", `ALTER TABLE jobs ADD COLUMN family LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "profile", `ALTER TABLE jobs ADD COLUMN profile LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "model", `ALTER TABLE jobs ADD COLUMN model LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "pool", `ALTER TABLE jobs ADD COLUMN pool LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "owner_lane", `ALTER TABLE jobs ADD COLUMN owner_lane LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "parent_lane", `ALTER TABLE jobs ADD COLUMN parent_lane LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "parent_job", `ALTER TABLE jobs ADD COLUMN parent_job LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "pane_id", `ALTER TABLE jobs ADD COLUMN pane_id LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "worktree", `ALTER TABLE jobs ADD COLUMN worktree JSON NULL`),
	mysqlIfNoColumn("jobs", "report_doc", `ALTER TABLE jobs ADD COLUMN report_doc LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "result", `ALTER TABLE jobs ADD COLUMN result JSON NULL`),
	mysqlIfNoColumn("jobs", "created_by", `ALTER TABLE jobs ADD COLUMN created_by LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("jobs", "selection", `ALTER TABLE jobs ADD COLUMN selection JSON NULL`),
	mysqlIfNoColumn("jobs", "harness", `ALTER TABLE jobs ADD COLUMN harness LONGTEXT NULL`),
	mysqlIfNoColumn("jobs", "effort", `ALTER TABLE jobs ADD COLUMN effort LONGTEXT NULL`),
	mysqlIfNoColumn("jobs", "claimed_at", `ALTER TABLE jobs ADD COLUMN claimed_at DATETIME(6) NULL`),
	mysqlIfNoColumn("jobs", "spawned_at", `ALTER TABLE jobs ADD COLUMN spawned_at DATETIME(6) NULL`),
	mysqlIfNoColumn("jobs", "spawned_received_at", `ALTER TABLE jobs ADD COLUMN spawned_received_at DATETIME(6) NULL`),
	mysqlIfNoColumn("jobs", "ready_at", `ALTER TABLE jobs ADD COLUMN ready_at DATETIME(6) NULL`),
	mysqlIfNoColumn("jobs", "ready_received_at", `ALTER TABLE jobs ADD COLUMN ready_received_at DATETIME(6) NULL`),
	mysqlIfNoColumn("jobs", "ended_at", `ALTER TABLE jobs ADD COLUMN ended_at DATETIME(6) NULL`),
	mysqlIfNoColumn("jobs", "ended_received_at", `ALTER TABLE jobs ADD COLUMN ended_received_at DATETIME(6) NULL`),
	mysqlIfNoColumn("jobs", "report_sha256", `ALTER TABLE jobs ADD COLUMN report_sha256 VARCHAR(64) NULL`),
	mysqlIfNoColumn("jobs", "report_version", `ALTER TABLE jobs ADD COLUMN report_version INT NULL`),
	mysqlIfNoColumn("jobs", "skills", `ALTER TABLE jobs ADD COLUMN skills JSON NULL`),
	mysqlIfNoColumn("jobs", "skills_supported", `ALTER TABLE jobs ADD COLUMN skills_supported BOOLEAN NULL`),
	mysqlIfNoColumn("jobs", "end_reason", `ALTER TABLE jobs ADD COLUMN end_reason LONGTEXT NULL`),
	mysqlIfNoColumn("jobs", "diff_files", `ALTER TABLE jobs ADD COLUMN diff_files INT NULL`),
	mysqlIfNoColumn("jobs", "diff_insertions", `ALTER TABLE jobs ADD COLUMN diff_insertions INT NULL`),
	mysqlIfNoColumn("jobs", "diff_deletions", `ALTER TABLE jobs ADD COLUMN diff_deletions INT NULL`),
	mysqlIfNoColumn("jobs", "input_tokens", `ALTER TABLE jobs ADD COLUMN input_tokens BIGINT NULL`),
	mysqlIfNoColumn("jobs", "output_tokens", `ALTER TABLE jobs ADD COLUMN output_tokens BIGINT NULL`),
	mysqlIfNoColumn("jobs", "reasoning_tokens", `ALTER TABLE jobs ADD COLUMN reasoning_tokens BIGINT NULL`),
	mysqlIfNoColumn("jobs", "cache_read_tokens", `ALTER TABLE jobs ADD COLUMN cache_read_tokens BIGINT NULL`),
	mysqlIfNoColumn("jobs", "cache_write_tokens", `ALTER TABLE jobs ADD COLUMN cache_write_tokens BIGINT NULL`),
	mysqlIfNoColumn("jobs", "usage_source", `ALTER TABLE jobs ADD COLUMN usage_source LONGTEXT NULL`),
	mysqlIfNoColumn("jobs", "usage_recorded_at", `ALTER TABLE jobs ADD COLUMN usage_recorded_at DATETIME(6) NULL`),
	mysqlIfNoColumn("jobs", "role", `ALTER TABLE jobs ADD COLUMN role LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfColumnNotNull("jobs", "task_id", `ALTER TABLE jobs MODIFY COLUMN task_id BIGINT NULL`),
	mysqlIfNoIndex("jobs", "jobs_task", `CREATE INDEX jobs_task ON jobs(task_id, created_at, job_id)`),
	mysqlIfNoConstraint("jobs", "jobs_report_sha256_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_report_sha256_check CHECK(report_sha256 IS NULL OR report_sha256 REGEXP '^[0-9a-f]{64}$')`),
	mysqlIfNoConstraint("jobs", "jobs_report_version_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_report_version_check CHECK(report_version IS NULL OR report_version>=1)`),
	mysqlIfNoConstraint("jobs", "jobs_skills_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_skills_check CHECK(skills IS NULL OR JSON_TYPE(skills)='ARRAY')`),
	mysqlIfNoConstraint("jobs", "jobs_diff_files_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_diff_files_check CHECK(diff_files IS NULL OR diff_files>=0)`),
	mysqlIfNoConstraint("jobs", "jobs_diff_insertions_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_diff_insertions_check CHECK(diff_insertions IS NULL OR diff_insertions>=0)`),
	mysqlIfNoConstraint("jobs", "jobs_diff_deletions_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_diff_deletions_check CHECK(diff_deletions IS NULL OR diff_deletions>=0)`),
	mysqlIfNoConstraint("jobs", "jobs_input_tokens_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_input_tokens_check CHECK(input_tokens IS NULL OR input_tokens>=0)`),
	mysqlIfNoConstraint("jobs", "jobs_output_tokens_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_output_tokens_check CHECK(output_tokens IS NULL OR output_tokens>=0)`),
	mysqlIfNoConstraint("jobs", "jobs_reasoning_tokens_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_reasoning_tokens_check CHECK(reasoning_tokens IS NULL OR reasoning_tokens>=0)`),
	mysqlIfNoConstraint("jobs", "jobs_cache_read_tokens_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_cache_read_tokens_check CHECK(cache_read_tokens IS NULL OR cache_read_tokens>=0)`),
	mysqlIfNoConstraint("jobs", "jobs_cache_write_tokens_check", `ALTER TABLE jobs ADD CONSTRAINT jobs_cache_write_tokens_check CHECK(cache_write_tokens IS NULL OR cache_write_tokens>=0)`),

	// Expand the relay_events stub. Prefixes preserve the old owner_lane index.
	mysqlIfNoColumn("relay_events", "kind", `ALTER TABLE relay_events ADD COLUMN kind VARCHAR(32) NOT NULL DEFAULT ('job.completed')`),
	mysqlIfNoColumn("relay_events", "job_id", `ALTER TABLE relay_events ADD COLUMN job_id VARCHAR(128) NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "epoch", `ALTER TABLE relay_events ADD COLUMN epoch INT NOT NULL DEFAULT 0`),
	mysqlIfNoColumn("relay_events", "machine", `ALTER TABLE relay_events ADD COLUMN machine LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "pane_id", `ALTER TABLE relay_events ADD COLUMN pane_id LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "report_path", `ALTER TABLE relay_events ADD COLUMN report_path LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "report_last_line", `ALTER TABLE relay_events ADD COLUMN report_last_line LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "question", `ALTER TABLE relay_events ADD COLUMN question LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "pr", `ALTER TABLE relay_events ADD COLUMN pr LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "head", `ALTER TABLE relay_events ADD COLUMN head LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "reason", `ALTER TABLE relay_events ADD COLUMN reason LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "event_time", `ALTER TABLE relay_events ADD COLUMN event_time DATETIME(6) NULL`),
	mysqlIfNoColumn("relay_events", "received_at", `ALTER TABLE relay_events ADD COLUMN received_at DATETIME(6) NOT NULL DEFAULT ('1970-01-01 00:00:00.000000')`),
	mysqlIfNoColumn("relay_events", "delivered_at", `ALTER TABLE relay_events ADD COLUMN delivered_at DATETIME(6) NULL`),
	mysqlIfNoColumn("relay_events", "delivered_to", `ALTER TABLE relay_events ADD COLUMN delivered_to LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "attempts", `ALTER TABLE relay_events ADD COLUMN attempts INT NOT NULL DEFAULT 0`),
	mysqlIfNoColumn("relay_events", "event_id", `ALTER TABLE relay_events ADD COLUMN event_id VARCHAR(256) NOT NULL DEFAULT ('')`),
	mysqlIfNoColumn("relay_events", "text", `ALTER TABLE relay_events ADD COLUMN text LONGTEXT NOT NULL DEFAULT ('')`),
	mysqlIfNoIndex("relay_events", "relay_events_idempotency", `CREATE UNIQUE INDEX relay_events_idempotency ON relay_events((CASE WHEN kind IN ('job.completed','job.escalate','job.joined','job.lost','job.revoked') THEN kind END),(CASE WHEN kind IN ('job.completed','job.escalate','job.joined','job.lost','job.revoked') THEN job_id END),(CASE WHEN kind IN ('job.completed','job.escalate','job.joined','job.lost','job.revoked') THEN epoch END),(CASE WHEN kind IN ('job.completed','job.escalate','job.joined','job.lost','job.revoked') THEN UNHEX(SHA2(report_path,256)) END),(CASE WHEN kind IN ('job.completed','job.escalate','job.joined','job.lost','job.revoked') THEN UNHEX(SHA2(reason,256)) END))`),
	mysqlIfNoIndex("relay_events", "relay_events_lane_event_idempotency", `CREATE UNIQUE INDEX relay_events_lane_event_idempotency ON relay_events((CASE WHEN kind='lane.event' THEN owner_lane END),(CASE WHEN kind='lane.event' THEN event_id END))`),
	mysqlIfNoIndex("relay_events", "relay_events_undelivered_kind_id", `CREATE INDEX relay_events_undelivered_kind_id ON relay_events(kind,id)`),
	mysqlIfNoIndex("relay_events", "relay_events_owner_lane", `CREATE INDEX relay_events_owner_lane ON relay_events(owner_lane)`),

	`CREATE TABLE IF NOT EXISTS notification_outbox (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, kind VARCHAR(32) NOT NULL, target_lane VARCHAR(255) NOT NULL, event_id VARCHAR(256) NOT NULL, text LONGTEXT NOT NULL, created_at DATETIME(6) NOT NULL, sent_at DATETIME(6), hub_row_id BIGINT, applied_at DATETIME(6), applied_by VARCHAR(128), skipped_at DATETIME(6), skip_reason VARCHAR(128), UNIQUE KEY notification_outbox_event_id(event_id), KEY notification_outbox_unsent_live(id,sent_at,skipped_at), CONSTRAINT notification_outbox_kind_check CHECK(kind IN ('decision_answered','chat_answer','dots_report','dots_decision','dots_blocked','dots_followup','yt_write_op','decision_denied','disposition_answered','deploy_result','deploy_lag')), CONSTRAINT notification_outbox_applied_after_sent CHECK(applied_at IS NULL OR sent_at IS NOT NULL), CONSTRAINT notification_outbox_skip_reason CHECK(skipped_at IS NULL OR skip_reason IS NOT NULL))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS mcp_write_audit (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, tool VARCHAR(64) NOT NULL, actor VARCHAR(320) NOT NULL, channel VARCHAR(32) NOT NULL, author VARCHAR(128) NOT NULL, task_id BIGINT, idempotency_key VARCHAR(128) NOT NULL DEFAULT '', result JSON, at DATETIME(6) NOT NULL)` + mysqlTableOptions,
	mysqlIfNoIndex("mcp_write_audit", "mcp_write_audit_key", `CREATE UNIQUE INDEX mcp_write_audit_key ON mcp_write_audit((CASE WHEN idempotency_key<>'' THEN tool END),(CASE WHEN idempotency_key<>'' THEN actor END),(CASE WHEN idempotency_key<>'' THEN idempotency_key END))`),
	`CREATE TABLE IF NOT EXISTS placement_decisions (id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, job_id VARCHAR(128) NOT NULL, attempt INT NOT NULL DEFAULT 1, task_id BIGINT, decided_at DATETIME(6) NOT NULL, decided_on VARCHAR(128) NOT NULL, tool_version LONGTEXT NOT NULL DEFAULT ('') , source VARCHAR(16) NOT NULL, ranking LONGTEXT NOT NULL, state_source LONGTEXT NOT NULL, chosen_host VARCHAR(128), outcome VARCHAR(16) NOT NULL, reason LONGTEXT NOT NULL DEFAULT ('') , profile LONGTEXT NOT NULL DEFAULT ('') , pool LONGTEXT NOT NULL DEFAULT ('') , account_key LONGTEXT NOT NULL DEFAULT ('') , family LONGTEXT NOT NULL DEFAULT ('') , model LONGTEXT NOT NULL DEFAULT ('') , role LONGTEXT NOT NULL DEFAULT ('') , pane_id LONGTEXT NOT NULL DEFAULT ('') , repo LONGTEXT NOT NULL DEFAULT ('') , candidates JSON NOT NULL, UNIQUE KEY placement_decisions_job_id_attempt_key(job_id,attempt), KEY placement_decisions_task(task_id,decided_at,id), KEY placement_decisions_at(decided_at,id), KEY placement_decisions_host(chosen_host,decided_at,id), CONSTRAINT placement_decisions_attempt_check CHECK(attempt>0), CONSTRAINT placement_decisions_source_check CHECK(source IN ('auto','forced')), CONSTRAINT placement_decisions_outcome_check CHECK(outcome IN ('placed','refused','failed')))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS attachments (sha256 CHAR(64) NOT NULL PRIMARY KEY, size_bytes BIGINT NOT NULL, mime LONGTEXT NOT NULL, original_name LONGTEXT NOT NULL, created_by LONGTEXT NOT NULL, created_at DATETIME(6) NOT NULL, object_location LONGTEXT NULL, archived_at DATETIME(6) NULL, archive_location LONGTEXT NOT NULL DEFAULT ('') , hot_deleted_at DATETIME(6) NULL, CONSTRAINT attachments_size_bytes_check CHECK(size_bytes>=0))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS attachment_refs (sha256 CHAR(64) NOT NULL, ref_kind VARCHAR(16) NOT NULL, ref_id VARCHAR(512) NOT NULL DEFAULT '', PRIMARY KEY(sha256,ref_kind,ref_id), KEY attachment_refs_target(ref_kind,ref_id), CONSTRAINT attachment_refs_attachment_fk FOREIGN KEY(sha256) REFERENCES attachments(sha256) ON DELETE RESTRICT, CONSTRAINT attachment_refs_ref_kind_check CHECK(ref_kind IN ('checkpoint','document','memory','task','none')))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS attachment_uploads (id CHAR(32) NOT NULL PRIMARY KEY, staging_key LONGTEXT NOT NULL, name LONGTEXT NOT NULL, mime LONGTEXT NOT NULL, size_bytes BIGINT NOT NULL, sha256 CHAR(64) NOT NULL, ref_kind VARCHAR(16) NOT NULL, ref_id LONGTEXT NOT NULL DEFAULT ('') , created_by LONGTEXT NOT NULL, status VARCHAR(16) NOT NULL, reason LONGTEXT NOT NULL DEFAULT ('') , created_at DATETIME(6) NOT NULL, expires_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL, staging_deleted_at DATETIME(6) NULL, claim_nonce VARCHAR(128) NULL, KEY attachment_uploads_charged_created(created_at,staging_deleted_at), KEY attachment_uploads_charged_client(created_by(128),staging_deleted_at), CONSTRAINT attachment_uploads_size_check CHECK(size_bytes>=0), CONSTRAINT attachment_uploads_ref_kind_check CHECK(ref_kind IN ('checkpoint','document','memory','task','none')), CONSTRAINT attachment_uploads_status_check CHECK(status IN ('pending','completing','done','refused')))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS attachment_usage (month CHAR(7) NOT NULL PRIMARY KEY, puts BIGINT NOT NULL DEFAULT 0, gets BIGINT NOT NULL DEFAULT 0, bytes_added BIGINT NOT NULL DEFAULT 0, CONSTRAINT attachment_usage_month_check CHECK(month REGEXP '^[0-9]{4}-[0-9]{2}$'))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS ledger_cursors (consumer VARCHAR(128) NOT NULL PRIMARY KEY, acked_sync_id BIGINT NOT NULL, acked_at DATETIME(6) NOT NULL, updated_by LONGTEXT NOT NULL, CONSTRAINT ledger_cursors_acked_nonnegative CHECK(acked_sync_id>=0))` + mysqlTableOptions,
}
