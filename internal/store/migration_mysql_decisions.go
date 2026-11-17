package store

// B6's MySQL baseline. Each table is new except chat_questions, the lane-only
// task stub from B2; that table is expanded with catalog-guarded columns.
// DDL commits one step at a time, so every step is repeatable after a crash.
// CREATE TABLE locks only the new table. The chat_questions ALTER takes a
// metadata lock on the empty B2 stub; the new decision-answer guard takes row
// locks only on writes to decision_answer_attempts.
var mysqlDecisionsSteps = []string{
	`CREATE TABLE IF NOT EXISTS dots_results (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		document_key VARCHAR(512) NOT NULL UNIQUE,
		comment_id VARCHAR(255) NOT NULL UNIQUE,
		issue_uuid VARCHAR(255) NOT NULL,
		issue_identifier VARCHAR(255) NOT NULL,
		kind VARCHAR(16) NOT NULL CHECK(kind IN ('RESULT','BLOCKED','QUESTION')),
		verdict LONGTEXT,
		artefact_sha256 CHAR(64) NOT NULL CHECK(artefact_sha256 REGEXP '^[0-9a-f]{64}$'),
		task_id BIGINT,
		routed_lane VARCHAR(255) NOT NULL,
		adoption_state VARCHAR(32) NOT NULL DEFAULT 'pending_review' CHECK(adoption_state IN ('pending_review','accepted','partially_used','rejected','superseded')),
		adopted_ref LONGTEXT, adopted_by LONGTEXT, adopted_at DATETIME(6),
		superseded_by BIGINT, answered_at DATETIME(6), answered_by LONGTEXT,
		answer_comment_id BIGINT, answer_text LONGTEXT, ask_operator BOOLEAN,
		canva_url LONGTEXT, canva_page LONGTEXT, canva_version LONGTEXT,
		created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL,
		CHECK((adoption_state='superseded')=(superseded_by IS NOT NULL)),
		KEY dots_results_task(task_id),
		CONSTRAINT dots_results_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT,
		CONSTRAINT dots_results_superseded_fk FOREIGN KEY(superseded_by) REFERENCES dots_results(id) ON DELETE RESTRICT
	)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS dots_run_tokens (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, task_id BIGINT NOT NULL,
		token CHAR(64) NOT NULL CHECK(token REGEXP '^[0-9a-f]{64}$'),
		issue_key CHAR(64) NOT NULL CHECK(issue_key REGEXP '^[0-9a-f]{64}$'),
		takeover BOOLEAN NOT NULL DEFAULT FALSE, issued_by LONGTEXT NOT NULL,
		issued_at DATETIME(6) NOT NULL, superseded_at DATETIME(6),
		UNIQUE KEY dots_run_tokens_task_issue(task_id,issue_key),
		UNIQUE KEY dots_run_tokens_active(task_id,(CASE WHEN superseded_at IS NULL THEN task_id END)),
		CONSTRAINT dots_run_tokens_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT
	)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS decision_requests (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		source VARCHAR(32) NOT NULL CHECK(source IN ('task_flag','task_request','job_escalate','lane_event','chat','session_chat','linear')),
		source_ref VARCHAR(512) NOT NULL CHECK(source_ref<>''), task_id BIGINT, job_id VARCHAR(255) NOT NULL DEFAULT '',
		lane VARCHAR(255) NOT NULL DEFAULT '', question LONGTEXT NOT NULL DEFAULT (''),
		reading LONGTEXT NOT NULL DEFAULT (''), grounds JSON NOT NULL DEFAULT (JSON_ARRAY()), options JSON,
		recommended LONGTEXT NOT NULL DEFAULT (''), default_action LONGTEXT NOT NULL DEFAULT (''),
		due_at DATETIME(6), snoozed_until DATETIME(6), expires_at DATETIME(6),
		status VARCHAR(16) NOT NULL DEFAULT 'open' CHECK(status IN ('open','answered','superseded','expired')),
		answer LONGTEXT NOT NULL DEFAULT (''), answered_by LONGTEXT NOT NULL DEFAULT (''), answered_at DATETIME(6),
		superseded_by LONGTEXT NOT NULL DEFAULT '', created_at DATETIME(6) NOT NULL, updated_at DATETIME(6) NOT NULL,
		UNIQUE KEY decision_requests_source_ref(source,source_ref),
		KEY decision_requests_task_open(task_id,status,id), KEY decision_requests_job_open(job_id,status,id),
		KEY decision_requests_status_created(status,created_at,id)
	)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS decision_answer_attempts (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, channel VARCHAR(16) NOT NULL
			CHECK(channel IN ('cli','assistant','console','session','linear','lane','default','mcp','dots')),
		event_key VARCHAR(512) NOT NULL, payload_hash CHAR(64) NOT NULL, task_id BIGINT NOT NULL,
		request_id VARCHAR(512), revision_request_id VARCHAR(512) NOT NULL, question_revision INT,
		provenance VARCHAR(32), actor LONGTEXT NOT NULL,
		result VARCHAR(32) NOT NULL CHECK(result IN ('answered','conflict','stale_revision','pending_confirmation','expired','superseded','refused','denied')),
		event_at DATETIME(6), received_at DATETIME(6) NOT NULL,
		option_id LONGTEXT, text LONGTEXT, operator_quote LONGTEXT, denies_attempt_id BIGINT, receipt LONGTEXT,
		CHECK((result='denied')=(denies_attempt_id IS NOT NULL)),
		UNIQUE KEY decision_answer_attempts_channel_event_key(channel,event_key),
		UNIQUE KEY decision_answer_attempts_one_deny(denies_attempt_id),
		KEY decision_answer_attempts_task(task_id,id),
		CONSTRAINT decision_answer_attempts_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT,
		CONSTRAINT decision_answer_attempts_denies_fk FOREIGN KEY(denies_attempt_id) REFERENCES decision_answer_attempts(id) ON DELETE RESTRICT
	)` + mysqlTableOptions,
	`CREATE TRIGGER IF NOT EXISTS decision_answer_attempts_guard_update BEFORE UPDATE ON decision_answer_attempts FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='decision_answer_attempts is append-only'; END IF; END`,
	`CREATE TRIGGER IF NOT EXISTS decision_answer_attempts_guard_delete BEFORE DELETE ON decision_answer_attempts FOR EACH ROW BEGIN IF COALESCE(@hk_bulk_load,0) <> 1 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='decision_answer_attempts is append-only'; END IF; END`,
	`CREATE TABLE IF NOT EXISTS decision_resolutions (
		resolution_id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, request_id VARCHAR(512) NOT NULL,
		question_revision INT NOT NULL, revision_request_id VARCHAR(512) NOT NULL, task_id BIGINT NOT NULL,
		kind VARCHAR(24) NOT NULL CHECK(kind IN ('answered','default_applied','withdrawn')),
		option_id LONGTEXT, text LONGTEXT, receipt LONGTEXT,
		provenance VARCHAR(32) NOT NULL CHECK(provenance IN ('operator_direct','operator_relay','assistant_judgement')),
		channel VARCHAR(32) NOT NULL, decided_by LONGTEXT NOT NULL, recorded_by LONGTEXT NOT NULL,
		attempt_id BIGINT NOT NULL, committed_at DATETIME(6) NOT NULL,
		UNIQUE KEY decision_resolutions_request_revision(request_id,question_revision),
		CONSTRAINT decision_resolutions_task_fk FOREIGN KEY(task_id) REFERENCES tasks(id) ON DELETE RESTRICT,
		CONSTRAINT decision_resolutions_attempt_fk FOREIGN KEY(attempt_id) REFERENCES decision_answer_attempts(id) ON DELETE RESTRICT
	)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS decision_deliveries (
		request_id BIGINT NOT NULL, destination VARCHAR(16) NOT NULL CHECK(destination IN ('discord')),
		message_id VARCHAR(255) NOT NULL DEFAULT '', desired_version VARCHAR(16) NOT NULL DEFAULT '',
		applied_version VARCHAR(16) NOT NULL DEFAULT '', state VARCHAR(16) NOT NULL CHECK(state IN ('pending','unsent','failed')),
		attempts INT NOT NULL DEFAULT 0, next_attempt_at DATETIME(6) NOT NULL, last_error_code LONGTEXT NOT NULL DEFAULT '',
		posted_at DATETIME(6), edited_at DATETIME(6), created_at DATETIME(6) NOT NULL,
		PRIMARY KEY(request_id,destination), KEY decision_deliveries_due(state,next_attempt_at,request_id)
	)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS decision_notify_digest (
		day DATE NOT NULL PRIMARY KEY, state VARCHAR(16) NOT NULL CHECK(state IN ('pending','sent','empty','unsent','failed')),
		listed INT NOT NULL DEFAULT 0, message_id VARCHAR(255) NOT NULL DEFAULT '', attempts INT NOT NULL DEFAULT 0,
		next_attempt_at DATETIME(6) NOT NULL, last_error LONGTEXT NOT NULL DEFAULT '', sent_at DATETIME(6), created_at DATETIME(6) NOT NULL
	)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS operator_inbox_state (
		item_key VARCHAR(192) NOT NULL PRIMARY KEY, read_at DATETIME(6), done_at DATETIME(6), snoozed_until DATETIME(6),
		actor VARCHAR(320) NOT NULL, updated_at DATETIME(6) NOT NULL
	)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS operator_inbox_events (
		event_key VARCHAR(128) NOT NULL, item_key VARCHAR(192) NOT NULL,
		action VARCHAR(16) NOT NULL CHECK(action IN ('read','unread','done','undone','snooze')),
		previous_read_at DATETIME(6), previous_done_at DATETIME(6), previous_snoozed_until DATETIME(6),
		previous_request_id BIGINT, previous_request_snoozed_until DATETIME(6),
		applied_read_at DATETIME(6), applied_done_at DATETIME(6), applied_snoozed_until DATETIME(6),
		undone_at DATETIME(6), actor VARCHAR(320) NOT NULL, at DATETIME(6) NOT NULL,
		PRIMARY KEY(event_key,item_key), KEY operator_inbox_events_item(item_key,at)
	)` + mysqlTableOptions,
	// B2's only-column stub has no data. Make its producer key textual, as in
	// PostgreSQL, then add the rest of the chat question shape. The table's
	// existing lane index is retained.
	`ALTER TABLE chat_questions CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin, MODIFY id VARCHAR(64) NOT NULL`,
	mysqlIfNoColumn("chat_questions", "conversation_id", `ALTER TABLE chat_questions
		ADD COLUMN conversation_id VARCHAR(255) NOT NULL DEFAULT 'operator-desk',
		ADD COLUMN body LONGTEXT NOT NULL DEFAULT (''),
		ADD COLUMN state VARCHAR(16) NOT NULL DEFAULT 'pending',
		ADD COLUMN created_at DATETIME(6) NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
		ADD COLUMN updated_at DATETIME(6) NOT NULL DEFAULT (UTC_TIMESTAMP(6)),
		ADD COLUMN resolved_at DATETIME(6), ADD COLUMN revision INT NOT NULL DEFAULT 1,
		ADD COLUMN answer_message_id BIGINT`),
	`CREATE TABLE IF NOT EXISTS chat_messages (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY, conversation_id VARCHAR(255) NOT NULL DEFAULT 'operator-desk',
		author VARCHAR(16) NOT NULL CHECK(author IN ('operator','desk')), body LONGTEXT NOT NULL,
		source_channel VARCHAR(64) NOT NULL DEFAULT 'legacy', origin_event_id VARCHAR(255) NOT NULL DEFAULT '',
		origin_timestamp DATETIME(6), semantic_hash CHAR(64) NOT NULL DEFAULT '',
		relay_state VARCHAR(16) NOT NULL CHECK(relay_state IN ('stored','delivered','failed','not_sent')),
		created_at DATETIME(6) NOT NULL, delivered_at DATETIME(6),
		CHECK((author='desk' AND relay_state='not_sent' AND delivered_at IS NULL) OR (author='operator' AND relay_state<>'not_sent')),
		UNIQUE KEY chat_messages_event_key(conversation_id,source_channel,(CASE WHEN origin_event_id<>'' THEN origin_event_id END)),
		KEY chat_messages_created(created_at,id), KEY chat_messages_undelivered(author,relay_state,id)
	)` + mysqlTableOptions,
	`CREATE TABLE IF NOT EXISTS chat_message_questions (
		message_id BIGINT NOT NULL, question_id VARCHAR(64) NOT NULL,
		relation_kind VARCHAR(16) NOT NULL CHECK(relation_kind IN ('posted','reply','resolve')),
		question_text LONGTEXT NOT NULL, PRIMARY KEY(message_id,question_id,relation_kind),
		KEY chat_message_questions_question(question_id,message_id),
		CONSTRAINT chat_message_questions_message_fk FOREIGN KEY(message_id) REFERENCES chat_messages(id) ON DELETE RESTRICT
	)` + mysqlTableOptions,
	mysqlIfNoIndex("chat_questions", "chat_questions_state_created", `CREATE INDEX chat_questions_state_created ON chat_questions(state,created_at,id)`),
	mysqlIfNoIndex("chat_questions", "chat_questions_created", `CREATE INDEX chat_questions_created ON chat_questions(created_at,id)`),
	mysqlIfNoConstraint("chat_questions", "chat_questions_state_check", `ALTER TABLE chat_questions ADD CONSTRAINT chat_questions_state_check CHECK(state IN ('pending','resolved','withdrawn'))`),
	mysqlIfNoConstraint("chat_questions", "chat_questions_id_check", `ALTER TABLE chat_questions ADD CONSTRAINT chat_questions_id_check CHECK(id REGEXP '^Q-[0-9]{8}-[0-9]{2,}$')`),
}

