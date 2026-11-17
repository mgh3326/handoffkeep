package store

// The YouTrack write ops area on MySQL (hk 5076, hk 4899 B4a): the op log and
// its guards, the comment-copy tables and the task binding the issue-apply
// guard needs. DDL takes metadata locks on the new tables only; the stubs for
// tables other areas own (youtrack_projects, yt_binding_history,
// yt_feed_cursors, yt_decision_bindings) are created whole here because the
// guard triggers and reads name them — B4b/B6/B7 extend them if their own
// columns differ (guarded ALTERs, as for jobs in 20261118-mysql-tasks).
//
// Decisions recorded here, each with the PostgreSQL object it replaces:
//   - TEXT columns that sit in a key become bounded VARCHARs at the length the
//     CHECK allows (entity_id, idem_key, principal, actor 200 -> VARCHAR(200);
//     tenant 255; issue/result ids 64; kinds 64). LONGTEXT where the CHECK
//     bound is larger than 768 bytes x 4 (last_error 2000, note 2000,
//     payload_sha stays VARCHAR(128)). Free text not in a key is LONGTEXT.
//   - Partial indexes become functional keys: the expression is the CASE that
//     is NULL exactly when the PostgreSQL predicate is false, and the key name
//     equals PostgreSQL's so the drift check pairs them.
//   - yt_write_ops.idem key UNIQUE(principal,kind,idem_key) is named after
//     PostgreSQL's constraint name; every other key keeps its index name.
//   - The four guards of yt_write_ops become BEFORE UPDATE/DELETE triggers
//     with SIGNAL under the @hk_bulk_load bypass (design §11.6): the lifecycle
//     state machine (final version of handoffkeep_yt_write_ops_guard, incl.
//     the hk 4263 retarget and hk 4318 reissue exceptions and the unused
//     hk 4457 hold rule), the tenant immutability guard, and the two apply
//     guards that require the binding (and map) rows before an issue_create
//     or decision_create op may be marked applied. IS DISTINCT FROM reads
//     NOT (a <=> b).
//   - youtrack_comment_links is left out of MySQL (an empty retired table):
//     the catch-up and audit EXISTS clauses that read it are dropped from the
//     MySQL texts.
var mysqlYTOpsSteps = []string{
	`CREATE TABLE IF NOT EXISTS yt_write_ops (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		principal VARCHAR(200) NOT NULL,
		kind VARCHAR(64) NOT NULL,
		idem_key VARCHAR(200) NOT NULL,
		payload_sha VARCHAR(128) NOT NULL,
		payload JSON NOT NULL,
		entity_kind VARCHAR(64) NOT NULL,
		entity_id VARCHAR(200) NOT NULL,
		yt_target_id VARCHAR(64) NOT NULL DEFAULT '',
		actor VARCHAR(200) NOT NULL,
		parent_id BIGINT,
		child_seq INT NOT NULL DEFAULT 0,
		state VARCHAR(16) NOT NULL,
		yt_result_id VARCHAR(64) NOT NULL DEFAULT '',
		reason VARCHAR(200) NOT NULL DEFAULT '',
		last_error VARCHAR(2000) NOT NULL DEFAULT '',
		attempts INT NOT NULL DEFAULT 0,
		reconciles INT NOT NULL DEFAULT 0,
		created_at DATETIME(6) NOT NULL,
		dispatched_at DATETIME(6),
		next_attempt_at DATETIME(6) NOT NULL,
		resolved_at DATETIME(6),
		tenant VARCHAR(255) NOT NULL DEFAULT '',
		CONSTRAINT yt_write_ops_principal_check CHECK(principal <> '' AND CHAR_LENGTH(principal) <= 200),
		CONSTRAINT yt_write_ops_kind_check CHECK(kind REGEXP '^[a-z][a-z0-9_]{0,63}$'),
		CONSTRAINT yt_write_ops_idem_key_check CHECK(idem_key <> '' AND CHAR_LENGTH(idem_key) <= 200),
		CONSTRAINT yt_write_ops_payload_sha_check CHECK(payload_sha <> '' AND CHAR_LENGTH(payload_sha) <= 128),
		CONSTRAINT yt_write_ops_entity_kind_check CHECK(entity_kind REGEXP '^[a-z][a-z0-9_]{0,63}$'),
		CONSTRAINT yt_write_ops_entity_id_check CHECK(entity_id <> '' AND CHAR_LENGTH(entity_id) <= 200),
		CONSTRAINT yt_write_ops_yt_target_id_check CHECK(CHAR_LENGTH(yt_target_id) <= 64),
		CONSTRAINT yt_write_ops_actor_check CHECK(actor <> '' AND CHAR_LENGTH(actor) <= 200),
		CONSTRAINT yt_write_ops_child_seq_check CHECK(child_seq >= 0),
		CONSTRAINT yt_write_ops_state_check CHECK(state IN ('prepared','dispatched','applied','rejected','remote_unknown')),
		CONSTRAINT yt_write_ops_yt_result_id_check CHECK(CHAR_LENGTH(yt_result_id) <= 64),
		CONSTRAINT yt_write_ops_reason_check CHECK(CHAR_LENGTH(reason) <= 200),
		CONSTRAINT yt_write_ops_last_error_check CHECK(CHAR_LENGTH(last_error) <= 2000),
		CONSTRAINT yt_write_ops_attempts_check CHECK(attempts >= 0),
		CONSTRAINT yt_write_ops_reconciles_check CHECK(reconciles >= 0),
		CONSTRAINT yt_write_ops_tenant_check CHECK(CHAR_LENGTH(tenant) <= 255),
		CONSTRAINT yt_write_ops_resolved_check CHECK((state IN ('applied','rejected')) = (resolved_at IS NOT NULL)),
		CONSTRAINT yt_write_ops_applied_check CHECK(state <> 'applied' OR yt_result_id <> ''),
		UNIQUE KEY yt_write_ops_principal_kind_idem_key_key(principal, kind, idem_key),
		KEY yt_write_ops_entity_open(entity_kind, entity_id, id),
		KEY yt_write_ops_due(next_attempt_at, id),
		KEY yt_write_ops_dispatched(dispatched_at, id),
		KEY yt_write_ops_alert_scan(created_at, id),
		KEY yt_write_ops_binding_conflict(reason, id),
		KEY yt_write_ops_comment_requeue(kind, state, reason, tenant, id),
		CONSTRAINT yt_write_ops_parent_id_fkey FOREIGN KEY(parent_id) REFERENCES yt_write_ops(id) ON DELETE RESTRICT)` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_entity_map (
		entity_kind VARCHAR(64) NOT NULL,
		hk_key VARCHAR(200) NOT NULL,
		yt_id VARCHAR(64) NOT NULL,
		yt_parent_id VARCHAR(64) NOT NULL DEFAULT '',
		op_id BIGINT NOT NULL,
		created_at DATETIME(6) NOT NULL,
		tenant VARCHAR(255) NOT NULL DEFAULT '',
		CONSTRAINT yt_entity_map_entity_kind_check CHECK(entity_kind REGEXP '^[a-z][a-z0-9_]{0,63}$'),
		CONSTRAINT yt_entity_map_hk_key_check CHECK(hk_key <> '' AND CHAR_LENGTH(hk_key) <= 200),
		CONSTRAINT yt_entity_map_yt_id_check CHECK(yt_id <> '' AND CHAR_LENGTH(yt_id) <= 64),
		CONSTRAINT yt_entity_map_yt_parent_id_check CHECK(CHAR_LENGTH(yt_parent_id) <= 64),
		CONSTRAINT yt_entity_map_tenant_check CHECK(CHAR_LENGTH(tenant) <= 255),
		PRIMARY KEY(entity_kind, hk_key),
		UNIQUE KEY yt_entity_map_entity_kind_tenant_yt_id_key(entity_kind, tenant, yt_id),
		CONSTRAINT yt_entity_map_op_id_fkey FOREIGN KEY(op_id) REFERENCES yt_write_ops(id) ON DELETE RESTRICT)` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_task_bindings (
		task_id BIGINT NOT NULL PRIMARY KEY,
		tenant VARCHAR(255) NOT NULL,
		issue_id VARCHAR(64) NOT NULL,
		issue_readable VARCHAR(64) NOT NULL DEFAULT '',
		project_id VARCHAR(64) NOT NULL DEFAULT '',
		bound_by VARCHAR(200) NOT NULL,
		bound_at DATETIME(6) NOT NULL,
		comments_from BIGINT NOT NULL DEFAULT 0,
		origin VARCHAR(32) NOT NULL DEFAULT 'import_verified',
		op_id BIGINT,
		CONSTRAINT yt_task_bindings_task_id_check CHECK(task_id > 0),
		CONSTRAINT yt_task_bindings_tenant_check CHECK(tenant <> '' AND CHAR_LENGTH(tenant) <= 255),
		CONSTRAINT yt_task_bindings_issue_id_check CHECK(issue_id <> '' AND CHAR_LENGTH(issue_id) <= 64),
		CONSTRAINT yt_task_bindings_issue_readable_check CHECK(CHAR_LENGTH(issue_readable) <= 64),
		CONSTRAINT yt_task_bindings_project_id_check CHECK(CHAR_LENGTH(project_id) <= 64),
		CONSTRAINT yt_task_bindings_bound_by_check CHECK(bound_by <> '' AND CHAR_LENGTH(bound_by) <= 200),
		CONSTRAINT yt_task_bindings_comments_from_check CHECK(comments_from >= 0),
		CONSTRAINT yt_task_bindings_origin_check CHECK(origin IN ('import_verified','op_applied','auto_verified')),
		UNIQUE KEY yt_task_bindings_tenant_issue_id_key(tenant, issue_id))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_comment_canary (
		task_id BIGINT NOT NULL PRIMARY KEY,
		tenant VARCHAR(255) NOT NULL,
		issue_id VARCHAR(64) NOT NULL,
		issue_readable VARCHAR(64) NOT NULL DEFAULT '',
		project_id VARCHAR(64) NOT NULL DEFAULT '',
		added_by VARCHAR(200) NOT NULL,
		added_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_comment_canary_task_id_check CHECK(task_id > 0),
		CONSTRAINT yt_comment_canary_tenant_check CHECK(tenant <> '' AND CHAR_LENGTH(tenant) <= 255),
		CONSTRAINT yt_comment_canary_issue_id_check CHECK(issue_id <> '' AND CHAR_LENGTH(issue_id) <= 64),
		CONSTRAINT yt_comment_canary_issue_readable_check CHECK(CHAR_LENGTH(issue_readable) <= 64),
		CONSTRAINT yt_comment_canary_project_id_check CHECK(CHAR_LENGTH(project_id) <= 64),
		CONSTRAINT yt_comment_canary_added_by_check CHECK(added_by <> '' AND CHAR_LENGTH(added_by) <= 200),
		UNIQUE KEY yt_comment_canary_tenant_issue_id_key(tenant, issue_id))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_comment_write_links (
		comment_id BIGINT NOT NULL PRIMARY KEY,
		op_id BIGINT NOT NULL,
		created_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_comment_write_links_comment_id_check CHECK(comment_id > 0),
		UNIQUE KEY yt_comment_write_links_op_id_key(op_id),
		CONSTRAINT yt_comment_write_links_op_id_fkey FOREIGN KEY(op_id) REFERENCES yt_write_ops(id) ON DELETE RESTRICT)` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_authority (
		kind VARCHAR(16) NOT NULL PRIMARY KEY,
		authority VARCHAR(16) NOT NULL DEFAULT 'hk',
		switched_at DATETIME(6),
		switched_by VARCHAR(200) NOT NULL DEFAULT '',
		note VARCHAR(2000) NOT NULL DEFAULT '',
		CONSTRAINT yt_authority_kind_check CHECK(kind IN ('tasks','comments','documents','decisions')),
		CONSTRAINT yt_authority_authority_check CHECK(authority IN ('hk','youtrack')),
		CONSTRAINT yt_authority_switched_by_check CHECK(CHAR_LENGTH(switched_by) <= 200),
		CONSTRAINT yt_authority_note_check CHECK(CHAR_LENGTH(note) <= 2000))` + mysqlTableOptions,
	`INSERT INTO yt_authority(kind) VALUES ('tasks'),('comments'),('documents'),('decisions') ON DUPLICATE KEY UPDATE kind=kind`,

	`CREATE TABLE IF NOT EXISTS yt_write_op_alerts (
		op_id BIGINT NOT NULL,
		alert_class VARCHAR(16) NOT NULL,
		created_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_write_op_alerts_class_check CHECK(alert_class IN ('stuck','rejected')),
		PRIMARY KEY(op_id, alert_class))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_decision_bindings (
		decision_id VARCHAR(20) NOT NULL PRIMARY KEY,
		tenant VARCHAR(255) NOT NULL,
		issue_id VARCHAR(64) NOT NULL,
		issue_readable VARCHAR(64) NOT NULL DEFAULT '',
		project_id VARCHAR(64) NOT NULL DEFAULT '',
		origin VARCHAR(32) NOT NULL,
		op_id BIGINT,
		bound_by VARCHAR(200) NOT NULL,
		bound_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_decision_bindings_decision_id_check CHECK(decision_id REGEXP '^[1-9][0-9]{0,18}$'),
		CONSTRAINT yt_decision_bindings_tenant_check CHECK(tenant <> '' AND CHAR_LENGTH(tenant) <= 255),
		CONSTRAINT yt_decision_bindings_issue_id_check CHECK(issue_id <> '' AND CHAR_LENGTH(issue_id) <= 64),
		CONSTRAINT yt_decision_bindings_issue_readable_check CHECK(CHAR_LENGTH(issue_readable) <= 64),
		CONSTRAINT yt_decision_bindings_project_id_check CHECK(CHAR_LENGTH(project_id) <= 64),
		CONSTRAINT yt_decision_bindings_origin_check CHECK(origin IN ('import_verified','op_applied')),
		CONSTRAINT yt_decision_bindings_bound_by_check CHECK(bound_by <> '' AND CHAR_LENGTH(bound_by) <= 200),
		UNIQUE KEY yt_decision_bindings_tenant_issue_id_key(tenant, issue_id))` + mysqlTableOptions,

	`CREATE TABLE IF NOT EXISTS yt_write_op_adoptions (
		op_id BIGINT NOT NULL PRIMARY KEY,
		attempts INT NOT NULL,
		yt_id VARCHAR(64) NOT NULL,
		recorded_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_write_op_adoptions_op_id_check CHECK(op_id > 0),
		CONSTRAINT yt_write_op_adoptions_attempts_check CHECK(attempts > 0),
		CONSTRAINT yt_write_op_adoptions_yt_id_check CHECK(yt_id <> '' AND CHAR_LENGTH(yt_id) <= 64))` + mysqlTableOptions,

	// The issue_create lane: an intent to create a task's issue once its
	// project maps, and the comment watermark an adopted issue's binding
	// starts from.
	`CREATE TABLE IF NOT EXISTS yt_issue_intents (
		task_id BIGINT NOT NULL PRIMARY KEY,
		tenant VARCHAR(255) NOT NULL,
		recorded_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_issue_intents_task_id_check CHECK(task_id > 0),
		CONSTRAINT yt_issue_intents_tenant_check CHECK(tenant <> ''))` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS yt_issue_comment_bounds (
		op_id BIGINT NOT NULL PRIMARY KEY,
		task_id BIGINT NOT NULL,
		comments_from BIGINT NOT NULL,
		recorded_at DATETIME(6) NOT NULL,
		CONSTRAINT yt_issue_comment_bounds_op_id_check CHECK(op_id > 0),
		CONSTRAINT yt_issue_comment_bounds_task_id_check CHECK(task_id > 0),
		CONSTRAINT yt_issue_comment_bounds_comments_from_check CHECK(comments_from >= 0))` + mysqlTableOptions,

	// Stubs other areas own: their tables are read (never written) by this
	// area's ported paths. yt_binding_history's latest action decides the
	// unbound check of the issue enqueue; yt_feed_cursors is read by the
	// authority switch; youtrack_projects maps hk projects. The owners extend
	// with guarded ALTERs as the tasks area did for jobs.
	`CREATE TABLE IF NOT EXISTS yt_binding_history (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		task_id BIGINT NOT NULL,
		tenant VARCHAR(255) NOT NULL DEFAULT '',
		issue_id VARCHAR(64) NOT NULL DEFAULT '',
		origin VARCHAR(32) NOT NULL DEFAULT 'import_verified',
		action VARCHAR(16) NOT NULL,
		actor VARCHAR(200) NOT NULL DEFAULT '',
		reason LONGTEXT NOT NULL,
		at DATETIME(6) NOT NULL,
		KEY yt_binding_history_task(task_id, id DESC))` + mysqlTableOptions,
	`CREATE TRIGGER IF NOT EXISTS yt_binding_history_guard_update BEFORE UPDATE ON yt_binding_history FOR EACH ROW ` +
		mysqlBulkGuard(`SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_binding_history is append-only';`),
	`CREATE TRIGGER IF NOT EXISTS yt_binding_history_guard_delete BEFORE DELETE ON yt_binding_history FOR EACH ROW ` +
		mysqlBulkGuard(`SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_binding_history is append-only';`),
	`CREATE TABLE IF NOT EXISTS yt_feed_cursors (
		consumer VARCHAR(64) NOT NULL PRIMARY KEY,
		last_sync_id BIGINT NOT NULL,
		updated_at DATETIME(6) NOT NULL)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS youtrack_projects (
		hk_project VARCHAR(128) NOT NULL PRIMARY KEY,
		yt_project_id VARCHAR(64) NOT NULL DEFAULT '',
		short_name VARCHAR(64) NOT NULL DEFAULT '',
		created_at DATETIME(6) NOT NULL,
		updated_at DATETIME(6) NOT NULL)` + mysqlTableOptions,
	`INSERT INTO youtrack_projects(hk_project,short_name,created_at,updated_at) VALUES
		('handoffkeep','HK',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('panewire','PW',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('mergewake','MW',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('fleet-ops','FO',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('ParkingArcade','PA',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('auto_trader','AT',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('recallbeam','RB',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('rstack','RS',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('pacelever','PL',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6)),
		('other','OTHER',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))
		ON DUPLICATE KEY UPDATE hk_project=hk_project`,
}

