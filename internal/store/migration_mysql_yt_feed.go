package store

// The rest of the YouTrack area on MySQL (hk 5077, hk 4899 B4b): which article
// each document is, the feed tracker, the issue-link ledger and the
// automatic binder's state. yt_write_ops and its guards, yt_task_bindings,
// yt_decision_bindings, yt_binding_history, yt_feed_cursors and
// youtrack_projects arrived with B4a (migration_mysql_yt_write_ops.go) and are
// not repeated here; this migration adds only the tables that area still
// lacked.
//
// Decisions recorded here, each with the PostgreSQL object it replaces:
//   - TEXT columns that sit in a key become bounded VARCHARs at the length the
//     PostgreSQL CHECK allows (article/issue/result ids 64, tenant 255,
//     entity_id 512, actor/bound_by 200). Free text not in a key is LONGTEXT
//     or, where the CHECK bounds it, VARCHAR of that bound.
//   - Partial unique indexes become functional unique keys named after the
//     PostgreSQL index: each part is NULL when the predicate is false, which
//     keeps every released link and every closed unowned row out of the key.
//   - TIMESTAMPTZ is DATETIME(6) in UTC, JSONB is JSON, BOOLEAN is TINYINT(1).
//   - yt_link_plans is append-only, as yt_binding_history: BEFORE UPDATE and
//     BEFORE DELETE triggers under the @hk_bulk_load bypass. TRUNCATE is
//     refused by privilege (the application user has no DROP).
//
// Locks: each CREATE TABLE locks only its new, empty table. The triggers and
// indexes are on those new tables. No existing table is locked, so the
// previous release keeps serving during blue-green.
var mysqlYTFeedSteps = []string{
	`CREATE TABLE IF NOT EXISTS yt_article_bindings (
		document_id BIGINT NOT NULL PRIMARY KEY,
		tenant VARCHAR(255) NOT NULL,
		article_id VARCHAR(64) NOT NULL,
		article_readable VARCHAR(64) NOT NULL DEFAULT '',
		project_id VARCHAR(64) NOT NULL,
		part INT NOT NULL DEFAULT 0,
		generation INT NOT NULL DEFAULT 0,
		content_sha VARCHAR(64),
		origin VARCHAR(16) NOT NULL,
		op_id BIGINT,
		needs_update BOOLEAN NOT NULL DEFAULT FALSE,
		bound_by VARCHAR(200) NOT NULL,
		bound_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_article_bindings_document_id_check CHECK(document_id > 0),
		CONSTRAINT yt_article_bindings_tenant_check CHECK(tenant <> '' AND CHAR_LENGTH(tenant) <= 255),
		CONSTRAINT yt_article_bindings_article_id_check CHECK(article_id <> '' AND CHAR_LENGTH(article_id) <= 64),
		CONSTRAINT yt_article_bindings_article_readable_check CHECK(CHAR_LENGTH(article_readable) <= 64),
		CONSTRAINT yt_article_bindings_project_id_check CHECK(project_id <> '' AND CHAR_LENGTH(project_id) <= 64),
		CONSTRAINT yt_article_bindings_part_check CHECK(part >= 0),
		CONSTRAINT yt_article_bindings_generation_check CHECK(generation >= 0),
		CONSTRAINT yt_article_bindings_content_sha_check CHECK(content_sha IS NULL OR content_sha REGEXP '^[0-9a-f]{64}$'),
		CONSTRAINT yt_article_bindings_origin_check CHECK(origin IN ('import_verified','op_applied')),
		CONSTRAINT yt_article_bindings_bound_by_check CHECK(bound_by <> '' AND CHAR_LENGTH(bound_by) <= 200),
		UNIQUE KEY yt_article_bindings_tenant_article_id_key(tenant, article_id))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_snapshot_state (
		entity_kind VARCHAR(64) NOT NULL,
		entity_id VARCHAR(512) NOT NULL,
		kind VARCHAR(64) NOT NULL,
		seq BIGINT NOT NULL,
		last_sha LONGTEXT NOT NULL,
		last_op_id BIGINT NOT NULL,
		CONSTRAINT yt_snapshot_state_entity_kind_check CHECK(entity_kind <> '' AND CHAR_LENGTH(entity_kind) <= 64),
		CONSTRAINT yt_snapshot_state_entity_id_check CHECK(entity_id <> '' AND CHAR_LENGTH(entity_id) <= 512),
		CONSTRAINT yt_snapshot_state_kind_check CHECK(kind <> '' AND CHAR_LENGTH(kind) <= 64),
		CONSTRAINT yt_snapshot_state_seq_check CHECK(seq > 0),
		PRIMARY KEY(entity_kind, entity_id, kind))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_link_plans (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		op_id BIGINT NOT NULL,
		attempt INT NOT NULL,
		recover INT NOT NULL DEFAULT 0,
		plan JSON NOT NULL,
		created_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_link_plans_op_id_check CHECK(op_id > 0),
		CONSTRAINT yt_link_plans_attempt_check CHECK(attempt >= 0),
		CONSTRAINT yt_link_plans_recover_check CHECK(recover >= 0),
		UNIQUE KEY yt_link_plans_op_id_attempt_recover_key(op_id, attempt, recover))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_link_steps (
		op_id BIGINT NOT NULL,
		attempt INT NOT NULL,
		recover INT NOT NULL,
		step INT NOT NULL,
		marked_at DATETIME(6) NOT NULL,
		outcome VARCHAR(16),
		CONSTRAINT yt_link_steps_op_id_check CHECK(op_id > 0),
		CONSTRAINT yt_link_steps_attempt_check CHECK(attempt >= 0),
		CONSTRAINT yt_link_steps_recover_check CHECK(recover >= 0),
		CONSTRAINT yt_link_steps_step_check CHECK(step >= 0),
		CONSTRAINT yt_link_steps_outcome_check CHECK(outcome IN ('ok','definite','ambiguous')),
		PRIMARY KEY(op_id, attempt, recover, step))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_link_owned (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		tenant VARCHAR(255) NOT NULL,
		link_type VARCHAR(16) NOT NULL,
		from_issue VARCHAR(64) NOT NULL,
		to_issue VARCHAR(64) NOT NULL,
		writer_task_id BIGINT NOT NULL,
		target_task_id BIGINT NOT NULL,
		kind VARCHAR(16) NOT NULL,
		intended_op_id BIGINT NOT NULL,
		intended_at DATETIME(6) NOT NULL,
		released_op_id BIGINT,
		released_at DATETIME(6),
		CONSTRAINT yt_link_owned_tenant_check CHECK(tenant <> '' AND CHAR_LENGTH(tenant) <= 255),
		CONSTRAINT yt_link_owned_link_type_check CHECK(link_type IN ('subtask','relates','duplicate')),
		CONSTRAINT yt_link_owned_from_issue_check CHECK(from_issue <> '' AND CHAR_LENGTH(from_issue) <= 64),
		CONSTRAINT yt_link_owned_to_issue_check CHECK(to_issue <> '' AND CHAR_LENGTH(to_issue) <= 64),
		CONSTRAINT yt_link_owned_writer_task_id_check CHECK(writer_task_id > 0),
		CONSTRAINT yt_link_owned_target_task_id_check CHECK(target_task_id > 0),
		CONSTRAINT yt_link_owned_intended_op_id_check CHECK(intended_op_id > 0),
		CONSTRAINT yt_link_owned_released_op_id_check CHECK(released_op_id > 0),
		CONSTRAINT yt_link_owned_ends_check CHECK(from_issue <> to_issue AND writer_task_id <> target_task_id),
		CONSTRAINT yt_link_owned_kind_check CHECK((kind = 'parent' AND link_type = 'subtask') OR (kind = 'related' AND link_type = 'relates')
			OR (kind = 'duplicate_of' AND link_type = 'duplicate')),
		CONSTRAINT yt_link_owned_released_check CHECK(released_at IS NULL OR released_op_id IS NOT NULL),
		UNIQUE KEY yt_link_owned_live(
			(CASE WHEN released_op_id IS NULL THEN tenant END),
			(CASE WHEN released_op_id IS NULL THEN link_type END),
			(CASE WHEN released_op_id IS NULL THEN from_issue END),
			(CASE WHEN released_op_id IS NULL THEN to_issue END)),
		KEY yt_link_owned_from(tenant, from_issue),
		KEY yt_link_owned_to(tenant, to_issue),
		KEY yt_link_owned_releasing(released_op_id))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_link_unowned (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		tenant VARCHAR(255) NOT NULL,
		issue_id VARCHAR(64) NOT NULL,
		link_type VARCHAR(16) NOT NULL,
		side VARCHAR(32) NOT NULL,
		other_issue VARCHAR(64) NOT NULL,
		first_seen_op_id BIGINT NOT NULL,
		last_seen_op_id BIGINT NOT NULL,
		first_seen_at DATETIME(6) NOT NULL,
		last_seen_at DATETIME(6) NOT NULL,
		gone_at DATETIME(6),
		CONSTRAINT yt_link_unowned_tenant_check CHECK(tenant <> '' AND CHAR_LENGTH(tenant) <= 255),
		CONSTRAINT yt_link_unowned_issue_id_check CHECK(issue_id <> '' AND CHAR_LENGTH(issue_id) <= 64),
		CONSTRAINT yt_link_unowned_link_type_check CHECK(link_type IN ('subtask','relates','duplicate')),
		CONSTRAINT yt_link_unowned_side_check CHECK(side <> '' AND CHAR_LENGTH(side) <= 32),
		CONSTRAINT yt_link_unowned_other_issue_check CHECK(other_issue <> '' AND CHAR_LENGTH(other_issue) <= 64),
		CONSTRAINT yt_link_unowned_first_seen_op_id_check CHECK(first_seen_op_id > 0),
		CONSTRAINT yt_link_unowned_seen_order_check CHECK(last_seen_op_id >= first_seen_op_id),
		UNIQUE KEY yt_link_unowned_open(
			(CASE WHEN gone_at IS NULL THEN tenant END),
			(CASE WHEN gone_at IS NULL THEN issue_id END),
			(CASE WHEN gone_at IS NULL THEN link_type END),
			(CASE WHEN gone_at IS NULL THEN side END),
			(CASE WHEN gone_at IS NULL THEN other_issue END)))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_links_sweep (
		singleton BOOLEAN NOT NULL PRIMARY KEY DEFAULT TRUE,
		generation BIGINT NOT NULL DEFAULT 0,
		done_generation BIGINT NOT NULL DEFAULT 0,
		after_task_id BIGINT NOT NULL DEFAULT 0,
		updated_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_links_sweep_singleton_check CHECK(singleton = 1),
		CONSTRAINT yt_links_sweep_generation_check CHECK(generation >= 0),
		CONSTRAINT yt_links_sweep_done_check CHECK(done_generation >= 0 AND done_generation <= generation),
		CONSTRAINT yt_links_sweep_after_task_id_check CHECK(after_task_id >= 0))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_binding_intents (
		task_id BIGINT NOT NULL PRIMARY KEY,
		tenant VARCHAR(255) NOT NULL,
		state VARCHAR(16) NOT NULL,
		reason VARCHAR(200) NOT NULL DEFAULT '',
		attempts INT NOT NULL DEFAULT 0,
		last_error VARCHAR(2000) NOT NULL DEFAULT '',
		next_attempt_at DATETIME(6) NOT NULL,
		recorded_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_binding_intents_task_id_check CHECK(task_id > 0),
		CONSTRAINT yt_binding_intents_tenant_check CHECK(tenant <> ''),
		CONSTRAINT yt_binding_intents_state_check CHECK(state IN ('pending','refused','error','excluded')),
		CONSTRAINT yt_binding_intents_reason_check CHECK(CHAR_LENGTH(reason) <= 200),
		CONSTRAINT yt_binding_intents_attempts_check CHECK(attempts >= 0),
		CONSTRAINT yt_binding_intents_last_error_check CHECK(CHAR_LENGTH(last_error) <= 2000),
		KEY yt_binding_intents_due(next_attempt_at, task_id))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_binding_cursor (
		id SMALLINT NOT NULL PRIMARY KEY,
		last_comment_id BIGINT NOT NULL DEFAULT 0,
		CONSTRAINT yt_binding_cursor_id_check CHECK(id = 1),
		CONSTRAINT yt_binding_cursor_last_comment_id_check CHECK(last_comment_id >= 0))` + mysqlTableOptions,
	`INSERT INTO yt_binding_cursor(id, last_comment_id) VALUES (1, 0) ON DUPLICATE KEY UPDATE id=id`,

	`CREATE TABLE IF NOT EXISTS yt_family_roots (
		tenant VARCHAR(255) NOT NULL DEFAULT '',
		project_id VARCHAR(64) NOT NULL,
		family VARCHAR(64) NOT NULL,
		article_id VARCHAR(64) NOT NULL,
		op_id BIGINT NOT NULL,
		created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
		CONSTRAINT yt_family_roots_tenant_check CHECK(CHAR_LENGTH(tenant) <= 255),
		CONSTRAINT yt_family_roots_project_id_check CHECK(project_id <> '' AND CHAR_LENGTH(project_id) <= 64),
		CONSTRAINT yt_family_roots_family_check CHECK(family <> '' AND CHAR_LENGTH(family) <= 64),
		CONSTRAINT yt_family_roots_article_id_check CHECK(article_id <> '' AND CHAR_LENGTH(article_id) <= 64),
		PRIMARY KEY(tenant, project_id, family),
		CONSTRAINT yt_family_roots_op_id_fkey FOREIGN KEY(op_id) REFERENCES yt_write_ops(id) ON DELETE RESTRICT)` + mysqlTableOptions,
}

// yt_link_plans is append-only, as yt_binding_history: a plan is the record of
// one dispatch attempt and is never rewritten.
var mysqlYTFeedTriggers = []string{
	`CREATE TRIGGER IF NOT EXISTS yt_link_plans_guard_update BEFORE UPDATE ON yt_link_plans FOR EACH ROW ` +
		mysqlBulkGuard(`SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_link_plans is append-only';`),
	`CREATE TRIGGER IF NOT EXISTS yt_link_plans_guard_delete BEFORE DELETE ON yt_link_plans FOR EACH ROW ` +
		mysqlBulkGuard(`SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_link_plans is append-only';`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261118-mysql-yt-feed", after: []string{"20261118-mysql-yt-write-ops"},
		steps: append(mysqlYTFeedSteps, mysqlYTFeedTriggers...)})
}