func init() {
	registerMySQLMigration(mysqlMigration{
		id: "20261118-mysql-decisions", after: []string{"20261118-mysql-tasks"},
		steps: mysqlDecisionsSteps,
	})
	registerMySQLMigration(mysqlMigration{
		id: "20261118-decision-sync-bulk-load", after: []string{"20261118-mysql-decisions"},
		steps: []string{
			`CREATE TRIGGER IF NOT EXISTS sync_decision_records_insert AFTER INSERT ON decision_requests FOR EACH ROW ` + mysqlBulkGuard(`INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'decision',CONCAT('record:',NEW.id),'I',UTC_TIMESTAMP(6));`),
			`CREATE TRIGGER IF NOT EXISTS sync_decision_records_update AFTER UPDATE ON decision_requests FOR EACH ROW ` + mysqlBulkGuard(`INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'decision',CONCAT('record:',NEW.id),'U',UTC_TIMESTAMP(6));`),
			`CREATE TRIGGER IF NOT EXISTS sync_decision_records_delete AFTER DELETE ON decision_requests FOR EACH ROW ` + mysqlBulkGuard(`INSERT INTO sync_log(txn,model,model_id,action,at) VALUES(@hk_txn,'decision',CONCAT('record:',OLD.id),'D',UTC_TIMESTAMP(6));`),
		},
	})
}