// The four yt_write_ops guards. Each is mysqlBulkGuard-wrapped, like the
// append-only guards of 20261117-mysql-comments.
var mysqlYTOpsTriggers = []string{
	// handoffkeep_yt_write_ops_guard, UPDATE arm (final version of
	// 20261113-yt-comment-hold): identity columns immutable, resolved rows
	// frozen, the lifecycle transition list, the retarget (hk 4263) and
	// reissue (hk 4318) exceptions, and the unused comment hold (hk 4457).
	`CREATE TRIGGER IF NOT EXISTS yt_write_ops_guard_update BEFORE UPDATE ON yt_write_ops FOR EACH ROW ` +
		mysqlBulkGuard(
			`IF (NOT (NEW.id <=> OLD.id) OR NOT (NEW.principal <=> OLD.principal)
				OR NOT (NEW.kind <=> OLD.kind) OR NOT (NEW.idem_key <=> OLD.idem_key)
				OR (NOT (NEW.payload_sha <=> OLD.payload_sha)
					AND NOT (OLD.kind = 'comment_append' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND OLD.yt_target_id = '' AND NEW.yt_target_id <> '')
					AND NOT (OLD.kind = 'issue_create' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND NEW.yt_target_id <> ''))
				OR (NOT (NEW.payload <=> OLD.payload)
					AND NOT (OLD.kind = 'issue_create' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND NEW.yt_target_id <> ''))
				OR NOT (NEW.entity_kind <=> OLD.entity_kind) OR NOT (NEW.entity_id <=> OLD.entity_id)
				OR (NOT (NEW.yt_target_id <=> OLD.yt_target_id)
					AND NOT (OLD.kind = 'comment_append' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND OLD.yt_target_id = '' AND NEW.yt_target_id <> '')
					AND NOT (OLD.kind = 'issue_create' AND OLD.state = 'prepared' AND NEW.state = 'dispatched' AND NEW.yt_target_id <> ''))
				OR NOT (NEW.actor <=> OLD.actor)
				OR NOT (NEW.parent_id <=> OLD.parent_id) OR NOT (NEW.child_seq <=> OLD.child_seq)
				OR NOT (NEW.created_at <=> OLD.created_at)) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: the outbound payload and identity of an op are immutable';
			END IF;
			IF OLD.state IN ('applied','rejected') THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: a resolved op cannot change';
			END IF;
			IF NOT ((OLD.state = 'prepared' AND NEW.state IN ('dispatched','rejected'))
				OR (OLD.state = 'prepared' AND NEW.state = 'prepared' AND OLD.kind = 'comment_append' AND OLD.yt_target_id = '')
				OR (OLD.state = 'dispatched' AND NEW.state IN ('prepared','applied','rejected','remote_unknown'))
				OR (OLD.state = 'remote_unknown' AND NEW.state IN ('remote_unknown','applied','rejected'))) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: an op cannot make that state move';
			END IF;`),

	// The DELETE arm: yt_write_ops is append-only.
	`CREATE TRIGGER IF NOT EXISTS yt_write_ops_guard_delete BEFORE DELETE ON yt_write_ops FOR EACH ROW ` +
		mysqlBulkGuard(`SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops is append-only';`),

	// handoffkeep_yt_write_ops_tenant_guard (20261109-yt-comment-canary).
	`CREATE TRIGGER IF NOT EXISTS yt_write_ops_tenant_guard BEFORE UPDATE ON yt_write_ops FOR EACH ROW ` +
		mysqlBulkGuard(
			`IF NOT (NEW.tenant <=> OLD.tenant) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: the tenant of an op is immutable';
			END IF;`),

	// handoffkeep_yt_issue_apply_guard, final version of
	// 20261112-yt-task-map-guard: the binding row and the task map row must
	// both exist for an issue_create apply, unless reason is
	// binding_conflict.
	`CREATE TRIGGER IF NOT EXISTS yt_issue_apply_guard BEFORE UPDATE ON yt_write_ops FOR EACH ROW ` +
		mysqlBulkGuard(
			`IF NEW.kind = 'issue_create' AND NEW.state = 'applied' AND NOT (OLD.state <=> 'applied')
				AND NEW.reason <> 'binding_conflict'
				AND (NOT EXISTS (SELECT 1 FROM yt_task_bindings b
					WHERE CAST(b.task_id AS CHAR) = NEW.entity_id AND b.tenant = NEW.tenant AND b.issue_id = NEW.yt_result_id)
					OR NOT EXISTS (SELECT 1 FROM yt_entity_map m
						WHERE m.entity_kind = 'task' AND m.hk_key = NEW.entity_id AND m.yt_id = NEW.yt_result_id)) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: issue_create op applied without its binding or task map';
			END IF;`),

	// handoffkeep_yt_decision_apply_guard (20261112-yt-decision-bindings).
	`CREATE TRIGGER IF NOT EXISTS yt_decision_apply_guard BEFORE UPDATE ON yt_write_ops FOR EACH ROW ` +
		mysqlBulkGuard(
			`IF NEW.kind = 'decision_create' AND NEW.state = 'applied' AND NOT (OLD.state <=> 'applied')
				AND NEW.reason <> 'binding_conflict'
				AND NOT EXISTS (SELECT 1 FROM yt_decision_bindings b
					WHERE b.decision_id = NEW.entity_id AND b.tenant = NEW.tenant AND b.issue_id = NEW.yt_result_id) THEN
				SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='yt_write_ops: decision_create op applied without its binding';
			END IF;`),
}

func init() {
	registerMySQLMigration(mysqlMigration{id: "20261118-mysql-yt-write-ops", after: []string{"20261118-mysql-tasks"}, steps: append(mysqlYTOpsSteps, mysqlYTOpsTriggers...)})
}
